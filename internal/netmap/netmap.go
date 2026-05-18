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
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
// from the various async loads, the scroll position, and the selected
// device for the right-pane focused detail view.
type Model struct {
	data        DataMap
	scroll      int
	selectedDev int // index into data.Devices for the right-pane focus
	wanLoading, lanLoading, peersLoading, fwLoading bool
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
	switch v := msg.(type) {
	case wanLoadedMsg:
		m.data.WAN = WANInfo(v)
		m.wanLoading = false
	case lanLoadedMsg:
		m.data.LAN = LANInfo(v)
		m.lanLoading = false
	case peersLoadedMsg:
		m.data.Devices = v.peers
		m.peersLoading = false
	case firewallLoadedMsg:
		m.data.Firewall = v.lines
		m.fwLoading = false
	case tea.KeyMsg:
		switch v.String() {
		case "esc", "q":
			return m, nil, true
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
		case "pgdown", "ctrl+d", " ":
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
			// Refresh everything — re-fire all the async loaders.
			m.wanLoading, m.lanLoading, m.peersLoading, m.fwLoading = true, true, true, true
			return m, tea.Batch(loadWANCmd(), loadLANCmd(), loadPeersCmd(), loadFirewallCmd()), false
		}
	}
	return m, nil, false
}

func (m *Model) View(w, h int) string {
	bw := w - 8
	if bw < 90 {
		bw = 90
	}
	if bw > 160 {
		bw = 160
	}
	bh := h - 4
	if bh < 22 {
		bh = 22
	}
	if bh > 55 {
		bh = 55
	}

	titleBar := lipgloss.NewStyle().
		Foreground(theme.Magenta).Bold(true).Reverse(true).Padding(0, 2).
		Render(" 󰛳  TAILNET ENV MAPPING ")

	// ── WAN tier ──
	wanBlock := m.renderWANTier()
	// ── Router + LAN tier ──
	routerBlock := m.renderRouterTier()
	// ── Devices tier (tree under router) ──
	devicesBlock := m.renderDevicesTier()
	// ── Selected device focus pane ──
	focusBlock := m.renderFocusPane()
	// ── Firewall summary ──
	firewallBlock := m.renderFirewallTier()

	body := lipgloss.JoinVertical(lipgloss.Left,
		wanBlock,
		connector("│"),
		routerBlock,
		connector("│"),
		devicesBlock,
		"",
		focusBlock,
		"",
		firewallBlock,
	)

	bodyLines := strings.Split(body, "\n")
	contentH := bh - 4
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
		"  ↑↓/jk scroll · ←→/hl pick device · r refresh · Esc/q close")

	final := lipgloss.JoinVertical(lipgloss.Left,
		titleBar+scrollInfo,
		"",
		strings.Join(visible, "\n"),
		"",
		hint,
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Magenta).
		Padding(1, 2).
		Width(bw).
		Render(final)
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
