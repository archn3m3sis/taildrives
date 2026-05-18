// Package wizards is the in-TUI overlay set the splash menu launches —
// reports (status/performance/journal) render as scrollable text popups,
// share add/remove run as multi-step Bubble Tea sub-models. Nothing in
// here ever drops back to the raw terminal.
package wizards

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/actions"
	"github.com/archn3m3sis/taildrives/internal/local"
	"github.com/archn3m3sis/taildrives/internal/overlay"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

// Overlay is the shared interface — re-exported so wizard call-sites can
// use wizards.Overlay symmetrically with splash.Overlay.
type Overlay = overlay.Overlay

// renderStepBadges renders a horizontal completion-map strip: one styled
// tag per wizard step. Tag at index < active = "done" (green bg + ✓ glyph
// + uppercase label). Tag at index == active = "current" (yellow accent
// bg, bold). Tag at index > active = "pending" (muted dark bg, dim text).
// Active == len(steps) is the all-done sentinel (running/result phases).
// Joined with a small ›-style separator so the strip reads as flow.
//
// Used by both the Add and Remove wizards so the visual language stays
// consistent across the wizard suite.
func renderStepBadges(steps []string, active int) string {
	const sep = " "
	doneBG := lipgloss.Color("#15803d")    // green-700 — completed
	currentBG := lipgloss.Color("#eab308") // yellow-500 — matches title accent
	pendingBG := lipgloss.Color("#262626") // neutral-800 — quiet, not invisible
	textFG := lipgloss.Color("#ffffff")
	pendingFG := lipgloss.Color("#6b7280") // neutral-500 — readable but recessed
	arrowFG := lipgloss.Color("#404040")

	var out strings.Builder
	for i, label := range steps {
		var bg, fg lipgloss.Color
		var prefix string
		switch {
		case i < active:
			bg, fg = doneBG, textFG
			prefix = "✓ "
		case i == active:
			bg, fg = currentBG, lipgloss.Color("#000000")
		default:
			bg, fg = pendingBG, pendingFG
		}
		badge := lipgloss.NewStyle().
			Background(bg).Foreground(fg).Bold(i <= active).
			Padding(0, 1).
			Render(prefix + label)
		if i > 0 {
			out.WriteString(sep)
			out.WriteString(lipgloss.NewStyle().Foreground(arrowFG).Render("›"))
			out.WriteString(sep)
		}
		out.WriteString(badge)
	}
	return out.String()
}

// ── Common scrollable report popup ─────────────────────────────────────

type reportOverlay struct {
	title   string
	color   lipgloss.Color
	content string
	scroll  int

	// Transient feedback state:
	//   justCopied  — one-render bottom-bar "✓ Copied" hint
	//   printNotifyAt — sets a deadline for the centered print notification
	//                   popup; auto-dismisses when time.Now() passes the
	//                   deadline. A tea.Tick re-renders us when it expires.
	justCopied    bool
	printNotifyAt time.Time
}

func newReport(title, content string, color lipgloss.Color) *reportOverlay {
	return &reportOverlay{title: title, color: color, content: content}
}

func (r *reportOverlay) Init() tea.Cmd { return nil }

func (r *reportOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	// Tick from a prior Ctrl+P fires when the notification's display
	// window expires — clears printNotifyAt by leaving it in the past;
	// the next View call drops the popup.
	if _, ok := msg.(printDismissMsg); ok {
		r.printNotifyAt = time.Time{}
		return r, nil, false
	}
	// Clear the one-shot copy flag on ANY non-hint event so it
	// disappears once the operator does anything else.
	r.justCopied = false
	if km, ok := msg.(tea.KeyMsg); ok {
		switch {
		case key.Matches(km, key.NewBinding(key.WithKeys("ctrl+c"))):
			// Inside a report overlay, Ctrl+C means COPY, not QUIT — same
			// muscle memory as every other app. The overlay intercepts so
			// the global quit handler never sees it. q / Esc / Enter
			// still dismiss the overlay normally.
			r.justCopied = true
			return r, copyToClipboardCmd(r.content), false
		case key.Matches(km, key.NewBinding(key.WithKeys("ctrl+p"))):
			// Ctrl+P writes the report to a temp file then opens it in
			// the system default text viewer. We pop up a 5-second
			// centered notification explaining the next step (operator
			// must press Cmd/Ctrl+P inside that editor for an actual
			// physical print). Auto-dismisses via tea.Tick.
			r.printNotifyAt = time.Now().Add(5 * time.Second)
			return r, tea.Batch(
				printReportCmd(r.content, r.title),
				tea.Tick(5*time.Second, func(time.Time) tea.Msg {
					return printDismissMsg{}
				}),
			), false
		case key.Matches(km, key.NewBinding(key.WithKeys("q", "esc", "enter"))):
			return r, nil, true
		case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
			if r.scroll > 0 {
				r.scroll--
			}
		case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
			r.scroll++
		case key.Matches(km, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
			r.scroll -= 10
			if r.scroll < 0 {
				r.scroll = 0
			}
		case key.Matches(km, key.NewBinding(key.WithKeys("pgdown", "ctrl+d", " "))):
			r.scroll += 10
		case key.Matches(km, key.NewBinding(key.WithKeys("home", "g"))):
			r.scroll = 0
		case key.Matches(km, key.NewBinding(key.WithKeys("end", "G"))):
			r.scroll = 1 << 30 // clamped in View
		}
	}
	return r, nil, false
}

// printDismissMsg fires from the tea.Tick a report sets up when Ctrl+P
// is pressed. Receiving it clears printNotifyAt so the next View drops
// the centered print-notification popup.
type printDismissMsg struct{}

// copyToClipboardCmd returns a Cmd that pushes content to the system
// clipboard via OSC 52 — an ANSI escape sequence the terminal interprets
// as "put this base64 payload on the user's clipboard." Works locally
// AND over SSH because the LOCAL terminal handles the clipboard write,
// not the remote host (no need for pbcopy / xclip on the remote).
//
// Supported by Ghostty (operator's terminal), Kitty, iTerm2, WezTerm,
// Alacritty (with config), and tmux (with set-clipboard on). Terminals
// that don't support OSC 52 will silently drop the sequence — no error,
// just no copy. We don't bother detecting support since the cost of
// trying is zero.
func copyToClipboardCmd(content string) tea.Cmd {
	return func() tea.Msg {
		// Strip ANSI color codes from the content so what lands on the
		// clipboard is the readable text, not the escape-soup that the
		// terminal renders as colors. ansiRE matches the common SGR
		// sequences that the lipgloss styler emits.
		stripped := ansiRE.ReplaceAllString(content, "")
		seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(stripped)) + "\x07"
		os.Stdout.WriteString(seq)
		return nil
	}
}

// ansiRE matches CSI/SGR escape sequences (e.g. \x1b[31m, \x1b[0m). Used
// to plain-ify report content before sending it to the clipboard or to a
// printable temp file.
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// printReportCmd writes the report content (stripped of ANSI color codes)
// to a timestamped temp file, then spawns the OS default-handler so the
// operator gets a native print dialog (Cmd+P / Ctrl+P).
//
// macOS: `open` lands the file in TextEdit by default — the standard
//        print path is Cmd+P from there.
// Linux: `xdg-open` routes through the desktop's MIME handler.
// Windows: `cmd /c start` opens the .txt in whatever's associated.
//
// Caveat: over SSH this fires on the REMOTE host. Limitation, not a bug —
// terminal protocols don't have a print-this-content primitive equivalent
// to OSC 52's clipboard primitive. The justPrinted status text tells the
// operator where the file landed so they can find it if the open fails.
func printReportCmd(content, title string) tea.Cmd {
	return func() tea.Msg {
		stripped := ansiRE.ReplaceAllString(content, "")
		slug := slugifyTitle(title)
		fname := "taildrives-" + slug + "-" + time.Now().Format("20060102-150405") + ".txt"
		path := filepath.Join(os.TempDir(), fname)
		if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
			return nil
		}
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", path)
		case "linux":
			c = exec.Command("xdg-open", path)
		case "windows":
			c = exec.Command("cmd", "/c", "start", "", path)
		}
		if c != nil {
			_ = c.Start() // detached; we don't care about result
		}
		return nil
	}
}

// slugifyTitle turns "TAILSCALE DEVICE STATUS REPORT" into
// "tailscale-device-status-report" — lowercase, alnum + hyphens, suitable
// for a temp filename across all three OSes.
func slugifyTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func (r *reportOverlay) View(w, h int) string {
	bw := w - 12
	if bw < 60 {
		bw = 60
	}
	if bw > 160 {
		bw = 160
	}
	bh := h - 6
	if bh < 10 {
		bh = 10
	}
	if bh > 55 {
		bh = 55
	}
	contentH := bh - 5
	lines := strings.Split(r.content, "\n")
	total := len(lines)
	maxScroll := total - contentH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if r.scroll > maxScroll {
		r.scroll = maxScroll
	}
	end := r.scroll + contentH
	if end > total {
		end = total
	}
	visible := lines[r.scroll:end]
	for i, ln := range visible {
		if lipgloss.Width(ln) > bw-2 {
			runes := []rune(ln)
			for lipgloss.Width(string(runes))+1 > bw-2 {
				runes = runes[:len(runes)-1]
			}
			visible[i] = string(runes) + "…"
		}
	}
	titleBar := lipgloss.NewStyle().
		Foreground(r.color).Bold(true).Reverse(true).
		Padding(0, 2).Render(" " + r.title + " ")
	scrollInfo := ""
	if maxScroll > 0 {
		scrollInfo = "  " + theme.ItemDim.Render(itoa(r.scroll+1)+"-"+itoa(end)+" / "+itoa(total))
	}
	// Transient action hint takes precedence over the static keybinding
	// hint for one render — operator gets immediate feedback that their
	// keystroke landed before the bar reverts to the help text. Static
	// hint uses nerd-font glyphs (copy /  print / etc.) so the
	// shortcuts have the same visual reference language the file browser
	// uses for its filetype tags.
	var hint string
	switch {
	case r.justCopied:
		hint = lipgloss.NewStyle().Foreground(theme.Green).Bold(true).
			Render(" Copied to clipboard (via OSC 52)")
	default:
		hint = theme.ItemDim.Render(
			"↑↓/jk scroll · PgUp/PgDn jump · g/G top/bot ·  Ctrl+C copy ·  Ctrl+P print · Enter/Esc close")
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		titleBar+scrollInfo,
		"",
		strings.Join(visible, "\n"),
		"",
		hint)
	rendered := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(r.color).
		Padding(1, 2).
		Width(bw).
		Render(body)
	// Centered print-notification popup overlays on top while the
	// 5-second display window is active. Composed via overlayCenter
	// which splices a styled box into the middle of the underlying view.
	if !r.printNotifyAt.IsZero() && time.Now().Before(r.printNotifyAt) {
		remaining := int(time.Until(r.printNotifyAt).Seconds()) + 1
		popup := renderPrintNotification(remaining, w)
		return overlayCenter(rendered, popup, w, h)
	}
	return rendered
}

// renderPrintNotification produces the centered popup shown when Ctrl+P
// fires. Tells the operator that the report was exported to their
// system's default text editor and that the actual physical print step
// happens INSIDE that editor (Cmd+P / Ctrl+P from there) — addresses
// the common confusion of "I pressed print but nothing came out."
func renderPrintNotification(secondsLeft, w int) string {
	bw := 72
	if w < 80 {
		bw = w - 8
	}
	icon := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fbbf24")).Bold(true).
		Render("")
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fbbf24")).Bold(true).Reverse(true).
		Padding(0, 2).
		Render("  " + icon + "  EXPORTED FOR PRINTING ")
	msgLines := []string{
		"",
		lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("  TAILSCALE-CLI has exported your summary to your"),
		lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("  device's local default text editing program."),
		"",
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  For a physical print of your summary, please print"),
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  from the context menu inside of your text editor"),
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  (typically Cmd+P on macOS, Ctrl+P on Linux/Windows)."),
		"",
		lipgloss.NewStyle().Foreground(theme.ItemDim.GetForeground()).Italic(true).
			Render(fmt.Sprintf("  This notification auto-dismisses in %ds…", secondsLeft)),
		"",
	}
	body := title + "\n" + strings.Join(msgLines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("#fbbf24")).
		Background(lipgloss.Color("#1a1a1a")).
		Padding(0, 1).
		Width(bw).
		Render(body)
}

// overlayCenter composites `popup` centered onto `underlay`. Same
// technique the splash uses for its overlay — split into lines, overwrite
// the middle rows so the popup floats on top of the report.
func overlayCenter(underlay, popup string, w, h int) string {
	bw := lipgloss.Width(popup)
	bh := lipgloss.Height(popup)
	x := (w - bw) / 2
	y := (h - bh) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	mainLines := strings.Split(underlay, "\n")
	popupLines := strings.Split(popup, "\n")
	for i, pl := range popupLines {
		row := y + i
		if row >= len(mainLines) {
			break
		}
		// Replace the row's prefix with spaces up to x, then the popup
		// line. ANSI-aware truncation is impractical here, so just write
		// x spaces + popup line and let the terminal handle the rest.
		mainLines[row] = strings.Repeat(" ", x) + pl
	}
	return strings.Join(mainLines, "\n")
}

// ── Public constructors ────────────────────────────────────────────────

// NewHelpOverlay returns a purpose-built help overlay (not the generic
// reportOverlay) so the layout can be a real designed UI: bordered
// sections, two-column keybindings, header with GitHub link badge, and
// no spurious Ctrl+C/Ctrl+P advertising (those only make sense inside
// streamed reports).
func NewHelpOverlay() Overlay {
	return newHelpOverlay()
}

// helpOverlay is the in-splash help screen. Lives inside the TUI; Esc
// dismisses it back to the splash menu. Tab cycles between the two
// content tabs (STANDARD + ADVANCED FEATURES). Scroll position is
// remembered per-tab so flipping back doesn't lose your place.
type helpOverlay struct {
	tab     int   // 0 = STANDARD, 1 = ADVANCED FEATURES
	scrolls [2]int
}

func newHelpOverlay() *helpOverlay { return &helpOverlay{} }

func (h *helpOverlay) Init() tea.Cmd { return nil }

func (h *helpOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "esc", "q", "enter", "?":
			return h, nil, true
		case "tab":
			h.tab = (h.tab + 1) % 2
			return h, nil, false
		case "shift+tab":
			h.tab = (h.tab + 1) % 2 // only 2 tabs, same effect
			return h, nil, false
		case "up", "k":
			if h.scrolls[h.tab] > 0 {
				h.scrolls[h.tab]--
			}
		case "down", "j":
			h.scrolls[h.tab]++
		case "pgup", "ctrl+u":
			h.scrolls[h.tab] -= 10
			if h.scrolls[h.tab] < 0 {
				h.scrolls[h.tab] = 0
			}
		case "pgdown", "ctrl+d", " ":
			h.scrolls[h.tab] += 10
		case "home", "g":
			h.scrolls[h.tab] = 0
		case "end", "G":
			h.scrolls[h.tab] = 1 << 30 // clamped in View
		}
	}
	return h, nil, false
}

func (h *helpOverlay) View(w, hPane int) string {
	// Pane width: hard-cap so help reads well even on ultrawide terminals.
	bw := w - 8
	if bw < 70 {
		bw = 70
	}
	if bw > 130 {
		bw = 130
	}
	bh := hPane - 4
	if bh < 18 {
		bh = 18
	}
	if bh > 50 {
		bh = 50
	}
	innerW := bw - 6 // border + padding

	// Per-tab accent color so the visual identity of the tab is obvious.
	accent := theme.Yellow
	if h.tab == 1 {
		accent = theme.Magenta
	}

	// ── Header section: title + tagline + links ──────────────────────
	titleBar := lipgloss.NewStyle().
		Foreground(accent).Bold(true).Reverse(true).Padding(0, 2).
		Render("   TAILDRIVES — HELP ")
	tagline := lipgloss.NewStyle().Foreground(theme.AccentHi).Italic(true).
		Render("In-house Bubble Tea TUI + CLI for managing Tailscale Drive shares across the n3m mesh.")
	githubLink := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a1a")).
		Foreground(lipgloss.Color("#22d3ee")).Bold(true).Padding(0, 1).
		Render(" github.com/archn3m3sis/taildrives")
	docsLink := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a1a")).
		Foreground(lipgloss.Color("#22d3ee")).Padding(0, 1).
		Render(" tailscale.com/kb/1369/tailscale-drive")
	// Tab strip — same visual language as the splash menu's tab nav.
	pill := func(label string, isActive bool) string {
		st := lipgloss.NewStyle().Padding(0, 2)
		if isActive {
			return st.Background(accent).Foreground(lipgloss.Color("#000000")).
				Bold(true).Render(label)
		}
		return st.Foreground(lipgloss.Color("#525252")).Render(label)
	}
	tabStrip := pill("STANDARD", h.tab == 0) +
		lipgloss.NewStyle().Foreground(lipgloss.Color("#262626")).Render(" │ ") +
		pill("ADVANCED FEATURES", h.tab == 1)
	header := lipgloss.JoinVertical(lipgloss.Left,
		titleBar,
		"",
		tagline,
		"",
		githubLink+"  "+docsLink,
		"",
		tabStrip,
	)

	// ── Section helpers — each section is its OWN bordered panel ─────
	sectionTitle := func(label string) string {
		return lipgloss.NewStyle().
			Foreground(theme.Yellow).Bold(true).
			Render(label)
	}
	kv := func(k, v string) string {
		keyStyled := lipgloss.NewStyle().
			Background(lipgloss.Color("#262626")).
			Foreground(lipgloss.Color("#22d3ee")).Bold(true).
			Padding(0, 1).Render(k)
		// Two-column: key gets a fixed cell, description fills rest.
		const keyColW = 22
		pad := keyColW - lipgloss.Width(keyStyled)
		if pad < 1 {
			pad = 1
		}
		return "  " + keyStyled + strings.Repeat(" ", pad) +
			theme.Item.Render(v)
	}

	// ── Section: TUI BASICS ─────────────────────────────────────────
	basics := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("TUI BASICS"),
		"",
		kv("↑↓ / jk", "move within active pane"),
		kv("← → / hl", "back / drill into folder or pane"),
		kv("g / G", "jump to top / bottom"),
		kv("PgUp / PgDn", "page up / page down"),
		kv("Tab", "cycle panes (Categories → Devices → Files)"),
		kv("Enter", "select / drill in"),
		kv("Backspace", "parent dir / previous pane"),
	)

	// ── Section: SEARCH & FILTER ─────────────────────────────────────
	search := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("SEARCH & FILTER"),
		"",
		kv("s  or  /", "fuzzy search across the current view (live filter)"),
		kv("Ctrl+D", "(in search) toggle directories-only mode"),
		kv("t", "category filter (greys out non-matching shares)"),
		kv(".", "(in file browser) toggle hidden files"),
	)

	// ── Section: ACTIONS ─────────────────────────────────────────────
	actions := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("ACTIONS"),
		"",
		kv("d", "download highlighted file or directory"),
		kv("u", "upload a local file into the current dir"),
		kv("c", "ARM copy → navigate → press c again to drop (Esc cancels)"),
		kv("b", "bulk-send marked files to one or more devices"),
		kv("Space", "mark/unmark   ·   Ctrl+A all   ·   Ctrl+X clear marks"),
		kv("P", "split-pane preview (text / Kitty graphics image)"),
		kv("r  or  F5", "refresh current directory"),
		kv("m", "show davfs2 mount command for the current path"),
		kv("D (capital)", "delete (God Mode gated, irreversible)"),
	)

	// ── Section: REPORTS ────────────────────────────────────────────
	reports := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("INSIDE REPORTS (perf / device status / journal)"),
		"",
		kv(" Ctrl+C", "copy report to clipboard (via OSC 52)"),
		kv(" Ctrl+P", "print — opens report in default viewer for printing"),
		"  "+theme.ItemDim.Render("(journal asks for a date range before copy/print fires)"),
	)

	// ── Section: GLOBAL ─────────────────────────────────────────────
	global := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("GLOBAL"),
		"",
		kv("?", "this help menu"),
		kv("Esc  Esc", "double-tap anywhere to exit with the outro animation"),
		kv("q  ·  Ctrl+C", "(outside reports) quit immediately"),
	)

	// ── Section: BASIC OPERATION (numbered) ──────────────────────────
	stepStyle := lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
	step := func(n int, text string) string {
		return "  " + stepStyle.Render(itoa(n)+".") + " " + theme.Item.Render(text)
	}
	operation := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("BASIC OPERATION"),
		"",
		step(1, "Splash menu → pick ENTER TAILDRIVES-CLI to open the main TUI."),
		step(2, "Left pane = Categories (filters)   ·   Middle = Devices   ·   Right = Files"),
		step(3, "Tab between panes   ·   ↑↓ to highlight   ·   Enter to drill in"),
		step(4, "Press s to fuzzy-search the current view   ·   Ctrl+D narrows to dirs only"),
		step(5, "Share a folder: splash menu → ADD TAILDRIVE SHARES → walk the wizard"),
		step(6, "Remove a share: splash menu → REMOVE TAILDRIVE SHARES"),
		step(7, "File browser: . toggles hidden   ·   ↗ marks symlinks   ·   [SHARED] = already published"),
		step(8, "Double-tap Esc anywhere to exit with the farewell animation"),
	)

	// ── Section: TAILSCALE DRIVE COMMANDS (reference) ────────────────
	cmdLine := func(c, d string) string {
		return "  " + lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).Render(c) +
			"   " + theme.ItemDim.Render(d)
	}
	commands := lipgloss.JoinVertical(lipgloss.Left,
		sectionTitle("TAILSCALE DRIVE CLI (reference — what taildrives wraps)"),
		"",
		cmdLine("tailscale drive list                ", "list shares published by this device"),
		cmdLine("tailscale drive share <name> <path> ", "publish <path> as a Drive share named <name>"),
		cmdLine("tailscale drive unshare <name>      ", "remove a share (does NOT delete data)"),
		cmdLine("tailscale drive set <name> <opts>   ", "tune share options (1.96+)"),
		"",
		"  "+theme.ItemDim.Render("Share name rules: [a-z0-9_] only · no hyphens · no dots · max 24 chars"),
		"  "+theme.ItemDim.Render("Add wizard auto-sanitizes — type whatever, it'll coerce on submit"),
		"",
		sectionTitle("TAILDRIVES CLI (headless equivalents)"),
		"",
		cmdLine("taildrives list                     ", "every share on every reachable device"),
		cmdLine("taildrives types                    ", "category labels and what they match"),
		cmdLine("taildrives ls / cat / get / put     ", "WebDAV path browsing + file I/O"),
		cmdLine("taildrives copy / bulk-send         ", "intra/inter-device copies + fan-out"),
		cmdLine("taildrives mount / serve / devices  ", "davfs2 helper · WebDAV URL · tailnet list"),
		cmdLine("taildrives version                  ", "build version"),
	)

	// Compose the full body. Each section becomes a bordered panel so
	// the layout has clear visual structure.
	wrap := func(s string) string {
		return lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#262626")).
			Padding(0, 2).
			Width(innerW).
			Render(s)
	}

	// ── ADVANCED FEATURES tab sections ─────────────────────────────
	// Same kv / sectionTitle helpers, magenta accent variants for the
	// section headers so it visually belongs to the magenta tab.
	advSectionTitle := func(label string) string {
		return lipgloss.NewStyle().
			Foreground(theme.Magenta).Bold(true).
			Render(label)
	}

	advTagline := lipgloss.NewStyle().Foreground(theme.AccentHi).Italic(true).
		Render("Advanced Options live on the third splash tab — diagnostic / observability features for tailnet topology, traffic, and scheduling.")

	netmapHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󰛳  TAILNET ENV MAPPING (live — v0.14.0)"),
		"",
		"  "+theme.Item.Render("Multi-tier topology map from your ISP down to per-device containers."),
		"  "+theme.Item.Render("Tiers paint progressively as their async data sources return."),
		"",
		kv("WAN tier", "external IP · ISP · ASN · geo (via ipinfo.io, falls back to ipify)"),
		kv("Router tier", "gateway IP · subnet CIDR · this host's LAN IP + interface"),
		kv("Devices tree", "tailnet hosts + LAN IPs + MAC + OS, sidecar containers filtered"),
		kv("Device focus", "←→ to pick · shows running containers + docker networks (local host only)"),
		kv("Firewall tier", "first 25 lines of `iptables -L -n` or `nft list ruleset` fallback"),
		"",
		kv("r", "refresh — re-fires all four async loaders"),
		kv("↑↓ / jk", "scroll the overlay"),
		kv("PgUp/PgDn", "page jumps"),
		kv("Esc / q", "close, return to splash menu"),
	)

	dtmHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󱎫  DATA TRANSMISSION MAPPING (v0.14.x)"),
		"",
		"  "+theme.Item.Render("Live byte-flow visualization across every tailnet pair, by direction and protocol."),
		"",
		"  "+theme.AccentHiS.Render("Planned:"),
		"  → "+theme.Item.Render("Per-pair bytes/sec (in + out) via `tailscale netstat` + tailscaled metrics"),
		"  → "+theme.Item.Render("Sortable matrix: src × dst, throughput cell colorscale"),
		"  → "+theme.Item.Render("Top-N talkers + listeners with 5-min sparklines"),
		"  → "+theme.Item.Render("Protocol breakdown (TCP / UDP / ICMP) per pair"),
		"  → "+theme.Item.Render("Optional pcap-tap mode for one selected pair (privacy-gated)"),
		"  → "+theme.Item.Render("Ctrl+E export current snapshot as CSV"),
	)

	derpHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󰒍  DERP SERVER STATUS (v0.14.x)"),
		"",
		"  "+theme.Item.Render("Health and latency profile of every DERP relay your tailnet uses."),
		"",
		"  "+theme.AccentHiS.Render("Planned:"),
		"  → "+theme.Item.Render("Live `tailscale netcheck` parse: nearest DERP, region, RTT, loss"),
		"  → "+theme.Item.Render("Per-region grid: status, median RTT, last observed"),
		"  → "+theme.Item.Render("DERP fallback rate across mesh — how many sessions are relayed?"),
		"  → "+theme.Item.Render("Historical strip-chart: nearest-DERP RTT over the last hour"),
		"  → "+theme.Item.Render("Drill into one region to see all peers routing through it"),
		"  → "+theme.Item.Render("Custom-DERP detection (self-hosted relays)"),
	)

	ctsHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󰴽  CONNECTION TYPE SUMMARY (v0.14.x)"),
		"",
		"  "+theme.Item.Render("Per-peer breakdown of how your tailnet sessions are actually routed."),
		"",
		"  "+theme.AccentHiS.Render("Planned:"),
		"  → "+theme.Item.Render("Per-peer table: direct UDP, direct IPv6, DERP-relayed, offline"),
		"  → "+theme.Item.Render("NAT type detection: open / restricted-cone / symmetric / port-restricted"),
		"  → "+theme.Item.Render("UPnP / PCP / NAT-PMP detection on local network"),
		"  → "+theme.Item.Render("Hairpinning status (router loops back to WAN IP?)"),
		"  → "+theme.Item.Render("IPv6 availability + Tailscale4via6 routing where applicable"),
		"  → "+theme.Item.Render("Per-peer last-handshake age + key rotation status"),
	)

	schedHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󰃭  TS-CLI SCHEDULER (v0.14.x)"),
		"",
		"  "+theme.Item.Render("Cron-style scheduling for tailscale-cli + taildrives actions across the mesh."),
		"",
		"  "+theme.AccentHiS.Render("Planned:"),
		"  → "+theme.Item.Render("Per-device schedule sheets — schedule any taildrives / tailscale command"),
		"  → "+theme.Item.Render("Visual cron-editor: presets (hourly/daily/weekly) + raw cron mode"),
		"  → "+theme.Item.Render("Pre-baked workflows: nightly share sweep · weekly key rotation · ..."),
		"  → "+theme.Item.Render("Run-history viewer per job, color-coded pass/fail/skip"),
		"  → "+theme.Item.Render("Backed by NixOS-managed systemd timers — survives restarts"),
	)

	watchHelp := lipgloss.JoinVertical(lipgloss.Left,
		advSectionTitle("󰈈  TS-CLI WATCHER (v0.14.x)"),
		"",
		"  "+theme.Item.Render("Live tailnet event stream — what changed, when, and on which device."),
		"",
		"  "+theme.AccentHiS.Render("Planned:"),
		"  → "+theme.Item.Render("Live stream: device online/offline, key rotation, ACL push"),
		"  → "+theme.Item.Render("Filter sidebar: by device, by event type, by severity"),
		"  → "+theme.Item.Render("Pinnable conditions: alert if device X drops off for >N minutes"),
		"  → "+theme.Item.Render("Compact log lines + drill-in detail view for full event payload"),
		"  → "+theme.Item.Render("Hook integration: trigger Pushover / Telegram / webhook on matches"),
		"  → "+theme.Item.Render("Replay buffer (last 1000 events) viewable on reconnect"),
	)

	// Compose body per active tab.
	var body string
	switch h.tab {
	case 1:
		body = lipgloss.JoinVertical(lipgloss.Left,
			header,
			"",
			advTagline,
			"",
			wrap(netmapHelp),
			"",
			wrap(dtmHelp),
			"",
			wrap(derpHelp),
			"",
			wrap(ctsHelp),
			"",
			wrap(schedHelp),
			"",
			wrap(watchHelp),
		)
	default:
		body = lipgloss.JoinVertical(lipgloss.Left,
			header,
			"",
			wrap(basics),
			"",
			wrap(search),
			"",
			wrap(actions),
			"",
			wrap(reports),
			"",
			wrap(global),
			"",
			wrap(operation),
			"",
			wrap(commands),
		)
	}

	// Scrollable: split body into lines, slice to scroll window. Scroll
	// position is per-tab so flipping tabs doesn't lose your place.
	bodyLines := strings.Split(body, "\n")
	total := len(bodyLines)
	contentH := bh - 4 // header + footer + spacers
	maxScroll := total - contentH
	if maxScroll < 0 {
		maxScroll = 0
	}
	if h.scrolls[h.tab] > maxScroll {
		h.scrolls[h.tab] = maxScroll
	}
	scroll := h.scrolls[h.tab]
	end := scroll + contentH
	if end > total {
		end = total
	}
	visible := bodyLines[scroll:end]

	scrollInfo := ""
	if maxScroll > 0 {
		scrollInfo = "  " + theme.ItemDim.Render(
			itoa(scroll+1)+"-"+itoa(end)+" / "+itoa(total))
	}

	hint := theme.ItemDim.Render(
		"  Tab switch help tab · ↑↓ / jk scroll · PgUp/PgDn jump · g/G top/bot · Esc/Enter/q close")

	final := lipgloss.JoinVertical(lipgloss.Left,
		scrollInfo,
		strings.Join(visible, "\n"),
		"",
		hint,
	)

	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(accent).
		Padding(1, 2).
		Width(bw).
		Render(final)
}

// NewHelpOverlayLegacy is the old reportOverlay-based help — kept around
// momentarily so the diff is contained; not exported once the new path
// proves stable.
func newHelpOverlayLegacy() Overlay {
	// Pieces are joined as one big rendered string so the inner styles
	// survive the reportOverlay's line-wrapping logic. Section headers
	// use bold accent; commands use the same dot-leader pattern the file
	// browser uses for detail rows, so the visual language is consistent.
	const repoURL = "https://github.com/archn3m3sis/taildrives"
	const tailscaleDocsURL = "https://tailscale.com/kb/1369/tailscale-drive"

	githubIcon := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#ffffff")).Bold(true).
		Render("") // nf-fa-github

	tagline := lipgloss.NewStyle().Foreground(theme.AccentHi).Italic(true).
		Render("In-house Bubble Tea TUI + CLI for managing Tailscale Drive shares across the n3m mesh.")

	section := func(s string) string {
		return "\n" + lipgloss.NewStyle().
			Foreground(theme.Yellow).Bold(true).
			Render("── "+s+" ──") + "\n"
	}
	cmd := func(name, desc string) string {
		return "  " + lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).Render(name) +
			"   " + theme.Item.Render(desc)
	}
	key := func(name, desc string) string {
		return "  " + lipgloss.NewStyle().Foreground(theme.Green).Bold(true).Render(name) +
			"   " + theme.Item.Render(desc)
	}

	body := strings.Join([]string{
		tagline,
		"",
		"  " + githubIcon + "  " +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).Underline(true).Render(repoURL),
		"  " + theme.ItemDim.Render("Upstream Tailscale Drive docs:") + "  " +
			lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).Underline(true).Render(tailscaleDocsURL),

		section("TUI BASICS"),
		key("↑↓ / jk    ", "move within active pane"),
		key("← → / hl   ", "back / drill into folder or pane"),
		key("g / G      ", "jump to top / bottom"),
		key("PgUp/PgDn  ", "page up / page down"),
		key("Tab        ", "cycle panes (Categories → Devices → Files)"),
		key("Enter      ", "select / drill in"),
		key("Backspace  ", "parent dir / previous pane"),

		section("SEARCH & FILTER"),
		key("s or /     ", "fuzzy search across the current view (live filter)"),
		key("Ctrl+D     ", "(in search) toggle directories-only mode"),
		key("t          ", "category filter (greys out non-matching shares)"),

		section("ACTIONS"),
		key("d          ", "download highlighted file or directory"),
		key("u          ", "upload a local file into the current directory"),
		key("c          ", "ARM copy → navigate → press c again to drop (Esc cancels)"),
		key("b          ", "bulk-send marked files to one or more devices"),
		key("Space      ", "mark/unmark · Ctrl+A marks all · Ctrl+X clears marks"),
		key("P          ", "split-pane preview (text / image via Kitty graphics)"),
		key("r / F5     ", "refresh current directory"),
		key("m          ", "show davfs2 mount command for the current path"),
		key("D          ", "delete (capital D, God Mode gated, irreversible)"),

		section("GLOBAL"),
		key("?          ", "this help menu"),
		key("Esc Esc    ", "double-tap to exit with the outro animation"),
		key("q · Ctrl+C ", "quit immediately"),

		section("UNDERLYING TAILSCALE DRIVE COMMANDS"),
		theme.ItemDim.Render("  These are what `taildrives` invokes against the local tailscale daemon."),
		theme.ItemDim.Render("  Useful to know if you ever want to drop down to raw CLI."),
		"",
		cmd("tailscale drive list                ", "list shares published by this device"),
		cmd("tailscale drive share <name> <path> ", "publish <path> as a Drive share named <name>"),
		cmd("tailscale drive unshare <name>      ", "remove a share (does NOT delete the underlying data)"),
		cmd("tailscale drive set <name> <opts>   ", "tune share options (Tailscale 1.96+)"),
		"",
		theme.ItemDim.Render("  Share names must match [a-z0-9_] only — no hyphens, no dots, max 24 chars."),
		theme.ItemDim.Render("  The Add wizard auto-sanitizes; you can also type a name yourself."),

		section("TAILDRIVES CLI SUBCOMMANDS"),
		theme.ItemDim.Render("  Headless equivalents — invoke without launching the TUI."),
		"",
		cmd("taildrives list                     ", "list every share on every reachable device"),
		cmd("taildrives types                    ", "list category-filter labels and what they match"),
		cmd("taildrives ls PATH                  ", "list a WebDAV path (e.g. /<user>/<device>/<share>/)"),
		cmd("taildrives cat PATH                 ", "print a file's contents to stdout"),
		cmd("taildrives get SRC [DST]            ", "download (default DST: ~/Downloads/basename)"),
		cmd("taildrives put SRC DST              ", "upload local SRC to WebDAV DST"),
		cmd("taildrives copy SRC DST             ", "copy (server-side when same share, else stream)"),
		cmd("taildrives bulk-send SRC DEV1 ...   ", "fan-out SRC to <dev>_taildrops on each DEV"),
		cmd("taildrives mount [DIR]              ", "print davfs2 mount command for DIR"),
		cmd("taildrives serve                    ", "print the WebDAV endpoint URL"),
		cmd("taildrives devices                  ", "list device names visible in your tailnet"),
		cmd("taildrives version                  ", "print build version"),

		section("BASIC OPERATION"),
		"  " + theme.Item.Render("1. ") + theme.ItemDim.Render("From the splash menu, pick ENTER TAILDRIVES-CLI to open the main TUI."),
		"  " + theme.Item.Render("2. ") + theme.ItemDim.Render("Left pane is Categories (filters), middle is Devices, right is Files."),
		"  " + theme.Item.Render("3. ") + theme.ItemDim.Render("Tab between panes; ↑↓ to highlight; Enter to drill in."),
		"  " + theme.Item.Render("4. ") + theme.ItemDim.Render("Press s to fuzzy-search the current view; Ctrl+D narrows to dirs only."),
		"  " + theme.Item.Render("5. ") + theme.ItemDim.Render("To share a folder: splash menu → ADD TAILDRIVE SHARES → walk the wizard."),
		"  " + theme.Item.Render("6. ") + theme.ItemDim.Render("To remove a share: splash menu → REMOVE TAILDRIVE SHARES."),
		"  " + theme.Item.Render("7. ") + theme.ItemDim.Render("Use the inline file browser's . to toggle hidden files; ↗ marks symlinks."),
		"  " + theme.Item.Render("8. ") + theme.ItemDim.Render("[SHARED] tag on a directory means it's already a registered Taildrive."),
		"  " + theme.Item.Render("9. ") + theme.ItemDim.Render("Double-tap Esc anywhere to exit with the farewell animation."),
	}, "\n")

	return newReport("TAILDRIVES — HELP", body, theme.Magenta)
}

// NewDeviceStatusReport renders the device-status report into a buffer
// then wraps it in a scrollable popup.
func NewDeviceStatusReport(actor string) Overlay {
	var buf bytes.Buffer
	actions.WriteDeviceStatusReport(&buf, actor)
	return newReport("TAILSCALE DEVICE STATUS REPORT", buf.String(), theme.Accent)
}

// NewJournalViewer wraps the lifecycle journal in a journalOverlay that
// intercepts Ctrl+C / Ctrl+P to show a date-range picker first — the
// journal is a HISTORICAL stream so "copy this" really means "copy what
// time window?". The default visible view is the last 7 days, but the
// picker lets the operator scope the copy/print to a different range
// before the action fires.
func NewJournalViewer(actor string) Overlay {
	var buf bytes.Buffer
	actions.WriteJournalReport(&buf, 7, 100)
	_ = actor
	return newJournalOverlay(buf.String())
}

// ── Lifecycle journal overlay with date-range picker on copy/print ────

// journalPreset is one option in the date-range picker shown before a
// copy/print fires. Days=0 means "all time" (capped by the journal store
// itself, not by our reader).
type journalPreset struct {
	Label string
	Days  int
}

var journalPresets = []journalPreset{
	{"Last 24 hours", 1},
	{"Last 7 days (current view)", 7},
	{"Last 30 days", 30},
	{"Last 90 days", 90},
	{"All time", 3650}, // ~10 years — effectively unlimited
}

type journalOverlay struct {
	report *reportOverlay

	// Picker state. When pickerAction is non-empty, the picker is open
	// and the View renders it on top of the journal report. Action is
	// either "copy" or "print" — determines which cmd fires after the
	// operator selects a range.
	pickerAction string
	pickerIdx    int
}

func newJournalOverlay(content string) *journalOverlay {
	return &journalOverlay{
		report:    newReport("TAILDRIVES LIFECYCLE JOURNAL", content, theme.Magenta),
		pickerIdx: 1, // Last 7 days default
	}
}

func (j *journalOverlay) Init() tea.Cmd { return nil }

func (j *journalOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	// Picker mode owns key handling while open.
	if j.pickerAction != "" {
		if km, ok := msg.(tea.KeyMsg); ok {
			switch km.String() {
			case "esc":
				j.pickerAction = ""
				return j, nil, false
			case "up", "k":
				if j.pickerIdx > 0 {
					j.pickerIdx--
				}
				return j, nil, false
			case "down", "j":
				if j.pickerIdx < len(journalPresets)-1 {
					j.pickerIdx++
				}
				return j, nil, false
			case "enter":
				// Pull a fresh journal slice for the chosen range, then
				// fire the copy or print Cmd against that content.
				preset := journalPresets[j.pickerIdx]
				var buf bytes.Buffer
				actions.WriteJournalReport(&buf, preset.Days, 10000)
				action := j.pickerAction
				j.pickerAction = ""
				switch action {
				case "copy":
					j.report.justCopied = true
					return j, copyToClipboardCmd(buf.String()), false
				case "print":
					j.report.printNotifyAt = time.Now().Add(5 * time.Second)
					return j, tea.Batch(
						printReportCmd(buf.String(), "TAILDRIVES JOURNAL "+preset.Label),
						tea.Tick(5*time.Second, func(time.Time) tea.Msg {
							return printDismissMsg{}
						}),
					), false
				}
			}
			return j, nil, false
		}
		return j, nil, false
	}

	// Not in picker mode. Intercept Ctrl+C / Ctrl+P to open the picker
	// FIRST instead of acting immediately on visible content. Anything
	// else delegates to the embedded reportOverlay (scroll, esc, etc.).
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "ctrl+c":
			j.pickerAction = "copy"
			return j, nil, false
		case "ctrl+p":
			j.pickerAction = "print"
			return j, nil, false
		}
	}
	ov, cmd, done := j.report.Update(msg)
	if rep, ok := ov.(*reportOverlay); ok {
		j.report = rep
	}
	return j, cmd, done
}

func (j *journalOverlay) View(w, h int) string {
	if j.pickerAction != "" {
		return j.renderPicker(w, h)
	}
	return j.report.View(w, h)
}

func (j *journalOverlay) renderPicker(w, h int) string {
	verb := "copy"
	icon := "" // nf-fa-clipboard
	if j.pickerAction == "print" {
		verb = "print"
		icon = "" // nf-fa-print
	}
	title := lipgloss.NewStyle().
		Foreground(theme.Magenta).Bold(true).Reverse(true).Padding(0, 2).
		Render(" " + icon + "  SELECT DATE RANGE TO " + strings.ToUpper(verb) + " ")

	var lines []string
	for i, p := range journalPresets {
		cur := "  "
		labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
		if i == j.pickerIdx {
			cur = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true).Render("▸ ")
			labelStyle = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		lines = append(lines, "  "+cur+labelStyle.Render(p.Label))
	}

	hint := theme.ItemDim.Render(
		"  ↑↓ select · Enter " + icon + " " + verb + " · Esc cancel")

	body := lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		"  "+theme.ItemDim.Render("Pick a time window. The journal will be re-extracted"),
		"  "+theme.ItemDim.Render("for that range, then "+verb+"ed."),
		"",
		strings.Join(lines, "\n"),
		"",
		hint,
	)

	bw := w * 6 / 10
	if bw < 60 {
		bw = 60
	}
	if bw > 100 {
		bw = 100
	}
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Magenta).
		Padding(1, 3).
		Width(bw).
		Render(body)
}

// NewPerformanceReport returns an overlay that fires the slow ping work
// asynchronously and updates the popup as progress arrives.
func NewPerformanceReport(actor string) Overlay {
	return newPerfOverlay(actor)
}

// NewAddShares + NewRemoveShares — stubs return a placeholder report
// until the multi-step wizards land in the next iteration.
func NewAddShares(actor string) Overlay {
	body := theme.Title.Render("ADD TAILDRIVE SHARES") + "\n\n" +
		theme.ItemDim.Render("The full multi-step picker + path-browser wizard is being built next.\n\n"+
			"For the immediate path, this overlay is a placeholder so the menu wiring is\n"+
			"end-to-end testable. The AddShare flow will land in the next deploy with:\n\n"+
			"  • category picker (workstation / server / service)\n"+
			"  • device picker (filtered to the chosen category)\n"+
			"  • filesystem browser on the chosen device (no path typing)\n"+
			"  • share name input + confirmation\n"+
			"  • runs tailscale drive share, logs to journal\n\n"+
			"All in the TUI overlay — no terminal exit.")
	_ = actor
	return newReport("ADD TAILDRIVE SHARES — placeholder", body, theme.Yellow)
}

// NewRemoveShares — placeholder until the multi-step picker overlay lands.
func NewRemoveShares(actor string) Overlay {
	body := theme.Title.Render("REMOVE TAILDRIVE SHARES") + "\n\n" +
		theme.ItemDim.Render("The full multi-step + multi-select wizard is being built next.\n\n"+
			"Pipeline:\n\n"+
			"  • category picker (workstation / server / service)\n"+
			"  • device picker (filtered to chosen category, only devices with shares)\n"+
			"  • share multi-picker (Space toggles, Enter commits)\n"+
			"  • YES-to-confirm prompt\n"+
			"  • runs tailscale drive unshare on each, logs to journal\n\n"+
			"All in the TUI overlay — no terminal exit.")
	_ = actor
	return newReport("REMOVE TAILDRIVE SHARES — placeholder", body, theme.Red)
}

// ── Performance overlay — async ping with live progress ───────────────

type perfOverlay struct {
	actor         string
	progress      actions.Progress
	content       string
	done          bool
	scroll        int
	justCopied    bool      // Ctrl+C bottom-bar feedback
	printNotifyAt time.Time // when set + in future, show centered popup
}

type perfProgressMsg actions.Progress
type perfDoneMsg struct{ content string }

func newPerfOverlay(actor string) *perfOverlay {
	return &perfOverlay{actor: actor}
}

func (p *perfOverlay) Init() tea.Cmd {
	prog := make(chan actions.Progress, 8)
	resultCh := make(chan string, 1)
	// Producer goroutine: run the report, push progress, push final content.
	go func() {
		var buf bytes.Buffer
		actions.WritePerformanceReport(&buf, p.actor, func(pr actions.Progress) {
			select {
			case prog <- pr:
			default:
			}
		})
		close(prog)
		resultCh <- buf.String()
	}()
	// Two Bubble Tea commands: one polls progress, one waits for result.
	return tea.Batch(
		waitProgress(prog),
		waitResult(resultCh),
	)
}

func waitProgress(ch <-chan actions.Progress) tea.Cmd {
	return func() tea.Msg {
		p, ok := <-ch
		if !ok {
			return nil
		}
		return perfProgressMsg(p)
	}
}
func waitResult(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		return perfDoneMsg{content: <-ch}
	}
}

func (p *perfOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if _, ok := msg.(printDismissMsg); ok {
		p.printNotifyAt = time.Time{}
		return p, nil, false
	}
	switch m := msg.(type) {
	case perfProgressMsg:
		p.progress = actions.Progress(m)
		// keep polling
		return p, nil, false
	case perfDoneMsg:
		p.content = m.content
		p.done = true
		return p, nil, false
	case tea.KeyMsg:
		if !p.done {
			// allow esc/q to abort the wait
			switch m.String() {
			case "esc", "q":
				return p, nil, true
			}
			return p, nil, false
		}
		// done: report-viewer key handling
		p.justCopied = false
		switch {
		case key.Matches(m, key.NewBinding(key.WithKeys("ctrl+c"))):
			p.justCopied = true
			return p, copyToClipboardCmd(p.content), false
		case key.Matches(m, key.NewBinding(key.WithKeys("ctrl+p"))):
			p.printNotifyAt = time.Now().Add(5 * time.Second)
			return p, tea.Batch(
				printReportCmd(p.content, "PERFORMANCE REPORT"),
				tea.Tick(5*time.Second, func(time.Time) tea.Msg {
					return printDismissMsg{}
				}),
			), false
		case key.Matches(m, key.NewBinding(key.WithKeys("q", "esc", "enter"))):
			return p, nil, true
		case key.Matches(m, key.NewBinding(key.WithKeys("up", "k"))):
			if p.scroll > 0 {
				p.scroll--
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("down", "j"))):
			p.scroll++
		case key.Matches(m, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
			p.scroll -= 10
			if p.scroll < 0 {
				p.scroll = 0
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("pgdown", "ctrl+d", " "))):
			p.scroll += 10
		}
	}
	return p, nil, false
}

func (p *perfOverlay) View(w, h int) string {
	if !p.done {
		// Render a progress popup
		title := lipgloss.NewStyle().
			Foreground(theme.Yellow).Bold(true).Reverse(true).
			Padding(0, 2).Render(" TAILSCALE PERFORMANCE REPORT — gathering ")
		stage := p.progress.Stage
		if stage == "" {
			stage = "initializing…"
		}
		bar := renderProgressBar(p.progress.Done, p.progress.Total, 50)
		body := lipgloss.JoinVertical(lipgloss.Left,
			title,
			"",
			"  Stage: "+theme.Item.Render(stage),
			"",
			"  "+bar,
			"",
			theme.ItemDim.Render("  Press Esc to abort"))
		return lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(theme.Yellow).
			Padding(1, 3).
			Width(80).
			Render(body)
	}
	// Render the finished report in the standard scroll viewer.
	r := newReport("TAILSCALE PERFORMANCE REPORT", p.content, theme.Yellow)
	r.scroll = p.scroll
	r.justCopied = p.justCopied
	r.printNotifyAt = p.printNotifyAt
	return r.View(w, h)
}

func renderProgressBar(done, total, width int) string {
	if total == 0 {
		return theme.ItemDim.Render("  (no work scheduled)")
	}
	filled := done * width / total
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	pct := 100 * done / total
	return lipgloss.NewStyle().Foreground(theme.Accent).Render(bar) +
		" " + theme.ItemDim.Render(itoa(done)+"/"+itoa(total)+" ("+itoa(pct)+"%)")
}

// ── tiny utilities ─────────────────────────────────────────────────────

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}

// Used to silence the linter if some funcs become unused during iteration.
var _ = local.HostName
