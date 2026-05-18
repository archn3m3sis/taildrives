// Package advanced houses the polished "scope preview" overlays for the
// Advanced Options tab — five features that have menu entries but their
// implementation will land across v0.14.x. Each preview is a designed
// panel (bordered, iconed, sectioned) showing the operator EXACTLY what's
// coming + when, so the menu entry is meaningful even before the feature
// lands. NOT a basic-zone TODO stub.
package advanced

import (
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/overlay"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

type Overlay = overlay.Overlay

// preview is the shared shape for all five scope-preview overlays. Each
// carries an icon glyph, a one-line tagline, a planned-scope block, an
// implementation-status line, and a target version tag.
type preview struct {
	icon       string
	title      string
	tagline    string
	scope      []string
	status     string
	targetVer  string
	accent     lipgloss.Color
}

func (p *preview) Init() tea.Cmd { return nil }

func (p *preview) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok {
		if key.Matches(km, key.NewBinding(key.WithKeys("esc", "q", "enter"))) {
			return p, nil, true
		}
	}
	return p, nil, false
}

func (p *preview) View(w, h int) string {
	bw := w - 12
	if bw < 70 {
		bw = 70
	}
	if bw > 110 {
		bw = 110
	}
	innerW := bw - 6

	titleBar := lipgloss.NewStyle().
		Foreground(p.accent).Bold(true).Reverse(true).Padding(0, 2).
		Render(" " + p.icon + "  " + p.title + " ")

	tagline := lipgloss.NewStyle().
		Foreground(theme.AccentHi).Italic(true).
		Render(p.tagline)

	versionTag := lipgloss.NewStyle().
		Background(p.accent).
		Foreground(lipgloss.Color("#000000")).
		Bold(true).
		Padding(0, 1).
		Render("TARGET " + p.targetVer)

	statusTag := lipgloss.NewStyle().
		Background(lipgloss.Color("#404040")).
		Foreground(lipgloss.Color("#fbbf24")).
		Bold(true).
		Padding(0, 1).
		Render("● " + p.status)

	scopeTitle := lipgloss.NewStyle().
		Foreground(p.accent).Bold(true).
		Render("PLANNED SCOPE")

	var scopeLines []string
	for _, s := range p.scope {
		scopeLines = append(scopeLines, "  "+
			lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).Render("→") +
			"  " + theme.Item.Render(s))
	}
	scopeBlock := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("#262626")).
		Padding(1, 2).
		Width(innerW).
		Render(scopeTitle + "\n\n" + strings.Join(scopeLines, "\n"))

	hint := theme.ItemDim.Render(
		"  Esc / q / Enter to return to the menu")

	body := lipgloss.JoinVertical(lipgloss.Left,
		titleBar,
		"",
		tagline,
		"",
		statusTag + "  " + versionTag,
		"",
		scopeBlock,
		"",
		hint,
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(p.accent).
		Padding(1, 2).
		Width(bw).
		Render(body)
}

// ── Public constructors — one per advanced action ─────────────────────

// NewDataTransmissionMapping previews the planned per-pair traffic map.
func NewDataTransmissionMapping() Overlay {
	return &preview{
		icon:      "󱎫", // nf-md-chart_line
		title:     "DATA TRANSMISSION MAPPING",
		tagline:   "Live byte-flow visualization across every tailnet pair, by direction and protocol.",
		accent:    lipgloss.Color("#0891b2"),
		status:    "Scoping complete — implementation queued",
		targetVer: "v0.14.x",
		scope: []string{
			"Per-pair bytes/sec (in + out) via `tailscale netstat` + tailscaled metrics",
			"Sortable matrix: src device × dst device, with throughput cell colorscale",
			"Top-N talkers + top-N listeners with sparklines for the last 5 minutes",
			"Protocol breakdown (TCP / UDP / ICMP) per pair",
			"Optional pcap-tap mode for one selected pair (privacy-gated, capital-K confirm)",
			"Export current snapshot as CSV via Ctrl+E for off-line analysis",
		},
	}
}

// NewDERPServerStatus previews the planned DERP relay health view.
func NewDERPServerStatus() Overlay {
	return &preview{
		icon:      "󰒍", // nf-md-server_network
		title:     "DERP SERVER STATUS",
		tagline:   "Health and latency profile of every DERP relay your tailnet is using.",
		accent:    lipgloss.Color("#7c3aed"),
		status:    "Scoping complete — implementation queued",
		targetVer: "v0.14.x",
		scope: []string{
			"Live `tailscale netcheck` parse: nearest DERP, region, RTT, packet loss",
			"Per-region grid: status (UP/DOWN/DEGRADED), median RTT, last observed",
			"DERP fallback rate across the mesh — how many sessions are relayed vs direct?",
			"Historical strip-chart: nearest-DERP RTT over the last hour",
			"Click-to-investigate: drill into one region to see all peers routing through it",
			"Custom-DERP detection (if the tailnet uses self-hosted DERP servers)",
		},
	}
}

// NewConnectionTypeSummary previews the planned direct-vs-relayed breakdown.
func NewConnectionTypeSummary() Overlay {
	return &preview{
		icon:      "󰴽", // nf-md-network_pos
		title:     "CONNECTION TYPE SUMMARY",
		tagline:   "Per-peer breakdown of how your tailnet sessions are actually routed.",
		accent:    lipgloss.Color("#10b981"),
		status:    "Scoping complete — implementation queued",
		targetVer: "v0.14.x",
		scope: []string{
			"Per-peer table: direct UDP, direct IPv6, DERP-relayed, offline",
			"NAT type detection: open / restricted-cone / symmetric / port-restricted",
			"UPnP / PCP / NAT-PMP detection on the local network",
			"Hairpinning status (does the router loop back to the WAN IP?)",
			"IPv6 availability + Tailscale4via6 / via6 routing where applicable",
			"Per-peer last-handshake age + key rotation status",
		},
	}
}

// NewTSCLIScheduler previews the planned tailscale-cli action scheduler.
func NewTSCLIScheduler() Overlay {
	return &preview{
		icon:      "󰃭", // nf-md-calendar_clock
		title:     "TS-CLI SCHEDULER",
		tagline:   "Cron-style scheduling for tailscale-cli + taildrives actions across the mesh.",
		accent:    lipgloss.Color("#f59e0b"),
		status:    "Scoping complete — implementation queued",
		targetVer: "v0.14.x",
		scope: []string{
			"Per-mesh-device schedule sheets — schedule any tailscale/taildrives command",
			"Visual cron-editor: presets (hourly/daily/weekly) + raw cron expression mode",
			"Common workflows pre-baked: nightly drive-share sweep, weekly key rotation,",
			"daily `tailscale netcheck` → Pushover alert if DERP-only",
			"Run-history viewer per scheduled job, color-coded pass/fail/skip",
			"Backed by a NixOS-managed systemd timer set so schedules survive restarts",
		},
	}
}

// NewTSCLIWatcher previews the planned tailscale state-change watcher.
func NewTSCLIWatcher() Overlay {
	return &preview{
		icon:      "󰈈", // nf-md-eye
		title:     "TS-CLI WATCHER",
		tagline:   "Live tailnet event stream — what changed, when, and on which device.",
		accent:    lipgloss.Color("#ef4444"),
		status:    "Scoping complete — implementation queued",
		targetVer: "v0.14.x",
		scope: []string{
			"Live stream of tailnet events: device online/offline, key rotation, ACL push",
			"Filter sidebar: by device, by event type, by severity",
			"Pinnable conditions: alert if a specific device drops off for >N minutes",
			"Compact log line per event + drill-in detail view for the full event payload",
			"Hook integration: trigger Pushover / Telegram / webhook on matching events",
			"Replay buffer (last 1000 events) viewable even after the watcher reconnects",
		},
	}
}
