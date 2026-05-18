// Package actions provides the global-action implementations the splash
// menu can launch. Every report-producing function comes in two forms:
//
//	WriteXxxReport(w io.Writer, actor string)   // writes to the supplied writer
//	RunXxxReport(actor string)                  // CLI convenience: writes to stdout + pauses
//
// The first form lets the in-TUI wizards.ReportViewer overlay capture the
// output into a string buffer and render it as a popup, without any of the
// program dropping back to the raw terminal.
//
// The add/remove share workflows are intended to be invoked via the
// in-TUI multi-step Bubble Tea wizards (internal/wizards/) — the simple
// CLI versions here are kept for headless invocation and for backward
// compatibility with the `taildrives status-report` style entry points.
package actions

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/archn3m3sis/taildrives/internal/journal"
	"github.com/archn3m3sis/taildrives/internal/local"
	"github.com/archn3m3sis/taildrives/internal/theme"
	"github.com/archn3m3sis/taildrives/internal/webdav"
)

// ── Mesh canonicals ────────────────────────────────────────────────────

// CoreServers / CoreWorkstations / CoreMobile are exported so the wizards
// package can present them as picker options without re-discovering them
// at runtime.
var (
	CoreServers      = []string{"n3m-srv-01", "n3m-srv-02", "n3m-srv-03", "n3m-srv-04", "n3m-srv-05"}
	CoreWorkstations = []string{"n3m-wks-01", "n3m-wks-02"}
	CoreMobile       = []string{"n3m-mob-01", "n3m-mob-02", "n3m-mob-03"}
)

// RoleOf returns a one-line description of a core device's role.
func RoleOf(host string) string {
	switch host {
	case "n3m-srv-01":
		return "UGREEN NAS (UGOS) — 11TB primary storage"
	case "n3m-srv-02":
		return "NixOS — main services host (the bulk of containers run here)"
	case "n3m-srv-03":
		return "ASUS Vivobook (NixOS) — mobile dev/edge node"
	case "n3m-srv-04":
		return "Dell rackmount — Windows Server 2025"
	case "n3m-srv-05":
		return "Dell PowerEdge R420 — Windows Server 2025"
	case "n3m-wks-01":
		return "macOS workstation (Apple Silicon) — primary operator station"
	case "n3m-wks-02":
		return "macOS workstation (Neo's Mac)"
	case "n3m-mob-01", "n3m-mob-02", "n3m-mob-03":
		return "Mobile device"
	}
	return ""
}

// ParentHostFor infers which core device a service container belongs to
// from its name. Containers named like n3m-adguard-exporter-srv01-ts have
// the parent baked in; everything else defaults to srv-02 (the main
// services host).
func ParentHostFor(sidecarName string) string {
	flat := strings.ReplaceAll(sidecarName, "-", "")
	for _, srv := range CoreServers {
		short := strings.ReplaceAll(strings.TrimPrefix(srv, "n3m-"), "-", "")
		if strings.Contains(flat, short) {
			return srv
		}
	}
	return "n3m-srv-02"
}

// DockerContainerFor maps a sidecar tailscale hostname to the docker
// container name it shadows.
func DockerContainerFor(sidecar string) string {
	name := sidecar
	if i := strings.LastIndex(name, "-ts-"); i >= 0 {
		name = name[:i]
	} else if strings.HasSuffix(name, "-ts") {
		name = strings.TrimSuffix(name, "-ts")
	}
	return name
}

// ── DEVICE STATUS REPORT ───────────────────────────────────────────────

type containerInfo struct {
	Sidecar  string
	Docker   string
	Online   bool
	LastSeen string
	Reason   string
}

// WriteDeviceStatusReport produces the full hierarchical mesh report
// against w. Safe to call from any context.
func WriteDeviceStatusReport(w io.Writer, actor string) {
	fmt.Fprintln(w, theme.Banner.Render("TAILSCALE DEVICE STATUS REPORT"))
	fmt.Fprintln(w, theme.ItemDim.Render(time.Now().Format(time.RFC1123)))
	fmt.Fprintln(w)

	peers, err := local.TailnetDevices()
	if err != nil {
		fmt.Fprintln(w, theme.Err.Render("✗ ")+err.Error())
		journal.Log(actor, "report.device_status", "", "", err)
		return
	}

	byHost := make(map[string]local.TailnetDevice, len(peers))
	for _, p := range peers {
		byHost[p.Hostname] = p
	}

	containersByHost := map[string][]containerInfo{}
	for _, p := range peers {
		if !webdav.IsSidecarDevice(p.Hostname) {
			continue
		}
		c := containerInfo{
			Sidecar:  p.Hostname,
			Docker:   DockerContainerFor(p.Hostname),
			Online:   p.Online,
			LastSeen: p.LastSeen,
		}
		parent := ParentHostFor(p.Hostname)
		containersByHost[parent] = append(containersByHost[parent], c)
	}

	// Enrich down-reasons. For LOCAL host use the local docker daemon
	// directly. For remote core-Linux hosts (srv-01 NAS, srv-03 Vivobook),
	// SSH and probe their docker. Windows hosts don't run docker — skip.
	// All probes run concurrently with a short timeout so a slow host
	// can't hold up the whole report.
	localHost := local.HostName()
	var enrichWg sync.WaitGroup
	for host, cs := range containersByHost {
		for i := range cs {
			if cs[i].Online {
				continue
			}
			enrichWg.Add(1)
			go func(h string, idx int) {
				defer enrichWg.Done()
				if h == localHost {
					cs[idx].Reason = dockerReason(cs[idx].Docker)
				} else {
					cs[idx].Reason = dockerReasonRemote(h, cs[idx].Docker)
				}
			}(host, i)
		}
		containersByHost[host] = cs
	}
	enrichWg.Wait()

	// Tally
	srvCount, srvUp := 0, 0
	wksCount, wksUp := 0, 0
	for _, h := range CoreServers {
		for _, c := range containersByHost[h] {
			srvCount++
			if c.Online {
				srvUp++
			}
		}
	}
	for _, h := range CoreWorkstations {
		for _, c := range containersByHost[h] {
			wksCount++
			if c.Online {
				wksUp++
			}
		}
	}

	mesh := summarizeMesh(byHost)
	fmt.Fprintln(w, theme.Title.Render("MESH SUMMARY"))
	fmt.Fprintf(w, "  Core servers      : %s\n", coreStatusLine(CoreServers, byHost))
	fmt.Fprintf(w, "  Core workstations : %s\n", coreStatusLine(CoreWorkstations, byHost))
	fmt.Fprintf(w, "  Core mobile       : %s\n", coreStatusLine(CoreMobile, byHost))
	fmt.Fprintf(w, "  Server containers : %s active · %s down (across all 5 core servers)\n",
		theme.OK.Render(fmt.Sprintf("%d", srvUp)),
		theme.Err.Render(fmt.Sprintf("%d", srvCount-srvUp)))
	fmt.Fprintf(w, "  Workstation ctnrs : %s active · %s down\n",
		theme.OK.Render(fmt.Sprintf("%d", wksUp)),
		theme.Err.Render(fmt.Sprintf("%d", wksCount-wksUp)))
	fmt.Fprintf(w, "  Tailnet total     : %d devices (%d real hosts + %d service sidecars)\n",
		mesh.Total, mesh.RealHosts, mesh.Sidecars)
	fmt.Fprintln(w)

	renderCoreSection(w, "CORE SERVERS", CoreServers, byHost, containersByHost)
	renderCoreSection(w, "CORE WORKSTATIONS", CoreWorkstations, byHost, containersByHost)
	renderCoreSection(w, "CORE MOBILE", CoreMobile, byHost, containersByHost)

	journal.Log(actor, "report.device_status", "tailnet",
		fmt.Sprintf("%d srv ctnrs (%d up), %d wks ctnrs (%d up), %d sidecars total",
			srvCount, srvUp, wksCount, wksUp, mesh.Sidecars), nil)
}

// RunDeviceStatusReport is the CLI wrapper.
func RunDeviceStatusReport(actor string) {
	defer pause()
	WriteDeviceStatusReport(os.Stdout, actor)
}

type meshSummary struct{ Total, RealHosts, Sidecars int }

func summarizeMesh(byHost map[string]local.TailnetDevice) meshSummary {
	var s meshSummary
	for h := range byHost {
		s.Total++
		if webdav.IsSidecarDevice(h) {
			s.Sidecars++
		} else {
			s.RealHosts++
		}
	}
	return s
}

func coreStatusLine(hosts []string, byHost map[string]local.TailnetDevice) string {
	var parts []string
	for _, h := range hosts {
		short := strings.TrimPrefix(h, "n3m-")
		dev, present := byHost[h]
		switch {
		case !present:
			parts = append(parts, theme.ItemDim.Render(short+":missing"))
		case dev.Online:
			parts = append(parts, theme.OK.Render(short))
		default:
			parts = append(parts, theme.Err.Render(short+":off"))
		}
	}
	return strings.Join(parts, " ")
}

func renderCoreSection(w io.Writer, title string, hosts []string, byHost map[string]local.TailnetDevice, containersByHost map[string][]containerInfo) {
	fmt.Fprintln(w, theme.Banner.Render("─── "+title+" ───"))
	for _, h := range hosts {
		dev, present := byHost[h]
		header := "● " + theme.AccentHiS.Render(h)
		switch {
		case !present:
			header += theme.ItemDim.Render("  (not in tailnet)")
		case !dev.Online:
			header += "  " + theme.Err.Render("OFFLINE") + theme.ItemDim.Render(" — last seen "+dev.LastSeen)
		default:
			header += "  " + theme.OK.Render("online") + theme.ItemDim.Render("  "+dev.OS)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, header)
		if r := RoleOf(h); r != "" {
			fmt.Fprintln(w, "  "+theme.ItemDim.Render(r))
		}

		cs := containersByHost[h]
		if len(cs) == 0 {
			fmt.Fprintln(w, "  "+theme.ItemDim.Render("Containers: (none registered with tailscale)"))
			continue
		}
		up, down := 0, 0
		for _, c := range cs {
			if c.Online {
				up++
			} else {
				down++
			}
		}
		fmt.Fprintf(w, "  Containers: %s active · %s down · %d total\n",
			theme.OK.Render(fmt.Sprintf("%d", up)),
			theme.Err.Render(fmt.Sprintf("%d", down)),
			len(cs))

		if down > 0 {
			fmt.Fprintln(w, "  "+theme.Err.Render("Down:"))
			for _, c := range cs {
				if c.Online {
					continue
				}
				reason := c.Reason
				if reason == "" {
					reason = "offline " + c.LastSeen + " (only the host this report runs on enriches down-reason)"
				}
				fmt.Fprintf(w, "    %s %s\n",
					theme.Err.Render("✗"),
					theme.AccentHiS.Render(c.Docker))
				fmt.Fprintf(w, "       %s\n", theme.ItemDim.Render(reason))
			}
		}
		if up > 0 {
			fmt.Fprintln(w, "  "+theme.OK.Render("Active (sample):"))
			sort.Slice(cs, func(i, j int) bool { return cs[i].Docker < cs[j].Docker })
			shown := 0
			for _, c := range cs {
				if !c.Online {
					continue
				}
				if shown >= 10 {
					fmt.Fprintf(w, "    %s\n", theme.ItemDim.Render(fmt.Sprintf("… %d more", up-shown)))
					break
				}
				fmt.Fprintf(w, "    %s %s\n",
					theme.OK.Render("✓"),
					theme.Item.Render(c.Docker))
				shown++
			}
		}
	}
}

// dockerReason probes the local docker daemon for a container's status +
// last log lines. Returns "" if docker isn't available locally.
func dockerReason(name string) string {
	docker := dockerBin()
	if docker == "" {
		return ""
	}
	out, _ := exec.Command(docker, "ps", "-a",
		"--filter", "name=^"+name+"$",
		"--format", "{{.Status}}").CombinedOutput()
	status := strings.TrimSpace(string(out))
	if status == "" {
		return "no container named " + name + " on this host"
	}
	out, _ = exec.Command(docker, "logs", "--tail", "5", name).CombinedOutput()
	logs := strings.TrimSpace(string(out))
	logs = strings.ReplaceAll(logs, "\n", " | ")
	if len(logs) > 200 {
		logs = logs[:197] + "…"
	}
	if logs == "" {
		return status
	}
	return status + " · " + logs
}

// dockerReasonRemote SSHes to `host` and probes docker there. Used so the
// status report can surface down-reasons for containers on other servers.
// Returns "" if SSH or docker is unavailable on the remote.
func dockerReasonRemote(host, name string) string {
	user := "archn3m3sis"
	if strings.HasPrefix(host, "n3m-srv-04") || strings.HasPrefix(host, "n3m-srv-05") {
		// Windows hosts likely don't run docker — bail.
		return ""
	}
	// Compose a single shell pipe: status + last 5 log lines.
	remote := fmt.Sprintf(
		"sudo docker ps -a --filter 'name=^%s$' --format '{{.Status}}' && echo '---LOGS---' && sudo docker logs --tail 5 %s 2>&1",
		name, name)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=4",
		user+"@"+host, remote).CombinedOutput()
	if err != nil {
		return ""
	}
	parts := strings.SplitN(string(out), "---LOGS---", 2)
	status := strings.TrimSpace(parts[0])
	if status == "" {
		return ""
	}
	logs := ""
	if len(parts) > 1 {
		logs = strings.TrimSpace(parts[1])
		logs = strings.ReplaceAll(logs, "\n", " | ")
		if len(logs) > 200 {
			logs = logs[:197] + "…"
		}
	}
	if logs == "" {
		return status
	}
	return status + " · " + logs
}

func dockerBin() string {
	for _, p := range []string{"/run/current-system/sw/bin/docker", "/usr/bin/docker", "/usr/local/bin/docker"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p
	}
	return ""
}

// ── PERFORMANCE REPORT ─────────────────────────────────────────────────

// Progress is a single tick fed to the optional progress callback during
// the performance report. Total may grow as targets are discovered.
type Progress struct {
	Done, Total int
	Stage       string // e.g. "netcheck", "pinging core", "pinging containers"
}

type pingResult struct {
	Host    string
	IP      string
	Path    string
	IsDERP  bool
	IsLocal bool
	Samples []float64
	Loss    float64
	Err     string
}

func (r pingResult) Avg() float64 {
	if len(r.Samples) == 0 {
		return 0
	}
	var s float64
	for _, v := range r.Samples {
		s += v
	}
	return s / float64(len(r.Samples))
}
func (r pingResult) Jitter() float64 {
	if len(r.Samples) < 2 {
		return 0
	}
	a := r.Avg()
	var sq float64
	for _, v := range r.Samples {
		d := v - a
		sq += d * d
	}
	return math.Sqrt(sq / float64(len(r.Samples)-1))
}
func (r pingResult) Min() float64 {
	if len(r.Samples) == 0 {
		return 0
	}
	m := r.Samples[0]
	for _, v := range r.Samples[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
func (r pingResult) Max() float64 {
	if len(r.Samples) == 0 {
		return 0
	}
	m := r.Samples[0]
	for _, v := range r.Samples[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

type netcheckResult struct {
	Raw, DERP, DERPLat string
	IPv4, IPv6, UDP    bool
}

func runNetcheck(bin string) netcheckResult {
	r := netcheckResult{}
	out, _ := exec.Command(bin, "netcheck").CombinedOutput()
	r.Raw = string(out)
	for _, ln := range strings.Split(r.Raw, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "* Nearest DERP:"):
			r.DERP = strings.TrimSpace(strings.TrimPrefix(ln, "* Nearest DERP:"))
		case strings.HasPrefix(ln, "* IPv4:"):
			r.IPv4 = strings.Contains(ln, "yes")
		case strings.HasPrefix(ln, "* IPv6:"):
			r.IPv6 = strings.Contains(ln, "yes")
		case strings.HasPrefix(ln, "* UDP:"):
			r.UDP = strings.Contains(ln, "true")
		}
	}
	return r
}

func pingTarget(bin, host string, count int) pingResult {
	r := pingResult{Host: host}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "ping",
		"--c", fmt.Sprintf("%d", count),
		"--until-direct=false",
		"--timeout", "1s",
		host)
	out, _ := cmd.CombinedOutput()
	pongRE := regexp.MustCompile(`pong from \S+ \(([\d.:a-fA-F]+)\) via (.+?) in ([\d.]+)ms`)
	for _, ln := range strings.Split(string(out), "\n") {
		m := pongRE.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		r.IP = m[1]
		r.Path = strings.TrimSpace(m[2])
		if v, err := strconv.ParseFloat(m[3], 64); err == nil {
			r.Samples = append(r.Samples, v)
		}
	}
	if len(r.Samples) == 0 {
		r.Path = "OFFLINE"
		r.Err = "no pong (timed out or unreachable)"
		r.Loss = 1.0
	} else {
		r.Loss = float64(count-len(r.Samples)) / float64(count)
		if strings.HasPrefix(r.Path, "DERP") {
			r.IsDERP = true
		}
	}
	return r
}

// WritePerformanceReport produces the full mesh latency report. The
// optional progress callback (may be nil) is fired as ping work
// completes so an overlay can render a live progress bar.
func WritePerformanceReport(w io.Writer, actor string, progress func(Progress)) {
	fmt.Fprintln(w, theme.Banner.Render("TAILSCALE PERFORMANCE REPORT"))
	fmt.Fprintln(w, theme.ItemDim.Render(time.Now().Format(time.RFC1123)))
	fmt.Fprintln(w)
	bin := tsBin()
	if bin == "" {
		fmt.Fprintln(w, theme.Err.Render("✗ tailscale CLI not found"))
		return
	}
	if progress != nil {
		progress(Progress{Stage: "netcheck"})
	}
	nc := runNetcheck(bin)

	peers, _ := local.TailnetDevices()
	byHost := map[string]local.TailnetDevice{}
	for _, p := range peers {
		byHost[p.Hostname] = p
	}
	all := append([]string{}, CoreServers...)
	all = append(all, CoreWorkstations...)
	all = append(all, CoreMobile...)
	containers := pickContainerSample(peers, 30)

	type t struct{ name, kind string }
	targets := make([]t, 0, len(all)+len(containers))
	for _, h := range all {
		targets = append(targets, t{h, "core"})
	}
	for _, h := range containers {
		targets = append(targets, t{h, "container"})
	}

	if progress != nil {
		progress(Progress{Done: 0, Total: len(targets), Stage: "pinging mesh"})
	}
	results := make([]pingResult, len(targets))
	var mu sync.Mutex
	var done int
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	localHostName := local.HostName()
	for i, tg := range targets {
		if tg.name == localHostName {
			results[i] = pingResult{Host: tg.name, Path: "(this host)", IsLocal: true, Samples: []float64{0}}
			done++
			if progress != nil {
				progress(Progress{Done: done, Total: len(targets), Stage: "pinging mesh"})
			}
			continue
		}
		if dev, ok := byHost[tg.name]; ok && !dev.Online {
			results[i] = pingResult{Host: tg.name, Path: "OFFLINE",
				Err: "device offline (last seen " + dev.LastSeen + ")", Loss: 1.0}
			done++
			if progress != nil {
				progress(Progress{Done: done, Total: len(targets), Stage: "pinging mesh"})
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, name string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = pingTarget(bin, name, 5)
			mu.Lock()
			done++
			d := done
			mu.Unlock()
			if progress != nil {
				progress(Progress{Done: d, Total: len(targets), Stage: "pinging mesh"})
			}
		}(i, tg.name)
	}
	wg.Wait()

	// Split results
	var core, ctrs []pingResult
	for i, tg := range targets {
		if tg.kind == "core" {
			core = append(core, results[i])
		} else {
			ctrs = append(ctrs, results[i])
		}
	}

	fmt.Fprintln(w, theme.Title.Render("LOCAL NETCHECK"))
	fmt.Fprintf(w, "  Nearest DERP region : %s\n", theme.AccentHiS.Render(coalesce(nc.DERP, "?")))
	fmt.Fprintf(w, "  IPv4 / IPv6         : %s / %s\n", yesno(nc.IPv4), yesno(nc.IPv6))
	fmt.Fprintf(w, "  UDP supported       : %s\n", yesno(nc.UDP))
	fmt.Fprintln(w)
	renderLatencyTable(w, "CORE MESH LATENCY", core)
	fmt.Fprintln(w)
	renderContainerSummary(w, ctrs, len(peers))
	fmt.Fprintln(w)
	renderHealthAnalysis(w, core, ctrs, nc)

	journal.Log(actor, "report.performance", "tailnet",
		fmt.Sprintf("core: %d/%d reachable; sampled containers: %d/%d reachable",
			countReachable(core), len(core), countReachable(ctrs), len(ctrs)), nil)
}

// RunPerformanceReport is the CLI wrapper with a stderr spinner.
func RunPerformanceReport(actor string) {
	defer pause()
	WritePerformanceReport(os.Stdout, actor, func(p Progress) {
		fmt.Fprintf(os.Stderr, "\r  %s: %d/%d ", p.Stage, p.Done, p.Total)
	})
	fmt.Fprintln(os.Stderr)
}

func pickContainerSample(peers []local.TailnetDevice, n int) []string {
	var all []string
	for _, p := range peers {
		if webdav.IsSidecarDevice(p.Hostname) && p.Online {
			all = append(all, p.Hostname)
		}
	}
	if len(all) <= n {
		return all
	}
	step := len(all) / n
	if step < 1 {
		step = 1
	}
	var out []string
	for i := 0; i < len(all) && len(out) < n; i += step {
		out = append(out, all[i])
	}
	return out
}

func renderLatencyTable(w io.Writer, title string, results []pingResult) {
	fmt.Fprintln(w, theme.Title.Render("═══ "+title+" ═══"))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-22s  %-22s  %8s  %10s  %6s  %s\n",
		theme.ItemDim.Render("Device"),
		theme.ItemDim.Render("Path"),
		theme.ItemDim.Render("RTT avg"),
		theme.ItemDim.Render("Jitter"),
		theme.ItemDim.Render("Loss"),
		theme.ItemDim.Render("Notes"))
	for _, r := range results {
		var path, avg, jit, loss, notes string
		switch {
		case r.IsLocal:
			path = theme.ItemDim.Render("(this host)")
			notes = theme.ItemDim.Render("can't tailscale-ping self")
		case r.Path == "OFFLINE":
			path = theme.Err.Render("OFFLINE")
			notes = theme.ItemDim.Render(r.Err)
		case r.IsDERP:
			path = theme.Warn.Render(truncatePath(r.Path, 22))
			avg = fmt.Sprintf("%.1f ms", r.Avg())
			jit = fmt.Sprintf("±%.1f ms", r.Jitter())
			loss = fmt.Sprintf("%.0f%%", r.Loss*100)
			notes = theme.Warn.Render("DERP fallback")
		default:
			path = theme.OK.Render(truncatePath(r.Path, 22))
			avg = fmt.Sprintf("%.1f ms", r.Avg())
			jit = fmt.Sprintf("±%.1f ms", r.Jitter())
			loss = fmt.Sprintf("%.0f%%", r.Loss*100)
			switch {
			case r.Avg() < 2:
				notes = theme.OK.Render("excellent")
			case r.Avg() < 10:
				notes = theme.Info.Render("good")
			case r.Avg() < 50:
				notes = theme.Info.Render("LAN/regional")
			default:
				notes = theme.Warn.Render("WAN/relayed")
			}
		}
		fmt.Fprintf(w, "  %-22s  %-22s  %8s  %10s  %6s  %s\n",
			theme.AccentHiS.Render(r.Host), path, avg, jit, loss, notes)
	}
}

func renderContainerSummary(w io.Writer, results []pingResult, totalPeers int) {
	fmt.Fprintln(w, theme.Title.Render("═══ SERVICE CONTAINER LATENCY (sampled) ═══"))
	fmt.Fprintln(w)
	var reachable []pingResult
	derp := 0
	for _, r := range results {
		if r.Path != "OFFLINE" {
			reachable = append(reachable, r)
		}
		if r.IsDERP {
			derp++
		}
	}
	if len(reachable) == 0 {
		fmt.Fprintln(w, "  "+theme.Err.Render("no containers reachable"))
		return
	}
	avg, jit, min, max := aggregate(reachable)
	p50, p95 := percentiles(reachable)
	fmt.Fprintf(w, "  Sampled        : %s of %d containers in tailnet\n",
		theme.Info.Render(fmt.Sprintf("%d", len(results))), totalPeers)
	fmt.Fprintf(w, "  Reachable      : %s\n",
		theme.OK.Render(fmt.Sprintf("%d (%.0f%%)", len(reachable),
			100*float64(len(reachable))/float64(len(results)))))
	fmt.Fprintf(w, "  RTT mean       : %s\n", theme.Item.Render(fmt.Sprintf("%.2f ms", avg)))
	fmt.Fprintf(w, "  RTT min / max  : %s / %s\n",
		theme.OK.Render(fmt.Sprintf("%.2f ms", min)),
		theme.Warn.Render(fmt.Sprintf("%.2f ms", max)))
	fmt.Fprintf(w, "  RTT p50 / p95  : %s / %s\n",
		theme.Item.Render(fmt.Sprintf("%.2f ms", p50)),
		theme.Item.Render(fmt.Sprintf("%.2f ms", p95)))
	fmt.Fprintf(w, "  Mean jitter    : %s\n", theme.Item.Render(fmt.Sprintf("±%.2f ms", jit)))
	fmt.Fprintf(w, "  DERP-routed    : %s of sample\n",
		theme.Warn.Render(fmt.Sprintf("%d", derp)))
}

func renderHealthAnalysis(w io.Writer, core, containers []pingResult, nc netcheckResult) {
	fmt.Fprintln(w, theme.Title.Render("═══ MESH HEALTH ANALYSIS ═══"))
	fmt.Fprintln(w)
	var issues []string
	for _, r := range core {
		if r.Path == "OFFLINE" {
			issues = append(issues, theme.Warn.Render("⚠ ")+r.Host+" unreachable — "+r.Err)
		}
	}
	for _, r := range core {
		if r.IsDERP {
			issues = append(issues, theme.Warn.Render("⚠ ")+r.Host+" on DERP fallback (direct path failed — NAT/firewall?)")
		}
	}
	for _, r := range core {
		if r.Jitter() > 5 && !r.IsDERP && r.Path != "OFFLINE" {
			issues = append(issues, theme.Warn.Render("⚠ ")+r.Host+
				fmt.Sprintf(" jitter ±%.1fms (>5ms threshold)", r.Jitter()))
		}
	}
	if !nc.UDP {
		issues = append(issues, theme.Err.Render("✗ ")+"UDP not supported on this network — Tailscale will fall back to DERP for ALL connections")
	}
	if len(issues) == 0 {
		fmt.Fprintln(w, "  "+theme.OK.Render("✓ All systems nominal — every core device reachable on direct path, jitter within bounds"))
	} else {
		for _, i := range issues {
			fmt.Fprintln(w, "  "+i)
		}
	}
	fmt.Fprintln(w)
	var best, worst pingResult
	first := true
	for _, r := range core {
		if r.Path == "OFFLINE" || r.IsLocal {
			continue
		}
		if first {
			best, worst = r, r
			first = false
			continue
		}
		if r.Avg() < best.Avg() {
			best = r
		}
		if r.Avg() > worst.Avg() {
			worst = r
		}
	}
	if !first {
		fmt.Fprintf(w, "  Best link  : %s (%.1f ms via %s)\n",
			theme.OK.Render(best.Host), best.Avg(), best.Path)
		fmt.Fprintf(w, "  Worst link : %s (%.1f ms via %s)\n",
			theme.Warn.Render(worst.Host), worst.Avg(), worst.Path)
	}
}

func aggregate(rs []pingResult) (avg, jit, min, max float64) {
	if len(rs) == 0 {
		return
	}
	min = math.MaxFloat64
	var sa, sj float64
	for _, r := range rs {
		sa += r.Avg()
		sj += r.Jitter()
		if r.Min() < min {
			min = r.Min()
		}
		if r.Max() > max {
			max = r.Max()
		}
	}
	return sa / float64(len(rs)), sj / float64(len(rs)), min, max
}

func percentiles(rs []pingResult) (p50, p95 float64) {
	if len(rs) == 0 {
		return
	}
	a := make([]float64, len(rs))
	for i, r := range rs {
		a[i] = r.Avg()
	}
	sort.Float64s(a)
	p50 = a[len(a)/2]
	i95 := int(0.95 * float64(len(a)))
	if i95 >= len(a) {
		i95 = len(a) - 1
	}
	p95 = a[i95]
	return
}

func countReachable(rs []pingResult) int {
	n := 0
	for _, r := range rs {
		if r.Path != "OFFLINE" {
			n++
		}
	}
	return n
}

func coalesce(s, f string) string {
	if s == "" {
		return f
	}
	return s
}

func yesno(b bool) string {
	if b {
		return theme.OK.Render("yes")
	}
	return theme.Err.Render("no")
}

func truncatePath(p string, w int) string {
	if len(p) <= w {
		return p
	}
	return p[:w-1] + "…"
}

// ── JOURNAL VIEWER ─────────────────────────────────────────────────────

// WriteJournalReport renders the last `days` of journal entries (capped
// at `max`) to w.
func WriteJournalReport(w io.Writer, days, max int) {
	fmt.Fprintln(w, theme.Banner.Render("TAILDRIVES LIFECYCLE JOURNAL"))
	fmt.Fprintln(w, theme.ItemDim.Render(fmt.Sprintf("last %d days · newest first · capped at %d entries", days, max)))
	fmt.Fprintln(w)
	entries, err := journal.ReadRecent(days, max)
	if err != nil {
		fmt.Fprintln(w, theme.Err.Render("✗ ")+err.Error())
		return
	}
	if len(entries) == 0 {
		fmt.Fprintln(w, theme.ItemDim.Render("  (no journal entries yet — actions you take will appear here)"))
		return
	}
	curDay := ""
	for _, e := range entries {
		day := e.Timestamp.Local().Format("Mon Jan 2, 2006")
		if day != curDay {
			fmt.Fprintln(w)
			fmt.Fprintln(w, theme.TitleActive.Render("─── "+day+" ───"))
			curDay = day
		}
		t := e.Timestamp.Local().Format("15:04:05")
		ok := theme.OK.Render("✓")
		if !e.Success {
			ok = theme.Err.Render("✗")
		}
		fmt.Fprintf(w, "  %s %s  %s  %s  %s\n",
			ok,
			theme.ItemDim.Render(t),
			theme.AccentHiS.Render(e.Action),
			theme.Item.Render(e.Target),
			theme.ItemDim.Render(e.Detail))
		if e.Error != "" {
			fmt.Fprintln(w, "       "+theme.Err.Render("error: "+e.Error))
		}
	}
}

func RunJournalViewer(actor string) {
	defer pause()
	WriteJournalReport(os.Stdout, 7, 100)
	_ = actor
}

// ── SHARE WIZARDS (CLI fallback — the real flow runs in internal/wizards/) ──

func RunRemoveShares(actor string) { wizardNotice("REMOVE") }
func RunAddShares(actor string)    { wizardNotice("ADD") }

func wizardNotice(kind string) {
	defer pause()
	fmt.Println(theme.Banner.Render(kind + " TAILDRIVE SHARES"))
	fmt.Println()
	fmt.Println(theme.ItemDim.Render("This wizard now runs as an in-TUI overlay — launch via splash menu Tab → Global Actions."))
}

// ── share manipulation primitives — used by the wizard package ────────

// SharesOn returns the shares currently published by `device`. For the
// LOCAL host, queries `tailscale drive list` (or the prefs JSON fallback)
// directly — Tailscale Drive's WebDAV hides own-host shares from the
// host itself, so PROPFIND returns empty for `localhost`. For remote
// devices, PROPFINDs the WebDAV root.
func SharesOn(device string) ([]string, error) {
	if device == local.HostName() {
		shares, err := local.ListShares()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(shares))
		for _, s := range shares {
			out = append(out, s.Name)
		}
		sort.Strings(out)
		return out, nil
	}
	dav := webdav.New("")
	ns, err := dav.AutoUserNamespace()
	if err != nil {
		return nil, err
	}
	entries, err := dav.WithTimeout(5 * time.Second).List("/" + ns + "/" + device)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// UnshareOn runs `tailscale drive unshare <name>` on `device` — locally
// when device is the current host, via SSH otherwise.
func UnshareOn(device, name string) error {
	if device == local.HostName() {
		return local.Unshare(name)
	}
	cmd := sshCmd(device, "tailscale drive unshare "+name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// ShareOn runs `tailscale drive share <name> <path>` on `device`.
func ShareOn(device, name, path string) error {
	if device == local.HostName() {
		bin := tsBin()
		out, err := exec.Command(bin, "drive", "share", name, path).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	cmd := sshCmd(device, fmt.Sprintf("tailscale drive share %s %q", name, path))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func sshCmd(device, remote string) *exec.Cmd {
	user := "archn3m3sis"
	if strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05") {
		user = "Administrator"
	}
	target := user + "@" + device
	if !strings.HasPrefix(device, "n3m-srv-04") &&
		!strings.HasPrefix(device, "n3m-srv-05") &&
		!strings.HasPrefix(device, "n3m-wks-") {
		remote = "sudo " + remote
	}
	// accept-new mirrors ~/.ssh/config defaults — auto-trust first-seen
	// hosts so the share-add/share-remove flow doesn't bomb out on a
	// freshly-installed mesh box. A CHANGED key still fails (MITM-safe).
	return exec.Command("ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		target, remote)
}

func tsBin() string {
	// macOS-only: prefer the Homebrew standalone CLI over /usr/local/bin/tailscale.
	// The latter is a shim into /Applications/Tailscale.app/.../tailscale, which
	// is built without the `drive` subcommand. The Homebrew formula (installed
	// via `brew install tailscale`) lands at /opt/homebrew/bin and DOES include
	// drive — and it talks to the same tailscaled socket the App uses.
	for _, p := range []string{
		"/run/current-system/sw/bin/tailscale",
		"/usr/bin/tailscale",
		"/opt/homebrew/bin/tailscale", // macOS: must come before /usr/local/bin shim
		"/usr/local/bin/tailscale",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	return ""
}

// ── shared CLI helpers ────────────────────────────────────────────────

func pause() {
	fmt.Println()
	fmt.Println(theme.ItemDim.Render("press Enter to return to the menu…"))
	bufio.NewReader(os.Stdin).ReadString('\n')
}
