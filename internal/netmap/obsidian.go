// Obsidian export — scan the local filesystem for vaults (any dir
// containing a `.obsidian/` subdirectory is a vault root), then write
// the netmap topology as a mermaid-formatted markdown note into the
// chosen vault. Triggered by `o` from the netmap overlay.
package netmap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/theme"
)

// vaultsScannedMsg fires when the async scan finishes; carries the
// detected vault roots so the picker can render them.
type vaultsScannedMsg struct {
	vaults []string
	err    error
}

// exportDoneMsg fires when the mermaid file write completes (or fails).
type exportDoneMsg struct {
	path string
	err  error
}

// exportDismissMsg auto-dismisses the post-export notification popup.
type exportDismissMsg struct{}

// scanForVaultsCmd walks the user's home + a handful of common notes
// locations looking for any `.obsidian/` directory. The PARENT of that
// dir is the vault root. Bounded by depth + count + wall-time so a
// pathological filesystem doesn't lock the TUI.
func scanForVaultsCmd() tea.Cmd {
	return func() tea.Msg {
		const maxDepth = 8
		const maxResults = 64
		const maxTime = 5 * time.Second

		ctx, cancel := context.WithTimeout(context.Background(), maxTime)
		defer cancel()

		// Seed roots — places vaults usually live. We dedupe so the same
		// vault isn't reported twice if it's mounted in multiple places.
		var roots []string
		if h := os.Getenv("HOME"); h != "" {
			roots = append(roots,
				h,
				filepath.Join(h, "Documents"),
				filepath.Join(h, "Notes"),
				filepath.Join(h, "Obsidian"),
				filepath.Join(h, "Library", "Mobile Documents"), // macOS iCloud
			)
		}
		// Mounted volumes on macOS + Linux
		for _, p := range []string{"/Volumes", "/mnt", "/media"} {
			if _, err := os.Stat(p); err == nil {
				roots = append(roots, p)
			}
		}

		found := map[string]struct{}{}
		for _, root := range roots {
			if ctx.Err() != nil {
				break
			}
			if _, err := os.Stat(root); err != nil {
				continue
			}
			walkVaultRoots(ctx, root, 0, maxDepth, found)
			if len(found) >= maxResults {
				break
			}
		}

		vaults := make([]string, 0, len(found))
		for v := range found {
			vaults = append(vaults, v)
		}
		sort.Strings(vaults)
		return vaultsScannedMsg{vaults: vaults}
	}
}

// walkVaultRoots is the recursive helper for scanForVaultsCmd. Looks at
// each directory's children for a `.obsidian/` subdir; if found, the
// PARENT (current dir) is recorded as a vault root. Skips dot-prefixed
// directories OTHER than `.obsidian` itself + a few well-known noise
// folders to keep the walk fast.
func walkVaultRoots(ctx context.Context, dir string, depth, maxDepth int,
	found map[string]struct{}) {
	if ctx.Err() != nil || depth > maxDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// First pass: does this dir contain a .obsidian subdir? If yes, dir
	// is itself a vault root.
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == ".obsidian" {
			found[dir] = struct{}{}
			// Don't recurse INTO the vault — sub-vaults are rare and
			// noisy on the picker.
			return
		}
	}
	// Second pass: recurse into non-noise subdirs.
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Skip hidden, noise, and system dirs.
		if strings.HasPrefix(name, ".") {
			continue
		}
		switch name {
		case "node_modules", "vendor", ".git", "target", "build",
			"dist", "out", "Library", "Applications", "System":
			continue
		}
		walkVaultRoots(ctx, filepath.Join(dir, name), depth+1, maxDepth, found)
	}
}

// generateMermaid produces a complete Obsidian-ready markdown note
// containing the network topology as a mermaid `graph TD` block, plus
// frontmatter and a contextual breakdown below.
func generateMermaid(data DataMap, hostname string) string {
	var b strings.Builder

	// Frontmatter — useful when the operator dataview-queries vault for
	// these notes later.
	stamp := time.Now().Format(time.RFC3339)
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "title: \"Tailnet Env Mapping — %s\"\n", stamp)
	fmt.Fprintf(&b, "type: network-topology\n")
	fmt.Fprintf(&b, "generated_at: %s\n", stamp)
	fmt.Fprintf(&b, "generated_by: taildrives-cli\n")
	fmt.Fprintf(&b, "from_host: %s\n", hostname)
	fmt.Fprintf(&b, "external_ip: %s\n", data.WAN.ExternalIP)
	fmt.Fprintf(&b, "isp: \"%s\"\n", data.WAN.ISP)
	fmt.Fprintf(&b, "asn: %s\n", data.WAN.ASN)
	fmt.Fprintf(&b, "gateway: %s\n", data.LAN.GatewayIP)
	fmt.Fprintf(&b, "subnet: %s\n", data.LAN.SubnetCIDR)
	fmt.Fprintf(&b, "tags: [tailnet, network-map, infrastructure, taildrives]\n")
	fmt.Fprintf(&b, "---\n\n")

	// Heading
	fmt.Fprintf(&b, "# Tailnet Env Mapping — %s\n\n", time.Now().Format("Mon Jan 2, 2006 15:04"))
	fmt.Fprintf(&b, "Captured from `%s` via `taildrives` TUI.\n\n", hostname)

	// Mermaid diagram
	fmt.Fprintf(&b, "## Topology\n\n")
	fmt.Fprintf(&b, "```mermaid\n")
	fmt.Fprintf(&b, "graph TD\n")
	fmt.Fprintf(&b, "  classDef wan fill:#fef3c7,stroke:#f59e0b,color:#000,stroke-width:2px\n")
	fmt.Fprintf(&b, "  classDef modem fill:#fef9c3,stroke:#a16207,color:#000\n")
	fmt.Fprintf(&b, "  classDef router fill:#cffafe,stroke:#0891b2,color:#000,stroke-width:2px\n")
	fmt.Fprintf(&b, "  classDef server fill:#ecfeff,stroke:#0891b2,color:#000\n")
	fmt.Fprintf(&b, "  classDef nas fill:#fffbeb,stroke:#f59e0b,color:#000\n")
	fmt.Fprintf(&b, "  classDef wks fill:#eff6ff,stroke:#3b82f6,color:#000\n")
	fmt.Fprintf(&b, "  classDef mobile fill:#faf5ff,stroke:#a855f7,color:#000\n")
	fmt.Fprintf(&b, "  classDef peripheral fill:#f5f5f5,stroke:#737373,color:#000\n")
	fmt.Fprintf(&b, "  classDef local fill:#fce7f3,stroke:#ec4899,color:#000,stroke-width:3px,font-weight:bold\n")
	fmt.Fprintf(&b, "  classDef container fill:#dcfce7,stroke:#16a34a,color:#000\n")
	fmt.Fprintf(&b, "\n")

	// WAN → MODEM → ROUTER tier
	wan := data.WAN
	fmt.Fprintf(&b, "  WAN[\"🌐 INTERNET<br/>IP: %s<br/>%s · %s<br/>%s, %s\"]:::wan\n",
		safeMermaid(wan.ExternalIP), safeMermaid(wan.ASN), safeMermaid(wan.ISP),
		safeMermaid(wan.City), safeMermaid(wan.Region))
	fmt.Fprintf(&b, "  MODEM[\"📡 ISP Modem<br/>L1/L2 bridge\"]:::modem\n")
	fmt.Fprintf(&b, "  ROUTER[\"🔀 Router<br/>Gateway: %s<br/>Subnet: %s\"]:::router\n",
		safeMermaid(data.LAN.GatewayIP), safeMermaid(data.LAN.SubnetCIDR))
	fmt.Fprintf(&b, "  WAN --> MODEM\n  MODEM --> ROUTER\n\n")

	// Group + emit physical hosts. Same group ordering as the TUI.
	type bucket struct {
		label   string
		class   string
		devices []PeerDevice
	}
	buckets := []*bucket{
		{label: "SERVERS",      class: "server",     devices: nil},
		{label: "NAS",          class: "nas",        devices: nil},
		{label: "WORKSTATIONS", class: "wks",        devices: nil},
		{label: "MOBILE",       class: "mobile",     devices: nil},
		{label: "PERIPHERALS",  class: "peripheral", devices: nil},
	}
	for _, d := range data.Devices {
		if d.IsSidecar {
			continue
		}
		switch roleOf(d.Hostname) {
		case "server":
			buckets[0].devices = append(buckets[0].devices, d)
		case "nas":
			buckets[1].devices = append(buckets[1].devices, d)
		case "workstation":
			buckets[2].devices = append(buckets[2].devices, d)
		case "mobile":
			buckets[3].devices = append(buckets[3].devices, d)
		default:
			buckets[4].devices = append(buckets[4].devices, d)
		}
	}

	for _, bk := range buckets {
		if len(bk.devices) == 0 {
			continue
		}
		fmt.Fprintf(&b, "  subgraph %s [\"%s\"]\n", bk.label, bk.label)
		fmt.Fprintf(&b, "    direction LR\n")
		for _, d := range bk.devices {
			id := mermaidID(d.Hostname)
			class := bk.class
			if d.IsLocal {
				class = "local"
			}
			label := fmt.Sprintf("%s<br/>LAN: %s<br/>TS: %s<br/>%s",
				safeMermaid(d.Hostname),
				safeMermaid(fallback(d.LANIP, "—")),
				safeMermaid(fallback(d.TailscaleIP, "—")),
				safeMermaid(d.OS))
			if d.IsLocal {
				label = "★ THIS DEVICE<br/>" + label
			}
			fmt.Fprintf(&b, "    %s[\"%s\"]:::%s\n", id, label, class)
		}
		fmt.Fprintf(&b, "  end\n")
		fmt.Fprintf(&b, "  ROUTER --> %s\n\n", bk.label)
	}

	// Container subgraph for the local host's services (if any).
	var localDev *PeerDevice
	for i := range data.Devices {
		if data.Devices[i].IsLocal {
			localDev = &data.Devices[i]
			break
		}
	}
	if localDev != nil && len(localDev.Containers) > 0 {
		fmt.Fprintf(&b, "  subgraph CONTAINERS [\"🐳 CONTAINERS ON %s\"]\n",
			safeMermaid(localDev.Hostname))
		fmt.Fprintf(&b, "    direction LR\n")
		for _, c := range localDev.Containers {
			fmt.Fprintf(&b, "    %s[\"%s\"]:::container\n",
				mermaidID(c), safeMermaid(c))
		}
		fmt.Fprintf(&b, "  end\n")
		fmt.Fprintf(&b, "  %s --> CONTAINERS\n", mermaidID(localDev.Hostname))
	}
	fmt.Fprintf(&b, "```\n\n")

	// Below the diagram: structured detail breakdown.
	fmt.Fprintf(&b, "## Device inventory\n\n")
	fmt.Fprintf(&b, "| Hostname | Role | LAN IP | Tailscale IP | MAC | OS | Online |\n")
	fmt.Fprintf(&b, "|----------|------|--------|--------------|-----|-----|--------|\n")
	for _, d := range data.Devices {
		if d.IsSidecar {
			continue
		}
		online := "✓"
		if !d.Online {
			online = "✗"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | `%s` | `%s` | %s | %s |\n",
			d.Hostname, roleOf(d.Hostname),
			fallback(d.LANIP, "—"),
			fallback(d.TailscaleIP, "—"),
			fallback(d.MAC, "—"),
			fallback(d.OS, "—"),
			online)
	}
	fmt.Fprintf(&b, "\n")

	if localDev != nil && len(localDev.Containers) > 0 {
		fmt.Fprintf(&b, "## Containers on %s\n\n", localDev.Hostname)
		for _, c := range localDev.Containers {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		fmt.Fprintf(&b, "\n")
	}

	if localDev != nil && len(localDev.DockerNets) > 0 {
		fmt.Fprintf(&b, "## Docker networks on %s\n\n", localDev.Hostname)
		for _, n := range localDev.DockerNets {
			fmt.Fprintf(&b, "- `%s`\n", n)
		}
		fmt.Fprintf(&b, "\n")
	}

	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "_Generated by [taildrives](https://github.com/archn3m3sis/taildrives) on %s._\n",
		stamp)

	return b.String()
}

// mermaidID returns a sanitized identifier safe to use as a node ID in a
// mermaid graph. Mermaid IDs have to be alphanumeric/underscore.
func mermaidID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "n"
	}
	return "n_" + b.String()
}

// safeMermaid escapes characters that would break mermaid label parsing
// (quotes, square brackets, pipes). Mermaid is finicky about these
// inside `["..."]` labels.
func safeMermaid(s string) string {
	s = strings.ReplaceAll(s, "\"", "'")
	s = strings.ReplaceAll(s, "[", "(")
	s = strings.ReplaceAll(s, "]", ")")
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "<", "(")
	s = strings.ReplaceAll(s, ">", ")")
	return s
}

// writeExportToVaultCmd writes the generated mermaid markdown into
// <vault>/Network Maps/<timestamp>-tailnet-env-mapping.md.
func writeExportToVaultCmd(vault, content string) tea.Cmd {
	return func() tea.Msg {
		dir := filepath.Join(vault, "Network Maps")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return exportDoneMsg{err: err}
		}
		stamp := time.Now().Format("20060102-150405")
		path := filepath.Join(dir, stamp+"-tailnet-env-mapping.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return exportDoneMsg{err: err}
		}
		return exportDoneMsg{path: path}
	}
}

// renderVaultPicker draws the in-netmap overlay shown after `o` is
// pressed. Two states: scanning + ready-to-pick. Polished card with the
// mermaid-plugin requirement notice at the bottom so the operator
// doesn't get a broken diagram in Obsidian.
func renderVaultPicker(scanning bool, vaults []string, selected int, w int) string {
	bw := 84
	if w-12 < bw {
		bw = w - 12
	}
	if bw < 60 {
		bw = 60
	}

	titleBar := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#a855f7")).Bold(true).Reverse(true).
		Padding(0, 2).
		Render("  󰈙  EXPORT TO OBSIDIAN VAULT  (mermaid topology) ")

	subtitle := lipgloss.NewStyle().
		Foreground(theme.AccentHi).Italic(true).
		Render("  Walks $HOME, ~/Documents, ~/Notes, /Volumes, /mnt looking for")
	sub2 := lipgloss.NewStyle().
		Foreground(theme.AccentHi).Italic(true).
		Render("  any directory containing a .obsidian/ subdir (= vault root).")

	var listBlock string
	if scanning {
		listBlock = lipgloss.NewStyle().
			Foreground(theme.ItemDim.GetForeground()).
			Render("  scanning filesystem for vaults…")
	} else if len(vaults) == 0 {
		listBlock = lipgloss.NewStyle().
			Foreground(theme.Err.GetForeground()).
			Render("  No Obsidian vaults found. Create one in Obsidian first,") + "\n" +
			lipgloss.NewStyle().Foreground(theme.Err.GetForeground()).
				Render("  then retry. (Obsidian creates the .obsidian/ folder on first open.)")
	} else {
		var lines []string
		for i, v := range vaults {
			cursor := "    "
			labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
			if i == selected {
				cursor = lipgloss.NewStyle().Foreground(lipgloss.Color("#a855f7")).
					Bold(true).Render("  ▸ ")
				labelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#a855f7")).
					Bold(true).Underline(true)
			}
			vaultName := filepath.Base(v)
			vaultPath := lipgloss.NewStyle().Foreground(theme.ItemDim.GetForeground()).
				Render(v)
			lines = append(lines, cursor+labelStyle.Render(vaultName)+"    "+vaultPath)
		}
		listBlock = strings.Join(lines, "\n")
	}

	// Mermaid plugin notice — the WHOLE point of warning the operator.
	notice := lipgloss.NewStyle().
		Background(lipgloss.Color("#7c2d12")).
		Foreground(lipgloss.Color("#fde68a")).
		Bold(true).Padding(0, 2).
		Render("  ⚠  Obsidian must have its built-in Mermaid plugin enabled (Settings → Core plugins → Mermaid). ")
	notice2 := lipgloss.NewStyle().
		Background(lipgloss.Color("#7c2d12")).
		Foreground(lipgloss.Color("#fde68a")).
		Padding(0, 2).
		Render("     Without it, the ```mermaid block renders as raw code, not a diagram. ")

	hint := theme.ItemDim.Render(
		"  ↑↓ select · Enter export · Esc cancel")

	body := lipgloss.JoinVertical(lipgloss.Left,
		titleBar,
		"",
		subtitle, sub2,
		"",
		listBlock,
		"",
		notice, notice2,
		"",
		hint,
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("#a855f7")).
		Padding(1, 2).
		Width(bw).
		Render(body)
}

// renderExportNotification — green confirmation popup after the file
// successfully lands in the vault. Auto-dismisses via tea.Tick.
func renderExportNotification(path string, secondsLeft, w int) string {
	bw := 84
	if w-12 < bw {
		bw = w - 12
	}
	if bw < 60 {
		bw = 60
	}
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#22c55e")).Bold(true).Reverse(true).
		Padding(0, 2).
		Render("  ✓  EXPORTED TO OBSIDIAN ")
	body := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		"  "+lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("Topology written as mermaid markdown to:"),
		"",
		"  "+lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).
			Bold(true).Render(path),
		"",
		"  "+lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("Open the vault in Obsidian → Network Maps/ to view the diagram."),
		"",
		lipgloss.NewStyle().Foreground(theme.ItemDim.GetForeground()).Italic(true).
			Render(fmt.Sprintf("  Auto-dismiss in %ds…", secondsLeft)),
		"",
	)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("#22c55e")).
		Background(lipgloss.Color("#0a1f0a")).
		Padding(0, 1).
		Width(bw).
		Render(body)
}
