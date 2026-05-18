// Package wizards is the in-TUI overlay set the splash menu launches —
// reports (status/performance/journal) render as scrollable text popups,
// share add/remove run as multi-step Bubble Tea sub-models. Nothing in
// here ever drops back to the raw terminal.
package wizards

import (
	"bytes"
	"strings"

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
}

func newReport(title, content string, color lipgloss.Color) *reportOverlay {
	return &reportOverlay{title: title, color: color, content: content}
}

func (r *reportOverlay) Init() tea.Cmd { return nil }

func (r *reportOverlay) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch {
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
	hint := theme.ItemDim.Render("↑↓/jk scroll · PgUp/PgDn jump · g/G top/bot · Enter/Esc close")
	body := lipgloss.JoinVertical(lipgloss.Left,
		titleBar+scrollInfo,
		"",
		strings.Join(visible, "\n"),
		"",
		hint)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(r.color).
		Padding(1, 2).
		Width(bw).
		Render(body)
}

// ── Public constructors ────────────────────────────────────────────────

// NewDeviceStatusReport renders the device-status report into a buffer
// then wraps it in a scrollable popup.
func NewDeviceStatusReport(actor string) Overlay {
	var buf bytes.Buffer
	actions.WriteDeviceStatusReport(&buf, actor)
	return newReport("TAILSCALE DEVICE STATUS REPORT", buf.String(), theme.Accent)
}

// NewJournalViewer wraps the journal text.
func NewJournalViewer(actor string) Overlay {
	var buf bytes.Buffer
	actions.WriteJournalReport(&buf, 7, 100)
	_ = actor
	return newReport("TAILDRIVES LIFECYCLE JOURNAL", buf.String(), theme.Magenta)
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
	actor    string
	progress actions.Progress
	content  string
	done     bool
	scroll   int
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
		switch {
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
