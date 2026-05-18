// Package netmap implements the TAILNET ENV MAPPING overlay — the most
// data-rich part of the taildrives TUI. Maps the operator's network from
// WAN edge down to per-device containers + docker networks, with ASCII
// connection-line drawing tying the tiers together.
//
// Data sources, in order of acquisition:
//
//	1. External IP + ISP / ASN  — curl ipinfo.io (or fallback ipify)
//	2. Default gateway + local IP — `ip route` parsing
//	3. Router (AmpliFi) device list — local LAN scan + AmpliFi /api fallback
//	4. Tailnet device map           — `tailscale status --json`
//	5. Per-device hostname + MAC    — ARP table (`ip neigh` / `arp -a`)
//	6. Docker containers + networks — `docker ps --format` + `docker network ls`
//	7. Firewall summary             — `iptables -L` / `nft list ruleset` head
//
// Each tier loads ASYNC via tea.Cmds so the overlay paints progressively
// — operator sees the WAN tier within ~500ms, full LAN tier within ~3s,
// docker / firewall details as they arrive.
package netmap

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/exportkit"
	"github.com/archn3m3sis/taildrives/internal/overlay"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

// Overlay re-exports the shared Overlay contract.
type Overlay = overlay.Overlay

// ── Data model ─────────────────────────────────────────────────────────

// WANInfo is the external-internet tier — what the operator's connection
// looks like from the ISP's side.
type WANInfo struct {
	ExternalIP string // public IP as seen by the internet
	ISP        string // org name from ipinfo (e.g. "Comcast Cable")
	ASN        string // autonomous system number (e.g. "AS7922")
	Country    string
	City       string
	Region     string
	Loaded     bool
	Err        error
}

// LANInfo describes the operator's local network — the router IP, this
// host's LAN IP + interface, and the default gateway.
type LANInfo struct {
	LocalIP     string
	Interface   string
	GatewayIP   string
	SubnetCIDR  string
	Loaded      bool
	Err         error
}

// PeerDevice is one device on the LAN (or tailnet — overlapping set).
// Combines what we learn from the ARP table and the tailscale daemon.
type PeerDevice struct {
	LANIP        string
	TailscaleIP  string
	Hostname     string
	MAC          string
	OS           string
	Online       bool   // per tailscale status (offline doesn't mean unreachable on LAN)
	IsLocal      bool   // this device itself
	IsRouter     bool   // gateway
	IsSidecar    bool   // tailscale-container sidecar (not a real host)
	Containers   []string // running container names on tailnet (for this host)
	DockerNets   []string // docker network names visible (local host only)
	LastSeen     string
}

// DataMap is the entire collected snapshot — everything the overlay
// renders comes out of this struct.
type DataMap struct {
	WAN       WANInfo
	LAN       LANInfo
	Devices   []PeerDevice
	Firewall  []string // first N lines of iptables-L output for the local host
}

// ── Async load Cmds — each tier is its own Cmd so they paint progressively

type wanLoadedMsg WANInfo
type lanLoadedMsg LANInfo
type peersLoadedMsg struct{ peers []PeerDevice }
type firewallLoadedMsg struct{ lines []string }

// loadWANCmd queries ipinfo.io (free tier, no auth) for the operator's
// external IP + ISP / ASN. Falls back to ipify if ipinfo is down.
func loadWANCmd() tea.Cmd {
	return func() tea.Msg {
		w := WANInfo{}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://ipinfo.io/json", nil)
		req.Header.Set("User-Agent", "taildrives/0.13 netmap")
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var body struct {
				IP, Hostname, City, Region, Country, Org string
			}
			if json.NewDecoder(resp.Body).Decode(&body) == nil {
				w.ExternalIP = body.IP
				w.City = body.City
				w.Region = body.Region
				w.Country = body.Country
				if body.Org != "" {
					// Org is typically "AS7922 Comcast Cable Communications"
					parts := strings.SplitN(body.Org, " ", 2)
					if len(parts) == 2 && strings.HasPrefix(parts[0], "AS") {
						w.ASN = parts[0]
						w.ISP = parts[1]
					} else {
						w.ISP = body.Org
					}
				}
				w.Loaded = true
				return wanLoadedMsg(w)
			}
		}
		// Fallback: ipify for the IP only — ISP info won't be available.
		req2, _ := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org", nil)
		resp2, err2 := client.Do(req2)
		if err2 != nil {
			w.Err = err
			return wanLoadedMsg(w)
		}
		defer resp2.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp2.Body.Read(buf)
		w.ExternalIP = strings.TrimSpace(string(buf[:n]))
		w.ISP = "(unknown — ipinfo unreachable)"
		w.Loaded = true
		return wanLoadedMsg(w)
	}
}

// loadLANCmd reads local route table + interface IPs to figure out this
// host's LAN IP, the default gateway, and the active interface name.
func loadLANCmd() tea.Cmd {
	return func() tea.Msg {
		l := LANInfo{}
		// Default gateway via `ip route show default` (Linux/macOS).
		out, err := exec.Command("ip", "route", "show", "default").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			// "default via 192.168.140.1 dev eno1 proto dhcp ..."
			fields := strings.Fields(line)
			for i, f := range fields {
				if f == "via" && i+1 < len(fields) {
					l.GatewayIP = fields[i+1]
				}
				if f == "dev" && i+1 < len(fields) {
					l.Interface = fields[i+1]
				}
			}
		} else {
			// macOS fallback
			out, _ := exec.Command("netstat", "-rn", "-f", "inet").Output()
			for _, line := range strings.Split(string(out), "\n") {
				f := strings.Fields(line)
				if len(f) >= 4 && f[0] == "default" {
					l.GatewayIP = f[1]
					l.Interface = f[3]
					break
				}
			}
		}
		// Local IP on the gateway-facing interface.
		if l.Interface != "" {
			ifc, err := net.InterfaceByName(l.Interface)
			if err == nil {
				addrs, _ := ifc.Addrs()
				for _, a := range addrs {
					ipNet, ok := a.(*net.IPNet)
					if !ok || ipNet.IP.To4() == nil {
						continue
					}
					l.LocalIP = ipNet.IP.String()
					l.SubnetCIDR = ipNet.String()
					break
				}
			}
		}
		l.Loaded = true
		return lanLoadedMsg(l)
	}
}

// loadPeersCmd builds the peer device list by joining `tailscale status
// --json` (authoritative for tailnet membership, hostname, OS, online)
// with the ARP table (authoritative for LAN IP ↔ MAC) where they overlap
// by hostname. Sidecar (tagged) devices are flagged so the UI can render
// them differently from real hosts.
func loadPeersCmd() tea.Cmd {
	return func() tea.Msg {
		peers := []PeerDevice{}

		// Tailscale status --json
		bin := tsBin()
		if bin == "" {
			return peersLoadedMsg{peers: peers}
		}
		out, err := exec.Command(bin, "status", "--json").Output()
		if err != nil {
			return peersLoadedMsg{peers: peers}
		}
		var st struct {
			Self struct {
				HostName    string
				TailscaleIPs []string
				Online      bool
				OS          string
				Tags        []string
			}
			Peer map[string]struct {
				HostName     string
				TailscaleIPs []string
				Online       bool
				OS           string
				LastSeen     string
				Tags         []string
			}
		}
		if err := json.Unmarshal(out, &st); err != nil {
			return peersLoadedMsg{peers: peers}
		}

		// Self entry
		selfIP := ""
		if len(st.Self.TailscaleIPs) > 0 {
			selfIP = st.Self.TailscaleIPs[0]
		}
		peers = append(peers, PeerDevice{
			Hostname:    st.Self.HostName,
			TailscaleIP: selfIP,
			Online:      true,
			OS:          st.Self.OS,
			IsLocal:     true,
			IsSidecar:   isTaggedSidecar(st.Self.Tags),
		})

		// Peers
		for _, p := range st.Peer {
			ip := ""
			if len(p.TailscaleIPs) > 0 {
				ip = p.TailscaleIPs[0]
			}
			peers = append(peers, PeerDevice{
				Hostname:    p.HostName,
				TailscaleIP: ip,
				Online:      p.Online,
				OS:          p.OS,
				IsSidecar:   isTaggedSidecar(p.Tags),
				LastSeen:    p.LastSeen,
			})
		}

		// ARP table — populate LANIP + MAC where hostname matches.
		// Output format varies; `ip neigh` on Linux gives:
		//   192.168.140.10 dev eno1 lladdr 00:11:22:33:44:55 REACHABLE
		arp, _ := exec.Command("ip", "neigh").Output()
		ipByMAC := map[string]string{}
		macByIP := map[string]string{}
		for _, line := range strings.Split(string(arp), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 {
				continue
			}
			ip := f[0]
			mac := ""
			for i, t := range f {
				if t == "lladdr" && i+1 < len(f) {
					mac = f[i+1]
				}
			}
			if mac != "" {
				ipByMAC[mac] = ip
				macByIP[ip] = mac
			}
		}
		// We don't have a hostname↔LANIP cross-reference reliably; use the
		// known n3m hosts mapping from /etc/hosts if present.
		hostsLANIP := parseEtcHosts()
		for i, p := range peers {
			if lan, ok := hostsLANIP[p.Hostname]; ok {
				peers[i].LANIP = lan
				if mac, ok := macByIP[lan]; ok {
					peers[i].MAC = mac
				}
			}
		}

		// For the LOCAL device, gather docker containers + networks too.
		for i, p := range peers {
			if !p.IsLocal {
				continue
			}
			peers[i].Containers = dockerContainersOnTailnet()
			peers[i].DockerNets = dockerNetworks()
		}

		return peersLoadedMsg{peers: peers}
	}
}

// loadFirewallCmd reads iptables/nft summary for the LOCAL host. Just the
// first ~20 chains-or-rules so the operator can spot egress restrictions
// without scrolling forever.
func loadFirewallCmd() tea.Cmd {
	return func() tea.Msg {
		out, err := exec.Command("iptables", "-L", "-n").Output()
		if err != nil || len(out) == 0 {
			out, _ = exec.Command("nft", "list", "ruleset").Output()
		}
		lines := strings.Split(string(out), "\n")
		if len(lines) > 25 {
			lines = lines[:25]
		}
		return firewallLoadedMsg{lines: lines}
	}
}

// ── Helpers ────────────────────────────────────────────────────────────

func tsBin() string {
	for _, p := range []string{
		"/run/current-system/sw/bin/tailscale",
		"/usr/bin/tailscale",
		"/opt/homebrew/bin/tailscale",
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

func isTaggedSidecar(tags []string) bool {
	for _, t := range tags {
		if t == "tag:server" || t == "tag:container" || strings.HasPrefix(t, "tag:") {
			return true
		}
	}
	return false
}

func parseEtcHosts() map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile("/etc/hosts")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		ip := f[0]
		// We want LAN IPs (192.168.x.x or 10.x.x.x or 172.16-31.x.x)
		if !isPrivateLAN(ip) {
			continue
		}
		for _, name := range f[1:] {
			// Don't overwrite — first match wins; gives us hostname → ip
			if _, ok := out[name]; !ok {
				out[name] = ip
			}
		}
	}
	return out
}

func isPrivateLAN(ip string) bool {
	switch {
	case strings.HasPrefix(ip, "192.168."):
		return true
	case strings.HasPrefix(ip, "10."):
		return true
	case strings.HasPrefix(ip, "172."):
		// 172.16.0.0/12
		var a, b int
		fmt.Sscanf(ip, "172.%d.%d", &a, &b)
		return a >= 16 && a <= 31
	}
	return false
}

func dockerContainersOnTailnet() []string {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		// Heuristic: tailscale-attached containers in this mesh follow
		// the n3m-* naming convention OR have -ts suffix.
		if ln == "" {
			continue
		}
		if strings.HasPrefix(ln, "n3m-") {
			names = append(names, ln)
		}
	}
	return names
}

func dockerNetworks() []string {
	out, err := exec.Command("docker", "network", "ls", "--format", "{{.Name}}").Output()
	if err != nil {
		return nil
	}
	var nets []string
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || ln == "bridge" || ln == "host" || ln == "none" {
			continue
		}
		nets = append(nets, ln)
	}
	return nets
}

// ── Overlay model ──────────────────────────────────────────────────────

// Model is the netmap overlay state. Holds the DataMap as it accretes
// from the various async loads, the scroll position, the selected
// device, the print-notification deadline, the Obsidian-export picker
// state, and the post-export confirmation state.
type Model struct {
	data        DataMap
	scroll      int
	selectedDev int // index into data.Devices for the right-pane focus
	wanLoading, lanLoading, peersLoading, fwLoading bool

	// Print + copy feedback (mirrors the wizard report overlays).
	printNotifyAt time.Time

	// Obsidian vault picker
	vaultPickerOpen bool
	vaultScanning   bool
	vaults          []string
	vaultIdx        int

	// Post-export confirmation
	exportPath     string
	exportNotifyAt time.Time
	exportErr      error

	// Cached width/height from the last View call — used by the popup
	// composer to center correctly without threading w/h through every
	// helper.
	lastW, lastH int
}

func New() *Model {
	return &Model{
		wanLoading:   true,
		lanLoading:   true,
		peersLoading: true,
		fwLoading:    true,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(loadWANCmd(), loadLANCmd(), loadPeersCmd(), loadFirewallCmd())
}

func (m *Model) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	// Async msgs that don't take key handling.
	switch v := msg.(type) {
	case wanLoadedMsg:
		m.data.WAN = WANInfo(v)
		m.wanLoading = false
		return m, nil, false
	case lanLoadedMsg:
		m.data.LAN = LANInfo(v)
		m.lanLoading = false
		return m, nil, false
	case peersLoadedMsg:
		m.data.Devices = v.peers
		m.peersLoading = false
		return m, nil, false
	case firewallLoadedMsg:
		m.data.Firewall = v.lines
		m.fwLoading = false
		return m, nil, false
	case exportkit.PrintDismissMsg:
		m.printNotifyAt = time.Time{}
		return m, nil, false
	case vaultsScannedMsg:
		m.vaultScanning = false
		m.vaults = v.vaults
		return m, nil, false
	case exportDoneMsg:
		m.exportPath = v.path
		m.exportErr = v.err
		m.exportNotifyAt = time.Now().Add(8 * time.Second)
		return m, tea.Tick(8*time.Second, func(time.Time) tea.Msg {
			return exportDismissMsg{}
		}), false
	case exportDismissMsg:
		m.exportNotifyAt = time.Time{}
		m.exportPath = ""
		m.exportErr = nil
		return m, nil, false
	}

	// Key handling — when vault picker is open, route keys to it.
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil, false
	}

	if m.vaultPickerOpen {
		switch km.String() {
		case "esc":
			m.vaultPickerOpen = false
			return m, nil, false
		case "up", "k":
			if m.vaultIdx > 0 {
				m.vaultIdx--
			}
			return m, nil, false
		case "down", "j":
			if m.vaultIdx < len(m.vaults)-1 {
				m.vaultIdx++
			}
			return m, nil, false
		case "enter":
			if len(m.vaults) == 0 {
				return m, nil, false
			}
			vault := m.vaults[m.vaultIdx]
			content := generateMermaid(m.data, hostname())
			m.vaultPickerOpen = false
			return m, writeExportToVaultCmd(vault, content), false
		}
		return m, nil, false
	}

	switch km.String() {
	case "esc", "q":
		return m, nil, true
	case "ctrl+c":
		// Copy the topology + summary content to clipboard.
		return m, exportkit.CopyToClipboardCmd(m.copyableContent()), false
	case "ctrl+p":
		m.printNotifyAt = time.Now().Add(5 * time.Second)
		return m, tea.Batch(
			exportkit.PrintReportCmd(m.copyableContent(), "TAILNET ENV MAPPING"),
			exportkit.PrintDismissCmd(),
		), false
	case "o", "O":
		// Open Obsidian vault picker + kick off async scan.
		m.vaultPickerOpen = true
		m.vaultScanning = true
		m.vaultIdx = 0
		m.vaults = nil
		return m, scanForVaultsCmd(), false
	case "up", "k":
		if m.scroll > 0 {
			m.scroll--
		}
	case "down", "j":
		m.scroll++
	case "pgup", "ctrl+u":
		m.scroll -= 10
		if m.scroll < 0 {
			m.scroll = 0
		}
	case "pgdown", " ":
		m.scroll += 10
	case "home", "g":
		m.scroll = 0
	case "end", "G":
		m.scroll = 1 << 30
	case "left", "h":
		if m.selectedDev > 0 {
			m.selectedDev--
		}
	case "right", "l":
		if m.selectedDev < len(m.data.Devices)-1 {
			m.selectedDev++
		}
	case "r":
		m.wanLoading, m.lanLoading, m.peersLoading, m.fwLoading = true, true, true, true
		return m, tea.Batch(loadWANCmd(), loadLANCmd(), loadPeersCmd(), loadFirewallCmd()), false
	}
	return m, nil, false
}

// copyableContent returns a plain-text representation of the entire
// netmap state for clipboard / print export. Uses the rendered diagram
// (ANSI codes will be stripped by exportkit) plus the structured
// inventory below.
func (m *Model) copyableContent() string {
	var b strings.Builder
	b.WriteString("TAILNET ENV MAPPING\n")
	b.WriteString("===================\n\n")
	b.WriteString(m.renderTopologyGrid(m.lastW))
	b.WriteString("\n\n--- DEVICE INVENTORY ---\n\n")
	for _, d := range m.data.Devices {
		if d.IsSidecar {
			continue
		}
		fmt.Fprintf(&b, "%s   role=%s   LAN=%s   TS=%s   MAC=%s   OS=%s   online=%v\n",
			d.Hostname, roleOf(d.Hostname),
			fallback(d.LANIP, "—"), fallback(d.TailscaleIP, "—"),
			fallback(d.MAC, "—"), fallback(d.OS, "—"), d.Online)
	}
	return b.String()
}

// hostname returns this machine's tailscale hostname (best-effort).
// Used by the Obsidian export to record where the snapshot came from.
func hostname() string {
	out, err := exec.Command(tsBin(), "status", "--self", "--peers=false").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && len(f[0]) > 0 && f[0][0] >= '0' && f[0][0] <= '9' {
				return f[1]
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

func (m *Model) View(w, h int) string {
	bw := w - 6
	if bw < 100 {
		bw = 100
	}
	if bw > 200 {
		bw = 200
	}
	bh := h - 4
	if bh < 30 {
		bh = 30
	}
	if bh > 70 {
		bh = 70
	}
	innerW := bw - 4

	titleBar := lipgloss.NewStyle().
		Foreground(theme.Magenta).Bold(true).Reverse(true).Padding(0, 2).
		Render(" 󰛳  TAILNET ENV MAPPING ")

	// Build the topology diagram as a 2D character grid — every box,
	// every connection line is placed at explicit (x,y) coords. The
	// grid is then composited with the per-device focus card + the
	// firewall strip below.
	topology := m.renderTopologyGrid(innerW)
	focusCard := m.renderFocusCard(innerW)
	containers := m.renderContainersStrip(innerW)
	firewall := m.renderFirewallStrip(innerW)

	body := lipgloss.JoinVertical(lipgloss.Left,
		topology,
		"",
		focusCard,
		"",
		firewall,
	)
	_ = containers // containers + docker nets now rendered INSIDE the
	// focus card when the selected device has them attached; the
	// standalone strip is gone so the layout stays compact.

	bodyLines := strings.Split(body, "\n")
	contentH := bh - 5
	maxScroll := len(bodyLines) - contentH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	end := m.scroll + contentH
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[m.scroll:end]

	scrollInfo := ""
	if maxScroll > 0 {
		scrollInfo = "  " + theme.ItemDim.Render(
			fmt.Sprintf("%d-%d / %d", m.scroll+1, end, len(bodyLines)))
	}
	hint := theme.ItemDim.Render(
		"  ←→/hl pick device · ↑↓/jk scroll · r refresh ·  Ctrl+C copy ·  Ctrl+P print · 󰈙 o Obsidian export · Esc/q close")

	final := lipgloss.JoinVertical(lipgloss.Left,
		titleBar+scrollInfo,
		"",
		strings.Join(visible, "\n"),
		"",
		hint,
	)
	rendered := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Magenta).
		Padding(1, 2).
		Width(bw).
		Render(final)

	// Stash for popups that need full pane dimensions.
	m.lastW, m.lastH = w, h

	// Overlay popups (last → drawn on top).
	if m.vaultPickerOpen {
		picker := renderVaultPicker(m.vaultScanning, m.vaults, m.vaultIdx, w)
		return exportkit.OverlayCenter(rendered, picker, w, h)
	}
	if !m.exportNotifyAt.IsZero() && time.Now().Before(m.exportNotifyAt) {
		remaining := int(time.Until(m.exportNotifyAt).Seconds()) + 1
		var popup string
		if m.exportErr != nil {
			popup = renderExportError(m.exportErr, remaining, w)
		} else {
			popup = renderExportNotification(m.exportPath, remaining, w)
		}
		return exportkit.OverlayCenter(rendered, popup, w, h)
	}
	if !m.printNotifyAt.IsZero() && time.Now().Before(m.printNotifyAt) {
		remaining := int(time.Until(m.printNotifyAt).Seconds()) + 1
		popup := exportkit.RenderPrintNotification(remaining, w)
		return exportkit.OverlayCenter(rendered, popup, w, h)
	}
	return rendered
}

// renderExportError is the red-tinged variant of renderExportNotification
// used when the file write failed. Same shape, different message + color.
func renderExportError(err error, secondsLeft, w int) string {
	bw := 84
	if w-12 < bw {
		bw = w - 12
	}
	if bw < 60 {
		bw = 60
	}
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#dc2626")).Bold(true).Reverse(true).
		Padding(0, 2).
		Render("  ✗  EXPORT FAILED ")
	body := lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		"  "+lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("Could not write the topology to the selected vault:"),
		"",
		"  "+lipgloss.NewStyle().Foreground(lipgloss.Color("#fca5a5")).
			Render(err.Error()),
		"",
		"  "+lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("Verify write permissions on the vault directory and retry."),
		"",
		lipgloss.NewStyle().Foreground(theme.ItemDim.GetForeground()).Italic(true).
			Render(fmt.Sprintf("  Auto-dismiss in %ds…", secondsLeft)),
		"",
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("#dc2626")).
		Background(lipgloss.Color("#1f0a0a")).
		Padding(0, 1).
		Width(bw).
		Render(body)
}

// ── Topology diagram ──────────────────────────────────────────────────

// renderTopologyGrid is the centerpiece — paints a real ASCII network
// map onto a Grid: INTERNET cloud at top, modem below, router below
// that, then GROUPED device bands (SERVERS / NAS / WORKSTATIONS / MOBILE
// / PERIPHERALS) each with its own colored title strip + box row(s).
// Router drops to a horizontal trunk; trunk drops to each group; group
// has its own internal bus to its devices. Operator can read role
// boundaries at a glance.
func (m *Model) renderTopologyGrid(w int) string {
	// Layout constants
	const (
		topPad     = 1
		tierGap    = 2
		boxW       = 24
		boxH       = 8
		boxHGap    = 2 // horizontal gap between device boxes within a group
		groupGap   = 4 // extra padding around each group's bands
		netBoxH    = 5 // WAN / modem / router box height
		bandTitleH = 1 // group title bar (centered chip) height
	)

	// Filter sidecars + group hosts by role. Ordering: SERVERS first,
	// then NAS, then WORKSTATIONS, then MOBILE, then ENT/PRINTER/BMC.
	// `group` is defined at package scope (see bottom of file) so
	// drawBand can take it as a parameter.
	groups := []*group{
		{label: "SERVERS",      role: "server",        color: lipgloss.Color("#0891b2")},
		{label: "NAS",          role: "nas",           color: lipgloss.Color("#f59e0b")},
		{label: "WORKSTATIONS", role: "workstation",   color: lipgloss.Color("#3b82f6")},
		{label: "MOBILE",       role: "mobile",        color: lipgloss.Color("#a855f7")},
		{label: "PERIPHERALS",  role: "other",         color: lipgloss.Color("#737373")},
	}
	for _, d := range m.data.Devices {
		if d.IsSidecar {
			continue
		}
		r := roleOf(d.Hostname)
		placed := false
		for _, gr := range groups {
			if gr.role == r {
				gr.devices = append(gr.devices, d)
				placed = true
				break
			}
		}
		if !placed {
			// Anything that didn't map to a primary role bucket lands in
			// PERIPHERALS — printers, BMCs, entertainment, unknown.
			groups[len(groups)-1].devices = append(groups[len(groups)-1].devices, d)
		}
	}
	// Drop empty groups so the topology doesn't render dead bands.
	nonEmpty := groups[:0]
	for _, gr := range groups {
		if len(gr.devices) > 0 {
			// Sort within group: local device first, then alphabetical.
			sort.SliceStable(gr.devices, func(i, j int) bool {
				if gr.devices[i].IsLocal != gr.devices[j].IsLocal {
					return gr.devices[i].IsLocal
				}
				return gr.devices[i].Hostname < gr.devices[j].Hostname
			})
			nonEmpty = append(nonEmpty, gr)
		}
	}
	groups = nonEmpty
	if len(groups) == 0 {
		groups = []*group{{label: "DEVICES", color: lipgloss.Color("#737373"),
			devices: []PeerDevice{{Hostname: "(no devices)", IsLocal: true}}}}
	}

	// Compute per-group row layout — how many boxes fit per row in each
	// group's allotted width? For simplicity, each group gets the full
	// pane width to wrap within. (Side-by-side group layout would
	// require complex packing; vertical bands are clearer.)
	stride := boxW + boxHGap
	perRow := (w - boxHGap) / stride
	if perRow < 1 {
		perRow = 1
	}

	// Total grid height: WAN + modem + router + trunk drop + sum of
	// per-group heights (each = bandTitle + boxRows * (boxH + tierGap)).
	gridH := topPad +
		netBoxH + tierGap +
		netBoxH + tierGap +
		netBoxH + tierGap +
		2 // trunk drop from router to first group
	for _, gr := range groups {
		boxRows := (len(gr.devices) + perRow - 1) / perRow
		gridH += bandTitleH + 1 + boxRows*(boxH+tierGap) + groupGap
	}
	g := NewGrid(w, gridH)

	// ── Tier 1: INTERNET cloud ──
	wanW := 56
	if wanW > w-4 {
		wanW = w - 4
	}
	wanX := (w - wanW) / 2
	wanY := topPad
	g.Box(wanX, wanY, wanW, netBoxH, lipgloss.Color("#f59e0b"), "󰖟  INTERNET / WAN")
	wan := m.data.WAN
	if m.wanLoading {
		g.SetStr(wanX+2, wanY+2, "loading external IP + ISP…", lipgloss.Color("#525252"))
	} else if wan.Err != nil {
		g.SetStr(wanX+2, wanY+2, "✗ "+wan.Err.Error(), lipgloss.Color("#dc2626"))
	} else {
		g.SetStr(wanX+2, wanY+1,
			"IP "+wan.ExternalIP+"  ASN "+wan.ASN, lipgloss.Color("#fbbf24"))
		ispLine := wan.ISP
		if wan.City != "" {
			ispLine += "   " + wan.City + ", " + wan.Region
		}
		if len(ispLine) > wanW-4 {
			ispLine = ispLine[:wanW-5] + "…"
		}
		g.SetStr(wanX+2, wanY+2, ispLine, lipgloss.Color("#fde68a"))
		g.SetStr(wanX+2, wanY+3,
			theme_dim("public internet"), lipgloss.Color("#525252"))
	}

	// Vertical connector WAN → MODEM
	centerX := w / 2
	wanBottom := wanY + netBoxH
	modemY := wanBottom + tierGap
	g.VLine(centerX, wanBottom, modemY-1, lipgloss.Color("#525252"))

	// ── Tier 2: MODEM ──
	modemW := 44
	if modemW > w-4 {
		modemW = w - 4
	}
	modemX := (w - modemW) / 2
	g.Box(modemX, modemY, modemW, netBoxH, lipgloss.Color("#a16207"), "󰽏  ISP MODEM")
	g.SetStr(modemX+2, modemY+1,
		"L1/L2 bridge from ISP to local network",
		lipgloss.Color("#fde68a"))
	g.SetStr(modemX+2, modemY+2,
		"(no SNMP/HTTP probe — transparent device)",
		lipgloss.Color("#525252"))
	g.SetStr(modemX+2, modemY+3,
		"WAN ──→ MODEM ──→ ROUTER",
		lipgloss.Color("#525252"))

	// Vertical connector MODEM → ROUTER
	modemBottom := modemY + netBoxH
	routerY := modemBottom + tierGap
	g.VLine(centerX, modemBottom, routerY-1, lipgloss.Color("#525252"))

	// ── Tier 3: ROUTER / GATEWAY ──
	routerW := 56
	if routerW > w-4 {
		routerW = w - 4
	}
	routerX := (w - routerW) / 2
	g.Box(routerX, routerY, routerW, netBoxH, lipgloss.Color("#0891b2"), "󰑩  ROUTER / GATEWAY")
	lan := m.data.LAN
	if m.lanLoading {
		g.SetStr(routerX+2, routerY+2, "loading gateway info…", lipgloss.Color("#525252"))
	} else {
		g.SetStr(routerX+2, routerY+1,
			"Gateway "+lan.GatewayIP+"   Subnet "+lan.SubnetCIDR,
			lipgloss.Color("#22d3ee"))
		g.SetStr(routerX+2, routerY+2,
			fmt.Sprintf("This host: %s on %s", lan.LocalIP, lan.Interface),
			lipgloss.Color("#67e8f9"))
		// Tally LAN-resident hosts across all (non-sidecar) peers.
		lanCount := 0
		for _, d := range m.data.Devices {
			if !d.IsSidecar && d.LANIP != "" {
				lanCount++
			}
		}
		g.SetStr(routerX+2, routerY+3,
			fmt.Sprintf("%d devices in tailnet · %d on LAN",
				len(m.data.Devices), lanCount),
			lipgloss.Color("#525252"))
	}

	// ── Tier 4: Grouped device bands ──
	// Layout strategy: SERVERS gets its own full-width tier (usually 5+
	// devices). The remaining groups (NAS, WORKSTATIONS, MOBILE,
	// PERIPHERALS) pack SIDE-BY-SIDE in ONE horizontal tier below, each
	// in its own width-allotment column. Kills the never-ending-scroll
	// feeling — vertical extent is bounded to ~2 device-tier heights.
	routerBottom := routerY + netBoxH
	trunkTopY := routerBottom + 1
	g.VLine(centerX, routerBottom, trunkTopY, lipgloss.Color("#525252"))

	// Partition groups: SERVERS gets its own row; everything else is
	// "compact" and shares one row.
	var serverGroup *group
	var compactGroups []*group
	for _, gr := range groups {
		if gr.role == "server" {
			serverGroup = gr
		} else {
			compactGroups = append(compactGroups, gr)
		}
	}

	// Track global ←→ cursor index. Order: SERVERS first, then compact
	// groups left-to-right in the order they appear in `compactGroups`.
	globalIdx := 0
	yCursor := trunkTopY + 1

	// ── Tier 4a: SERVERS — full-width band ──
	if serverGroup != nil {
		bandTopY := yCursor
		drawBand(g, 0, bandTopY, w, perRow, serverGroup, centerX, &globalIdx,
			m.selectedDev, true)
		// Compute band height to advance yCursor.
		boxRows := (len(serverGroup.devices) + perRow - 1) / perRow
		bandHeight := bandTitleH + 1 + boxRows*(boxH+tierGap) + 1
		// Drop trunk from router into the band (above the title strip).
		g.VLine(centerX, trunkTopY, bandTopY-1, lipgloss.Color("#525252"))
		yCursor = bandTopY + bandHeight + groupGap
		// Trunk continues down to feed the compact-groups tier.
		if len(compactGroups) > 0 {
			g.VLine(centerX, bandTopY+bandHeight, yCursor,
				lipgloss.Color("#525252"))
		}
	}

	// ── Tier 4b: NAS / WORKSTATIONS / MOBILE / PERIPHERALS side-by-side ──
	if len(compactGroups) > 0 {
		tierTopY := yCursor
		// Compute per-group width allotment. Each compact group gets an
		// equal slice of pane width. Within its slice, boxes wrap.
		colW := w / len(compactGroups)
		// How many boxes fit per row IN THIS COLUMN WIDTH?
		colPerRow := (colW - boxHGap) / stride
		if colPerRow < 1 {
			colPerRow = 1
		}
		// Tallest compact group sets the tier's vertical extent.
		tallestRows := 1
		for _, gr := range compactGroups {
			r := (len(gr.devices) + colPerRow - 1) / colPerRow
			if r > tallestRows {
				tallestRows = r
			}
		}
		tierHeight := bandTitleH + 1 + tallestRows*(boxH+tierGap) + 1

		// Horizontal trunk-fanout: vertical from router/SERVERS-trunk
		// drops to this tier, then a horizontal bus spans the centers of
		// all the column-bands. Each column-band hangs off that horizontal bus.
		fanoutY := tierTopY - 1
		// Compute each column's center x-coord.
		var colCenters []int
		for i := range compactGroups {
			cx := i*colW + colW/2
			colCenters = append(colCenters, cx)
		}
		// Horizontal fanout bus (in muted trunk color)
		g.HLine(colCenters[0], colCenters[len(colCenters)-1], fanoutY,
			lipgloss.Color("#525252"))
		// Junction where the central trunk meets the fanout
		// Find where centerX falls relative to bus endpoints
		junctionLeft := centerX > colCenters[0]
		junctionRight := centerX < colCenters[len(colCenters)-1]
		g.Junction(centerX, fanoutY, true, false, junctionLeft, junctionRight,
			lipgloss.Color("#525252"))

		// Now draw each compact group in its own column-zone.
		for i, gr := range compactGroups {
			colX := i * colW
			cx := colCenters[i]
			// Vertical drop from fanout bus to this column's title row
			g.VLine(cx, fanoutY+1, tierTopY-1, lipgloss.Color("#525252"))
			// Junction at fanout where column drops off
			isLeftEnd := i == 0
			isRightEnd := i == len(compactGroups)-1
			g.Junction(cx, fanoutY,
				false, true, !isLeftEnd, !isRightEnd,
				lipgloss.Color("#525252"))
			drawBand(g, colX, tierTopY, colW, colPerRow, gr, cx, &globalIdx,
				m.selectedDev, false)
		}
		yCursor = tierTopY + tierHeight
		_ = tierHeight
	}

	return g.Render()
}

// drawBand paints one group band into the grid at (x,y) within `width`
// horizontal space. Lays out devices in rows of `perRow`. Returns
// nothing — mutates the global cursor index via the pointer for ←→
// navigation continuity across all bands.
func drawBand(g *Grid, x, y, width, perRow int, gr *group,
	trunkCx int, globalIdx *int, selectedIdx int, isFullWidth bool) {
	const (
		boxW    = 24
		boxH    = 8
		boxHGap = 2
		tierGap = 2
	)
	stride := boxW + boxHGap

	// Title strip — centered chip in the band's accent color.
	titleText := fmt.Sprintf("━━ %s (%d) ━━", gr.label, len(gr.devices))
	if isFullWidth {
		titleText = fmt.Sprintf("━━━━━━  %s  (%d)  ━━━━━━", gr.label, len(gr.devices))
	}
	titleX := x + (width-len(titleText))/2
	g.SetStrStyled(titleX, y, titleText,
		lipgloss.Color("#000000"), gr.color, true)

	// Box rows inside this band.
	rowStartY := y + 2 // 1 line gap below title
	boxRows := (len(gr.devices) + perRow - 1) / perRow
	for rowIdx := 0; rowIdx < boxRows; rowIdx++ {
		startI := rowIdx * perRow
		endI := startI + perRow
		if endI > len(gr.devices) {
			endI = len(gr.devices)
		}
		rowCount := endI - startI
		rowWidth := rowCount*boxW + (rowCount-1)*boxHGap
		// Center this row within the band's column allotment.
		rowStartX := x + (width-rowWidth)/2
		rowY := rowStartY + rowIdx*(boxH+tierGap)
		busLineY := rowY - 1
		firstCenter := rowStartX + boxW/2
		lastCenter := rowStartX + (rowCount-1)*stride + boxW/2

		// Group-internal bus (in band's accent color)
		g.HLine(firstCenter, lastCenter, busLineY, gr.color)

		if rowIdx == 0 {
			// Connect band's title-area down to the bus.
			leftOfCenter := trunkCx > firstCenter
			rightOfCenter := trunkCx < lastCenter
			g.Junction(trunkCx, busLineY, true, false,
				leftOfCenter, rightOfCenter, gr.color)
		}

		// Per-device boxes
		for i := 0; i < rowCount; i++ {
			d := gr.devices[startI+i]
			bx := rowStartX + i*stride
			by := rowY
			cx := bx + boxW/2

			g.VLine(cx, busLineY+1, by-1, gr.color)
			isLeftEnd := i == 0
			isRightEnd := i == rowCount-1
			g.Junction(cx, busLineY,
				false, true, !isLeftEnd, !isRightEnd, gr.color)

			drawDeviceBox(g, bx, by, boxW, boxH, d,
				selectedIdx == *globalIdx)
			*globalIdx++
		}
	}
}

// group is the per-role bucket used by the topology renderer. Exported
// only via drawBand's parameter so the renderer can split servers from
// the side-by-side compact tier.
type group struct {
	label   string
	role    string
	color   lipgloss.Color
	devices []PeerDevice
}

// drawDeviceBox paints one host's tile in the device row. Color-coded by
// role; the selected (←→) device gets a brighter border + bg-tinted top
// row so the operator's cursor is obvious in the diagram.
func drawDeviceBox(g *Grid, x, y, w, h int, d PeerDevice, selected bool) {
	role := roleOf(d.Hostname)
	border := roleColor(role, d.Online, d.IsLocal)
	if selected {
		border = lipgloss.Color("#eab308") // yellow-500 — cursor highlight
	}
	icon := roleIcon(role)
	title := icon + " " + d.Hostname
	if len(title) > w-4 {
		title = title[:w-5] + "…"
	}
	g.Box(x, y, w, h, border, title)

	// Inner content rows — fit 5 lines inside an 8-row box (top + 5 + bottom)
	textFG := lipgloss.Color("#e5e7eb")
	dimFG := lipgloss.Color("#737373")
	if !d.Online {
		textFG = dimFG
	}

	row := func(line int, label, val string, valFG lipgloss.Color) {
		s := fmt.Sprintf("%-4s%s", label, val)
		if len(s) > w-2 {
			s = s[:w-3] + "…"
		}
		// Print label dim, value styled.
		g.SetStr(x+1, y+line, label, dimFG)
		g.SetStr(x+1+len(label), y+line, " "+val, valFG)
	}

	row(1, "LAN", fallback(d.LANIP, "—"), textFG)
	row(2, "TS",  fallback(d.TailscaleIP, "—"), lipgloss.Color("#22d3ee"))
	row(3, "MAC", truncateMAC(d.MAC), dimFG)
	if d.OS != "" {
		row(4, "OS", d.OS, textFG)
	}

	// Status row (bottom-most interior line). The LOCAL device gets a
	// completely different treatment from online/offline peers — bright
	// magenta bg with bold black text + a "★ THIS DEVICE" label —
	// because it's THE most important orientation cue in the entire
	// topology (operator needs to know which box is them at a glance).
	switch {
	case d.IsLocal:
		// Magenta highlight strip spanning the row's interior cells.
		g.FillBg(x+1, y+h-2, w-2, 1, lipgloss.Color("#ec4899")) // pink-500
		// Bold black text on top of the strip.
		label := "★ THIS DEVICE"
		// Center the label within the strip.
		pad := (w - 2 - len(label)) / 2
		if pad < 0 {
			pad = 0
		}
		fullLabel := strings.Repeat(" ", pad) + label
		if len(fullLabel) > w-2 {
			fullLabel = fullLabel[:w-2]
		}
		// Pad to the right edge so the bold attr covers the full strip.
		fullLabel = fmt.Sprintf("%-*s", w-2, fullLabel)
		g.SetStrStyled(x+1, y+h-2, fullLabel,
			lipgloss.Color("#000000"),
			lipgloss.Color("#ec4899"),
			true)
		// Also bold the title bar for extra emphasis — bold doesn't
		// change the char but bumps weight on terminals that support it.
		titleLine := y
		for cx := x; cx < x+w; cx++ {
			c := g.cells[titleLine][cx]
			c.bold = true
			g.cells[titleLine][cx] = c
		}
	case !d.Online:
		g.SetStr(x+1, y+h-2,
			fmt.Sprintf("%-*s", w-2, "○ offline"),
			lipgloss.Color("#525252"))
	default:
		g.SetStr(x+1, y+h-2,
			fmt.Sprintf("%-*s", w-2, "● online"),
			lipgloss.Color("#22c55e"))
	}
}

// ── Tier role + colors ────────────────────────────────────────────────

func roleOf(hostname string) string {
	switch {
	case hostname == "n3m-srv-01":
		return "nas"
	case strings.HasPrefix(hostname, "n3m-srv-"):
		return "server"
	case strings.HasPrefix(hostname, "n3m-nas-"):
		return "nas"
	case strings.HasPrefix(hostname, "n3m-wks-"):
		return "workstation"
	case strings.HasPrefix(hostname, "n3m-mob-"):
		return "mobile"
	case strings.HasPrefix(hostname, "n3m-ent-"):
		return "entertainment"
	case strings.HasPrefix(hostname, "n3m-prt-"):
		return "printer"
	case strings.HasPrefix(hostname, "n3m-idr-"):
		return "bmc"
	}
	return "device"
}

func roleColor(role string, online, isLocal bool) lipgloss.Color {
	if !online {
		return lipgloss.Color("#404040")
	}
	if isLocal {
		return lipgloss.Color("#22c55e") // green-500
	}
	switch role {
	case "server":
		return lipgloss.Color("#0891b2") // cyan-600
	case "nas":
		return lipgloss.Color("#f59e0b") // amber-500
	case "workstation":
		return lipgloss.Color("#3b82f6") // blue-500
	case "mobile":
		return lipgloss.Color("#a855f7") // purple-500
	case "entertainment":
		return lipgloss.Color("#ec4899") // pink-500
	case "printer":
		return lipgloss.Color("#737373")
	case "bmc":
		return lipgloss.Color("#dc2626") // red-600 — management plane
	}
	return lipgloss.Color("#737373")
}

func roleIcon(role string) string {
	switch role {
	case "server":
		return ""
	case "nas":
		return ""
	case "workstation":
		return ""
	case "mobile":
		return ""
	case "entertainment":
		return ""
	case "printer":
		return ""
	case "bmc":
		return ""
	}
	return ""
}

// ── Focus card, containers chip-row, firewall strip ────────────────

func (m *Model) renderFocusCard(w int) string {
	if len(m.data.Devices) == 0 {
		return ""
	}
	if m.selectedDev >= len(m.data.Devices) {
		m.selectedDev = 0
	}
	// Skip sidecars when navigating — match the topology filter.
	var hosts []PeerDevice
	for _, d := range m.data.Devices {
		if !d.IsSidecar {
			hosts = append(hosts, d)
		}
	}
	if m.selectedDev >= len(hosts) {
		m.selectedDev = 0
	}
	d := hosts[m.selectedDev]
	role := roleOf(d.Hostname)
	border := roleColor(role, d.Online, d.IsLocal)

	title := lipgloss.NewStyle().Foreground(border).Bold(true).
		Render("  " + roleIcon(role) + "  DEVICE FOCUS — " + d.Hostname +
			"   (←→ to navigate)")

	rows := []string{
		"  " + dotLeader("Role", lipgloss.NewStyle().Foreground(border).Bold(true).
			Render(strings.ToUpper(role)), w-6),
		"  " + dotLeader("Hostname", theme.AccentHiS.Render(d.Hostname), w-6),
		"  " + dotLeader("OS", theme.Item.Render(fallback(d.OS, "(unknown)")), w-6),
		"  " + dotLeader("Online", boolBadge(d.Online), w-6),
		"  " + dotLeader("Local device?", yesNoBadge(d.IsLocal), w-6),
		"  " + dotLeader("LAN IP", theme.Item.Render(fallback(d.LANIP, "(not on LAN)")), w-6),
		"  " + dotLeader("Tailscale IP",
			lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).
				Render(fallback(d.TailscaleIP, "(no TS IP)")), w-6),
		"  " + dotLeader("MAC address",
			theme.Item.Render(fallback(d.MAC, "(not in ARP cache)")), w-6),
	}
	if d.LastSeen != "" && !d.Online {
		rows = append(rows, "  "+dotLeader("Last seen", theme.ItemDim.Render(d.LastSeen), w-6))
	}

	// ── Affiliated services / containers attached to this device ──
	// Pull sidecar peers (filtered out of the topology) whose parent
	// host is this device. Only revealed in the FOCUSED card — keeps
	// the topology clean while making per-device service inventory one
	// ←→ keystroke away.
	affiliated := m.affiliatedSidecarsFor(d.Hostname)
	dockerNets := []string{}
	containers := []string{}
	if d.IsLocal {
		containers = d.Containers
		dockerNets = d.DockerNets
	}
	hasAffiliated := len(affiliated) > 0 || len(containers) > 0 || len(dockerNets) > 0

	inner := lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		strings.Join(rows, "\n"),
	)

	if hasAffiliated {
		divider := lipgloss.NewStyle().Foreground(border).
			Render("  " + strings.Repeat("─", w-8))
		section := lipgloss.NewStyle().Foreground(border).Bold(true).
			Render("  ── AFFILIATED SERVICES & CONTAINERS ──")
		inner += "\n\n" + divider + "\n" + section + "\n"

		// Tailnet-side sidecars (each is its own tailnet identity)
		if len(affiliated) > 0 {
			inner += "\n  " + theme.ItemDim.Render(
				fmt.Sprintf("Tailnet sidecars under %s (%d):",
					d.Hostname, len(affiliated))) + "\n"
			// Wrap chips into rows that fit width.
			chip := func(name string, online bool) string {
				bg := lipgloss.Color("#0891b2")
				if !online {
					bg = lipgloss.Color("#404040")
				}
				return lipgloss.NewStyle().
					Background(bg).Foreground(lipgloss.Color("#ffffff")).
					Bold(true).Padding(0, 1).
					Render(" " + name)
			}
			var lines []string
			var cur string
			for _, s := range affiliated {
				ch := chip(s.Hostname, s.Online)
				if lipgloss.Width(cur)+lipgloss.Width(ch)+1 > w-6 && cur != "" {
					lines = append(lines, cur)
					cur = ch
				} else if cur == "" {
					cur = ch
				} else {
					cur += " " + ch
				}
			}
			if cur != "" {
				lines = append(lines, cur)
			}
			for _, ln := range lines {
				inner += "  " + ln + "\n"
			}
		}

		// Docker-engine-visible containers (local host only — remote
		// would need an SSH round-trip per browse)
		if len(containers) > 0 {
			inner += "\n  " + theme.ItemDim.Render(
				fmt.Sprintf("Docker containers running here (%d):", len(containers))) + "\n"
			chip := func(name string) string {
				return lipgloss.NewStyle().
					Background(lipgloss.Color("#15803d")).
					Foreground(lipgloss.Color("#ffffff")).
					Bold(true).Padding(0, 1).
					Render(" " + name)
			}
			var lines []string
			var cur string
			for _, c := range containers {
				ch := chip(c)
				if lipgloss.Width(cur)+lipgloss.Width(ch)+1 > w-6 && cur != "" {
					lines = append(lines, cur)
					cur = ch
				} else if cur == "" {
					cur = ch
				} else {
					cur += " " + ch
				}
			}
			if cur != "" {
				lines = append(lines, cur)
			}
			for _, ln := range lines {
				inner += "  " + ln + "\n"
			}
		}

		// Docker networks (local host only)
		if len(dockerNets) > 0 {
			inner += "\n  " + theme.ItemDim.Render(
				fmt.Sprintf("Docker networks defined here (%d):", len(dockerNets))) + "\n"
			chip := func(name string) string {
				return lipgloss.NewStyle().
					Background(lipgloss.Color("#7c3aed")).
					Foreground(lipgloss.Color("#ffffff")).
					Bold(true).Padding(0, 1).
					Render(" " + name)
			}
			var nets []string
			for _, n := range dockerNets {
				nets = append(nets, chip(n))
			}
			inner += "  " + strings.Join(nets, " ") + "\n"
		}
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 2).
		Width(w).
		Render(inner)
}

// affiliatedSidecarsFor returns the sidecar PeerDevices whose parent
// host is `hostname`. Parent inference uses the same heuristic actions.
// ParentHostFor uses — short hostname embedded in the sidecar name.
func (m *Model) affiliatedSidecarsFor(hostname string) []PeerDevice {
	var out []PeerDevice
	short := strings.TrimPrefix(hostname, "n3m-")
	short = strings.ReplaceAll(short, "-", "")
	for _, d := range m.data.Devices {
		if !d.IsSidecar {
			continue
		}
		flat := strings.ReplaceAll(d.Hostname, "-", "")
		if strings.Contains(flat, short) {
			out = append(out, d)
		}
	}
	return out
}

func (m *Model) renderContainersStrip(w int) string {
	// Show the LOCAL host's containers as a horizontal chip strip — gives
	// the operator at-a-glance view of what services this host runs into
	// the tailnet. Only local because we can't enumerate docker on remote
	// hosts without an SSH round-trip per call.
	var local *PeerDevice
	for i := range m.data.Devices {
		if m.data.Devices[i].IsLocal {
			local = &m.data.Devices[i]
			break
		}
	}
	if local == nil {
		return ""
	}
	title := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true).
		Render("  ★  LOCAL HOST CONTAINERS ON TAILNET   (" + fmt.Sprintf("%d", len(local.Containers)) + ")")

	chip := func(name string) string {
		return lipgloss.NewStyle().
			Background(lipgloss.Color("#0891b2")).
			Foreground(lipgloss.Color("#ffffff")).
			Bold(true).Padding(0, 1).
			Render(" " + name)
	}

	var chips []string
	for _, c := range local.Containers {
		chips = append(chips, chip(c))
	}
	chipRow := "(none discovered)"
	if len(chips) > 0 {
		// Wrap chips into rows that fit width.
		var lines []string
		var cur string
		for _, ch := range chips {
			w0 := lipgloss.Width(cur)
			if w0+lipgloss.Width(ch)+1 > w-4 && cur != "" {
				lines = append(lines, cur)
				cur = ch
			} else if cur == "" {
				cur = ch
			} else {
				cur += " " + ch
			}
		}
		if cur != "" {
			lines = append(lines, cur)
		}
		chipRow = strings.Join(lines, "\n")
	}

	netsTitle := lipgloss.NewStyle().Foreground(lipgloss.Color("#22c55e")).Bold(true).
		Render("  ★  DOCKER NETWORKS   (" + fmt.Sprintf("%d", len(local.DockerNets)) + ")")
	netChips := []string{}
	for _, n := range local.DockerNets {
		netChips = append(netChips, lipgloss.NewStyle().
			Background(lipgloss.Color("#7c3aed")).
			Foreground(lipgloss.Color("#ffffff")).Bold(true).Padding(0, 1).
			Render(" " + n))
	}
	netRow := "(none)"
	if len(netChips) > 0 {
		netRow = strings.Join(netChips, " ")
	}

	inner := lipgloss.JoinVertical(lipgloss.Left,
		title, "",
		"  "+chipRow,
		"",
		netsTitle, "",
		"  "+netRow,
	)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#22c55e")).
		Padding(0, 2).
		Width(w).
		Render(inner)
}

func (m *Model) renderFirewallStrip(w int) string {
	title := lipgloss.NewStyle().Foreground(theme.Red).Bold(true).
		Render("  󰒃  LOCAL FIREWALL (head of iptables -L / nft list ruleset)")
	if m.fwLoading {
		return wrapTierPanel(title, "  "+theme.ItemDim.Render("loading…"),
			lipgloss.Color("#dc2626"), w)
	}
	var lines []string
	for _, ln := range m.data.Firewall {
		if ln == "" {
			continue
		}
		lines = append(lines, "  "+theme.ItemDim.Render(ln))
	}
	if len(lines) == 0 {
		lines = []string{"  " + theme.ItemDim.Render("(no firewall info — iptables/nft not readable as this user)")}
	}
	// Cap visual height — full firewall would push everything off-screen.
	if len(lines) > 8 {
		lines = lines[:8]
		lines = append(lines, "  "+theme.ItemDim.Render("…(truncated to first 8 rules)"))
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#dc2626")).
		Padding(0, 2).
		Width(w).
		Render(title + "\n\n" + strings.Join(lines, "\n"))
}

// ── small render helpers ─────────────────────────────────────────────

func theme_dim(s string) string { return s }

func countHosts(hosts []PeerDevice) int {
	n := 0
	for _, h := range hosts {
		if h.LANIP != "" {
			n++
		}
	}
	return n
}

func fallback(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func truncateMAC(mac string) string {
	if mac == "" {
		return "—"
	}
	if len(mac) > 17 {
		return mac[:17]
	}
	return mac
}

func yesNoBadge(b bool) string {
	if b {
		return lipgloss.NewStyle().
			Background(lipgloss.Color("#22c55e")).
			Foreground(lipgloss.Color("#000000")).Bold(true).
			Padding(0, 1).Render("YES")
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color("#404040")).
		Foreground(lipgloss.Color("#a3a3a3")).Padding(0, 1).
		Render("NO")
}

// connector renders a single centered "│" line — used between tiers in
// the topology tree so the eye traces parent→child relationships.
func connector(s string) string {
	return lipgloss.NewStyle().
		Foreground(lipgloss.Color("#525252")).
		Render("                              " + s)
}

func (m *Model) renderWANTier() string {
	title := lipgloss.NewStyle().Foreground(theme.Yellow).Bold(true).
		Render("  󰖟  INTERNET / WAN")
	var body string
	switch {
	case m.wanLoading:
		body = "  " + theme.ItemDim.Render("loading…")
	case m.data.WAN.Err != nil:
		body = "  " + theme.Err.Render("✗ ") + m.data.WAN.Err.Error()
	default:
		w := m.data.WAN
		body = strings.Join([]string{
			"  " + dotLeader("External IP", theme.AccentHiS.Render(w.ExternalIP), 56),
			"  " + dotLeader("ISP", theme.Item.Render(w.ISP), 56),
			"  " + dotLeader("ASN", theme.Item.Render(w.ASN), 56),
			"  " + dotLeader("Geo", theme.Item.Render(w.City+", "+w.Region+", "+w.Country), 56),
		}, "\n")
	}
	return wrapTierPanel(title, body, lipgloss.Color("#a16207"), 64)
}

func (m *Model) renderRouterTier() string {
	title := lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).
		Render("  󰑩  ROUTER / GATEWAY")
	var body string
	switch {
	case m.lanLoading:
		body = "  " + theme.ItemDim.Render("loading…")
	default:
		l := m.data.LAN
		body = strings.Join([]string{
			"  " + dotLeader("Gateway IP", theme.AccentHiS.Render(l.GatewayIP), 56),
			"  " + dotLeader("Subnet", theme.Item.Render(l.SubnetCIDR), 56),
			"  " + dotLeader("This host LAN IP", theme.Item.Render(l.LocalIP), 56),
			"  " + dotLeader("Interface", theme.Item.Render(l.Interface), 56),
		}, "\n")
	}
	return wrapTierPanel(title, body, lipgloss.Color("#0891b2"), 64)
}

func (m *Model) renderDevicesTier() string {
	title := lipgloss.NewStyle().Foreground(theme.Green).Bold(true).
		Render("  󰛳  DEVICES ON LAN + TAILNET")
	if m.peersLoading {
		return wrapTierPanel(title, "  "+theme.ItemDim.Render("loading…"),
			lipgloss.Color("#15803d"), 100)
	}

	// Filter sidecars out of the primary device tree — they bloat the
	// view and the operator's real interest is in the physical hosts.
	var hosts []PeerDevice
	for _, d := range m.data.Devices {
		if d.IsSidecar {
			continue
		}
		hosts = append(hosts, d)
	}

	// Tree drawing with ├ + └ corner connectors.
	var lines []string
	for i, d := range hosts {
		isLast := i == len(hosts)-1
		corner := "├──"
		if isLast {
			corner = "└──"
		}
		nameStyle := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
		if d.IsLocal {
			nameStyle = lipgloss.NewStyle().Foreground(theme.Green).Bold(true)
		} else if !d.Online {
			nameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#525252"))
		}
		osLabel := ""
		if d.OS != "" {
			osLabel = " (" + d.OS + ")"
		}
		row := "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("#525252")).
			Render(corner) + " " + nameStyle.Render(d.Hostname) +
			theme.ItemDim.Render(osLabel)
		if d.LANIP != "" {
			row += "  " + theme.ItemDim.Render("LAN ") +
				theme.Item.Render(d.LANIP)
		}
		if d.TailscaleIP != "" {
			row += "  " + theme.ItemDim.Render("TS ") +
				lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).Render(d.TailscaleIP)
		}
		if d.MAC != "" {
			row += "  " + theme.ItemDim.Render("MAC ") + theme.ItemDim.Render(d.MAC)
		}
		if !d.Online {
			row += "  " + lipgloss.NewStyle().
				Background(lipgloss.Color("#404040")).
				Foreground(lipgloss.Color("#a3a3a3")).
				Padding(0, 1).Render("OFFLINE")
		}
		lines = append(lines, row)
	}
	if len(lines) == 0 {
		lines = []string{"  " + theme.ItemDim.Render("(no devices)")}
	}
	return wrapTierPanel(title, strings.Join(lines, "\n"),
		lipgloss.Color("#15803d"), 130)
}

func (m *Model) renderFocusPane() string {
	if len(m.data.Devices) == 0 {
		return ""
	}
	if m.selectedDev >= len(m.data.Devices) {
		m.selectedDev = 0
	}
	d := m.data.Devices[m.selectedDev]
	title := lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).
		Render("  󰋊  DEVICE FOCUS — " + d.Hostname)
	var rows []string
	rows = append(rows,
		"  "+dotLeader("Hostname", theme.AccentHiS.Render(d.Hostname), 100),
		"  "+dotLeader("OS", theme.Item.Render(d.OS), 100),
		"  "+dotLeader("Online", boolBadge(d.Online), 100),
	)
	if d.LANIP != "" {
		rows = append(rows, "  "+dotLeader("LAN IP", theme.Item.Render(d.LANIP), 100))
	}
	if d.TailscaleIP != "" {
		rows = append(rows, "  "+dotLeader("Tailscale IP",
			lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).Render(d.TailscaleIP),
			100))
	}
	if d.MAC != "" {
		rows = append(rows, "  "+dotLeader("MAC address", theme.Item.Render(d.MAC), 100))
	}
	if d.IsLocal {
		rows = append(rows,
			"",
			lipgloss.NewStyle().Foreground(theme.Yellow).Bold(true).
				Render("  Application services on this host (tailnet-visible containers):"))
		if len(d.Containers) == 0 {
			rows = append(rows, "  "+theme.ItemDim.Render("(none discovered)"))
		} else {
			for _, c := range d.Containers {
				rows = append(rows, "  "+
					lipgloss.NewStyle().Foreground(lipgloss.Color("#0891b2")).
						Bold(true).Padding(0, 1).Render(""+c))
			}
		}
		rows = append(rows,
			"",
			lipgloss.NewStyle().Foreground(theme.Yellow).Bold(true).
				Render("  Docker networks visible:"))
		if len(d.DockerNets) == 0 {
			rows = append(rows, "  "+theme.ItemDim.Render("(none)"))
		} else {
			for _, n := range d.DockerNets {
				rows = append(rows, "  "+theme.Item.Render(" "+n))
			}
		}
	} else {
		rows = append(rows,
			"",
			"  "+theme.ItemDim.Render(
				"(container + docker enumeration only available for the local host)"))
	}
	return wrapTierPanel(title, strings.Join(rows, "\n"),
		lipgloss.Color("#7c3aed"), 130)
}

func (m *Model) renderFirewallTier() string {
	title := lipgloss.NewStyle().Foreground(theme.Red).Bold(true).
		Render("  󰒃  FIREWALL SUMMARY (local host, first 25 lines)")
	if m.fwLoading {
		return wrapTierPanel(title, "  "+theme.ItemDim.Render("loading…"),
			lipgloss.Color("#dc2626"), 130)
	}
	var rows []string
	for _, ln := range m.data.Firewall {
		rows = append(rows, "  "+theme.ItemDim.Render(ln))
	}
	if len(rows) == 0 {
		rows = []string{"  " + theme.ItemDim.Render("(no firewall info available — iptables/nft not readable as this user)")}
	}
	return wrapTierPanel(title, strings.Join(rows, "\n"),
		lipgloss.Color("#dc2626"), 130)
}

// wrapTierPanel produces a uniformly-styled bordered tier panel.
func wrapTierPanel(title, body string, border lipgloss.Color, width int) string {
	inner := lipgloss.JoinVertical(lipgloss.Left, title, "", body)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(width).
		Render(inner)
}

// dotLeader — same pattern used in the file browser detail pane: key,
// dots, value sized to fit. Kept as a package-local helper so netmap
// doesn't depend on wizards.
func dotLeader(label, value string, totalW int) string {
	labelStyled := theme.Title.Render(label)
	labelW := lipgloss.Width(labelStyled)
	valueW := lipgloss.Width(value)
	gap := totalW - labelW - valueW - 2
	if gap < 3 {
		gap = 3
	}
	dots := theme.ItemDim.Render(strings.Repeat(".", gap))
	return labelStyled + " " + dots + " " + value
}

func boolBadge(b bool) string {
	if b {
		return lipgloss.NewStyle().
			Background(lipgloss.Color("#15803d")).
			Foreground(lipgloss.Color("#ffffff")).
			Bold(true).Padding(0, 1).Render("ONLINE")
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color("#404040")).
		Foreground(lipgloss.Color("#a3a3a3")).
		Padding(0, 1).Render("OFFLINE")
}
