// Package splash is the animated boot screen + menu for the taildrives
// TUI. After the intro animation it hosts the main menu and the global-
// actions menu (Tab to toggle). Selecting a global action opens an
// overlay wizard IN PROCESS — the user never drops to the raw terminal.
package splash

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/overlay"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

// ── Phase machine ──────────────────────────────────────────────────────────

type Phase int

const (
	PhaseLogo Phase = iota
	PhaseWelcome
	PhasePause1
	PhaseUntype
	PhaseCredit
	PhasePause2
	PhaseMenu
	PhaseDone
)

// Action is what the user picked at the menu when the program ends.
type Action int

const (
	ActionNone Action = iota
	ActionEnter
	ActionHelp
	ActionGitHub
	ActionFeatureRequest
	ActionIssues
	ActionQuit

	// Global Actions — handled IN-PROCESS via overlays now, not returned.
	ActionRemoveShares
	ActionAddShares
	ActionDeviceStatusReport
	ActionPerformanceReport
	ActionLifecycleJournal
)

// Overlay re-exports the shared overlay.Overlay so splash callers can
// reference it as splash.Overlay (handy when wiring the factory).
type Overlay = overlay.Overlay

// OverlayFactory creates a fresh overlay for a given Action.
type OverlayFactory func(Action) Overlay

// ── Strings & art ──────────────────────────────────────────────────────────

const (
	welcomeText = "Welcome to Taildrives-CLI Management Console"
	creditText  = "Developed by @Archn3m3sis"
)

var logoDotOrder = [9][2]int{
	{1, 1}, {0, 1}, {1, 2}, {2, 1}, {1, 0},
	{0, 0}, {0, 2}, {2, 2}, {2, 0},
}

func renderLogo(n int) string {
	rowColors := []lipgloss.Color{theme.Accent, theme.Purple, theme.Magenta}
	lit := map[[2]int]bool{}
	for i := 0; i < n && i < len(logoDotOrder); i++ {
		lit[logoDotOrder[i]] = true
	}
	var lines []string
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
			lines = append(lines, b.String())
		}
		if r < 2 {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

// ── Menu items ───────────────────────────────────────────────────────────

type menuItem struct {
	Number int
	Label  string
	Action Action
}

var menu = []menuItem{
	{1, "ENTER TAILDRIVES-CLI", ActionEnter},
	{2, "TAILDRIVES-CLI HELP", ActionHelp},
	{3, "TAILDRIVES-CLI GITHUB", ActionGitHub},
	{4, "TAILDRIVES-CLI FEATURE REQUEST", ActionFeatureRequest},
	{5, "TAILDRIVES-CLI ISSUES", ActionIssues},
}

var globalActions = []menuItem{
	{1, "REMOVE TAILDRIVE SHARES", ActionRemoveShares},
	{2, "ADD TAILDRIVE SHARES", ActionAddShares},
	{3, "TAILSCALE DEVICE STATUS REPORT", ActionDeviceStatusReport},
	{4, "TAILSCALE PERFORMANCE REPORT", ActionPerformanceReport},
	{5, "TAILDRIVES LIFECYCLE JOURNAL", ActionLifecycleJournal},
}

// ── Model ──────────────────────────────────────────────────────────────────

type Model struct {
	Phase     Phase
	Action    Action
	W, H      int
	frame     int
	typed     string
	menuIdx   int
	globalIdx int
	onGlobal  bool
	keys      keymap
	skipped   bool
	finalQuit bool

	// Overlay state — when non-nil, all key events route to overlay and
	// View renders the popup on top of the menu.
	factory OverlayFactory
	overlay Overlay

	// Double-Esc tracking: a single Esc closes overlays / cancels the
	// active in-flight action; two Escs within doubleEscWindow trigger
	// the outro animation + clean exit. The Action is set to ActionQuit
	// and OutroRequested flips true so main.go knows to play the outro.
	lastEsc         time.Time
	OutroRequested  bool
}

type keymap struct {
	Up, Down, Enter, Quit, Skip, Number, Tab key.Binding
}

func newKeymap() keymap {
	return keymap{
		Up:     key.NewBinding(key.WithKeys("up", "k")),
		Down:   key.NewBinding(key.WithKeys("down", "j")),
		Enter:  key.NewBinding(key.WithKeys("enter")),
		Quit:   key.NewBinding(key.WithKeys("q", "ctrl+c")),
		Skip:   key.NewBinding(key.WithKeys("s")),
		Number: key.NewBinding(key.WithKeys("1", "2", "3", "4", "5")),
		Tab:    key.NewBinding(key.WithKeys("tab")),
	}
}

// NewWithFactory constructs the splash model and registers a factory that
// creates Overlay instances for global actions.
func NewWithFactory(f OverlayFactory) Model {
	return Model{Phase: PhaseLogo, keys: newKeymap(), factory: f}
}

// New is the no-overlay constructor (back-compat for the bare splash).
func New() Model { return NewWithFactory(nil) }

func (m Model) Init() tea.Cmd { return tickFast() }

type tickMsg time.Time

func tickFast() tea.Cmd {
	return tea.Tick(30*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func tickSlow() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ── Update ─────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Pre-overlay: double-Esc anywhere → outro exit. Must run BEFORE the
	// overlay router so an active wizard's own Esc handler doesn't swallow
	// the second press. doubleEscWindow is the max gap between the two
	// presses; 500ms is fast enough to be deliberate, slow enough to be
	// reachable on a normal keyboard.
	const doubleEscWindow = 500 * time.Millisecond
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		if !m.lastEsc.IsZero() && time.Since(m.lastEsc) <= doubleEscWindow {
			m.OutroRequested = true
			m.finalQuit = true
			m.Action = ActionQuit
			m.Phase = PhaseDone
			return m, tea.Quit
		}
		m.lastEsc = time.Now()
		// Don't return here — let single-Esc fall through to the overlay
		// or pane handler so it keeps its existing meaning (close popup,
		// cancel input, etc.).
	}

	// 1. If an overlay is active, route to it first.
	if m.overlay != nil {
		ov, cmd, done := m.overlay.Update(msg)
		m.overlay = ov
		if done {
			m.overlay = nil
		}
		return m, cmd
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.W, m.H = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m.advance()

	case tea.KeyMsg:
		if m.Phase != PhaseMenu {
			if key.Matches(msg, m.keys.Quit) {
				m.finalQuit = true
				m.Action = ActionQuit
				m.Phase = PhaseDone
				return m, tea.Quit
			}
			m.Phase = PhaseMenu
			m.skipped = true
			return m, nil
		}
		// Menu navigation
		active := menu
		idx := &m.menuIdx
		if m.onGlobal {
			active = globalActions
			idx = &m.globalIdx
		}
		switch {
		case key.Matches(msg, m.keys.Tab):
			m.onGlobal = !m.onGlobal
		case key.Matches(msg, m.keys.Up):
			if *idx > 0 {
				*idx--
			}
		case key.Matches(msg, m.keys.Down):
			if *idx < len(active)-1 {
				*idx++
			}
		case key.Matches(msg, m.keys.Number):
			n := int(msg.String()[0] - '0')
			if n >= 1 && n <= len(active) {
				*idx = n - 1
				return m.selectAction(active[*idx].Action)
			}
		case key.Matches(msg, m.keys.Enter):
			return m.selectAction(active[*idx].Action)
		case key.Matches(msg, m.keys.Quit):
			m.finalQuit = true
			m.Action = ActionQuit
			m.Phase = PhaseDone
			return m, tea.Quit
		}
	}
	return m, nil
}

// selectAction decides whether to launch an in-process overlay (global
// actions handled by the factory) or to exit splash so the host can
// dispatch externally (browser/help/ENTER).
func (m Model) selectAction(a Action) (tea.Model, tea.Cmd) {
	if m.factory != nil {
		switch a {
		case ActionRemoveShares, ActionAddShares,
			ActionDeviceStatusReport, ActionPerformanceReport,
			ActionLifecycleJournal:
			m.overlay = m.factory(a)
			if m.overlay != nil {
				return m, m.overlay.Init()
			}
		}
	}
	m.Action = a
	m.Phase = PhaseDone
	return m, tea.Quit
}

func (m Model) advance() (tea.Model, tea.Cmd) {
	switch m.Phase {
	case PhaseLogo:
		m.frame++
		if m.frame >= len(logoDotOrder)+2 {
			m.Phase = PhaseWelcome
			m.frame = 0
			m.typed = ""
			return m, tickFast()
		}
		return m, tickSlow()
	case PhaseWelcome:
		if len(m.typed) < len(welcomeText) {
			m.typed = welcomeText[:len(m.typed)+1]
			return m, tickFast()
		}
		m.Phase = PhasePause1
		m.frame = 0
		return m, tickSlow()
	case PhasePause1:
		m.frame++
		if m.frame >= 6 {
			m.Phase = PhaseUntype
			return m, tickFast()
		}
		return m, tickSlow()
	case PhaseUntype:
		if len(m.typed) > 0 {
			m.typed = m.typed[:len(m.typed)-1]
			return m, tickFast()
		}
		m.Phase = PhaseCredit
		return m, tickFast()
	case PhaseCredit:
		if len(m.typed) < len(creditText) {
			m.typed = creditText[:len(m.typed)+1]
			return m, tickFast()
		}
		m.Phase = PhasePause2
		m.frame = 0
		return m, tickSlow()
	case PhasePause2:
		m.frame++
		if m.frame >= 4 {
			m.Phase = PhaseMenu
			return m, nil
		}
		return m, tickSlow()
	}
	return m, nil
}

// ── View ───────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.W == 0 || m.H == 0 {
		return ""
	}
	logoLit := len(logoDotOrder)
	if m.Phase == PhaseLogo {
		logoLit = m.frame
	}
	logo := renderLogo(logoLit)

	wordmark := lipgloss.NewStyle().
		Foreground(theme.AccentHi).
		Bold(true).
		Render("T A I L D R I V E S")

	caret := lipgloss.NewStyle().Foreground(theme.Magenta).Bold(true).Render("▌")
	var typedLine string
	switch m.Phase {
	case PhaseLogo:
		typedLine = ""
	case PhaseWelcome, PhasePause1, PhaseUntype:
		style := lipgloss.NewStyle().Foreground(theme.Text).Bold(true)
		typedLine = style.Render(m.typed) + caret
	case PhaseCredit, PhasePause2:
		style := lipgloss.NewStyle().Foreground(theme.Pink).Bold(true).Italic(true)
		typedLine = style.Render(m.typed) + caret
	case PhaseMenu, PhaseDone:
		style := lipgloss.NewStyle().Foreground(theme.Pink).Bold(true).Italic(true)
		typedLine = style.Render(creditText)
	}

	parts := []string{logo, "", wordmark, "", typedLine}
	if m.Phase == PhaseMenu {
		parts = append(parts, "", m.renderMenu())
	}
	body := lipgloss.JoinVertical(lipgloss.Center, parts...)
	canvas := lipgloss.NewStyle().
		Width(m.W).
		Height(m.H).
		Align(lipgloss.Center, lipgloss.Center).
		Render(body)

	// Overlay on top
	if m.overlay != nil {
		ov := m.overlay.View(m.W, m.H)
		return overlayOnTop(canvas, ov, m.W, m.H)
	}
	return canvas
}

func (m Model) renderMenu() string {
	active := menu
	idx := m.menuIdx
	accent := theme.AccentHi
	border := theme.PurpleDeep
	tabLabel := "Tab → Global Actions"
	heading := "MAIN MENU"
	if m.onGlobal {
		active = globalActions
		idx = m.globalIdx
		accent = theme.Yellow
		border = theme.Yellow
		tabLabel = "Tab → Main Menu"
		heading = "GLOBAL ACTIONS"
	}

	var lines []string
	for i, it := range active {
		cursor := "   "
		labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
		numStyle := lipgloss.NewStyle().Foreground(theme.TextMuted)
		if i == idx {
			cursor = lipgloss.NewStyle().Foreground(accent).Bold(true).Render(" ▸ ")
			labelStyle = lipgloss.NewStyle().Foreground(accent).Bold(true).Underline(true)
			numStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
		}
		line := fmt.Sprintf("%s%s  %s",
			cursor,
			numStyle.Render(fmt.Sprintf("%d.", it.Number)),
			labelStyle.Render(it.Label))
		lines = append(lines, line)
	}
	headingLine := lipgloss.NewStyle().
		Foreground(accent).Bold(true).Reverse(true).
		Render("  " + heading + "  ")
	tabHint := lipgloss.NewStyle().
		Foreground(accent).Italic(true).
		Render(tabLabel)
	hint := lipgloss.NewStyle().
		Foreground(theme.TextMuted).Italic(true).
		Render("↑/↓ navigate · 1-5 jump · Enter select · Tab switch menu · q quit")
	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(border).
		Padding(1, 4).
		Render(headingLine + "   " + tabHint + "\n\n" +
			strings.Join(lines, "\n") + "\n\n" + hint)
	return box
}

// overlayOnTop composites `ov` centered onto `underlay`.
func overlayOnTop(underlay, ov string, w, h int) string {
	ow := lipgloss.Width(ov)
	oh := lipgloss.Height(ov)
	x := (w - ow) / 2
	y := (h - oh) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	mainLines := strings.Split(underlay, "\n")
	ovLines := strings.Split(ov, "\n")
	for i, ol := range ovLines {
		row := y + i
		if row >= len(mainLines) {
			break
		}
		mainLines[row] = strings.Repeat(" ", x) + ol
	}
	return strings.Join(mainLines, "\n")
}
