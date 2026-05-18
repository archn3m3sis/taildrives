// Package outro plays the exit animation triggered by double-Esc anywhere
// in the TUI. Reverses the splash intro: starts with all 9 logo dots lit
// (matching where the splash left off), then unlits them one-per-tick in
// reverse-intro order, finally showing the "THANKS FOR USING TAILDRIVES!"
// farewell line before the program quits.
//
// Why a separate package + separate tea.Program rather than an in-line
// phase in splash/tui: the outro must be triggered from BOTH splash AND
// the main TUI without each having to embed the animation state machine.
// main.go calls Run() on whichever model returns first with the
// OutroRequested flag set, then immediately calls outro.Play() to render
// the farewell — cleaner than threading outro state through every model.
package outro

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/theme"
)

// frameInterval is the per-tick delay during the unlit sequence. 80ms is
// snappy enough that the whole outro reads in <1s, slow enough that the
// eye registers each dot disappearing.
const frameInterval = 80 * time.Millisecond

// holdFinalFrame is how long we hold the farewell message before quitting.
// 900ms lets the operator actually read it without feeling stuck.
const holdFinalFrame = 900 * time.Millisecond

// logoDotOrder MIRRORS internal/splash.logoDotOrder. Duplicated here to
// avoid the outro package depending on splash (which would create a cycle
// once main.go imports both). If the splash intro order changes, change
// this too.
var logoDotOrder = [9][2]int{
	{1, 1}, {0, 1}, {1, 2}, {2, 1}, {1, 0},
	{0, 0}, {0, 2}, {2, 2}, {2, 0},
}

type tickMsg time.Time
type doneMsg struct{}

type Model struct {
	// dotsLit starts at 9 (all lit, matching where splash left off) and
	// decrements one per tick down to 0. After 0, we show the farewell
	// frame for holdFinalFrame, then quit.
	dotsLit  int
	farewell bool

	// Window dimensions — needed to center the logo on the full terminal
	// rather than inside a fixed 80-cell box (which produced the top-left
	// "jenky" placement the operator flagged).
	w, h int
}

func New() Model {
	return Model{dotsLit: 9}
}

func (m Model) Init() tea.Cmd {
	return tick(frameInterval)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = v.Width, v.Height
		return m, nil
	case tea.KeyMsg:
		// Any key skips the outro — operator hammering Esc twice
		// shouldn't have to wait through the animation if they want out.
		_ = v
		return m, tea.Quit
	case tickMsg:
		if !m.farewell {
			if m.dotsLit > 0 {
				m.dotsLit--
				return m, tick(frameInterval)
			}
			// All dots out — switch to the farewell frame and hold.
			m.farewell = true
			return m, tick(holdFinalFrame)
		}
		// Farewell frame was up for holdFinalFrame — exit.
		return m, tea.Quit
	case doneMsg:
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) View() string {
	rowColors := []lipgloss.Color{theme.Accent, theme.Purple, theme.Magenta}
	lit := map[[2]int]bool{}
	for i := 0; i < m.dotsLit; i++ {
		lit[logoDotOrder[i]] = true
	}
	var logoLines []string
	for r := 0; r < 3; r++ {
		for sub := 0; sub < 3; sub++ {
			var b strings.Builder
			for c := 0; c < 3; c++ {
				if c > 0 {
					b.WriteString("  ")
				}
				if lit[[2]int{r, c}] {
					on := lipgloss.NewStyle().Foreground(rowColors[r]).Bold(true)
					b.WriteString(on.Render("████████"))
				} else {
					dim := lipgloss.NewStyle().Foreground(theme.BgSubtle)
					b.WriteString(dim.Render("░░░░░░░░"))
				}
			}
			logoLines = append(logoLines, b.String())
		}
		if r < 2 {
			logoLines = append(logoLines, "")
		}
	}

	var farewell string
	if m.farewell {
		farewell = lipgloss.NewStyle().
			Foreground(theme.Yellow).Bold(true).
			Render("THANKS FOR USING TAILDRIVES!")
	} else {
		// Reserve the same vertical space so the logo doesn't jump when
		// the farewell text appears.
		farewell = " "
	}

	body := lipgloss.JoinVertical(lipgloss.Center,
		strings.Join(logoLines, "\n"),
		"",
		farewell,
	)

	// Center the composition on the FULL terminal — using lipgloss.Place
	// with the live window dims so the logo lands in the middle the same
	// way the splash intro does. Falls back to 80×24 if we haven't yet
	// received a WindowSizeMsg (first paint before init).
	w, h := m.w, m.h
	if w < 40 {
		w = 80
	}
	if h < 10 {
		h = 24
	}
	return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, body)
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Play runs the outro as a self-contained tea.Program. Blocks until the
// animation completes (or user keypress skips it). Called from main.go
// when a sub-program returns with the outro flag set.
//
// WithAltScreen mirrors the splash/main TUI rendering surface, so the
// outro draws ON the same full-screen canvas they were on — without it,
// the outro prints inline at the cursor position, producing the top-left
// "jenky" placement the operator flagged.
func Play() error {
	p := tea.NewProgram(New(), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
