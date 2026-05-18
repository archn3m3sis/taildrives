package wizards

import (
	"fmt"
	"path"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/actions"
	"github.com/archn3m3sis/taildrives/internal/journal"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

// addShareStep enumerates the wizard's state machine.
type addShareStep int

const (
	asPickCategory addShareStep = iota
	asPickDevice
	asPickPath
	asNameInput
	asConfirm
	asRunning
	asResult
)

type addShareWizard struct {
	actor string
	step  addShareStep

	categoryIdx int // 0 = workstations, 1 = servers, 2 = service/all
	deviceIdx   int
	devices     []string

	browser   *fileBrowser
	chosenDir string

	nameInput textinput.Model
	shareName string

	resultMsg string
	resultErr error
}

// NewAddShareWizard returns an overlay that runs the full add-share flow
// without ever leaving the TUI.
func NewAddShareWizard(actor string) Overlay {
	ti := textinput.New()
	ti.Prompt = " › "
	ti.Placeholder = "share-name-with-underscores"
	ti.CharLimit = 64
	return &addShareWizard{actor: actor, step: asPickCategory, nameInput: ti}
}

func (a *addShareWizard) Init() tea.Cmd { return nil }

func (a *addShareWizard) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok {
		// Esc always cancels (unless inside file browser which has its own esc handling)
		if a.step != asPickPath && km.String() == "esc" {
			return a, nil, true
		}
	}
	switch a.step {
	case asPickCategory:
		return a.updateCategoryPick(msg)
	case asPickDevice:
		return a.updateDevicePick(msg)
	case asPickPath:
		return a.updatePathPick(msg)
	case asNameInput:
		return a.updateNameInput(msg)
	case asConfirm:
		return a.updateConfirm(msg)
	case asRunning:
		return a.updateRunning(msg)
	case asResult:
		if km, ok := msg.(tea.KeyMsg); ok {
			switch km.String() {
			case "enter", "esc", "q":
				return a, nil, true
			}
		}
	}
	return a, nil, false
}

func (a *addShareWizard) updateCategoryPick(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return a, nil, false
	}
	switch {
	case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
		if a.categoryIdx > 0 {
			a.categoryIdx--
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
		if a.categoryIdx < 2 {
			a.categoryIdx++
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("1"))):
		a.categoryIdx = 0
	case key.Matches(km, key.NewBinding(key.WithKeys("2"))):
		a.categoryIdx = 1
	case key.Matches(km, key.NewBinding(key.WithKeys("3"))):
		a.categoryIdx = 2
	case key.Matches(km, key.NewBinding(key.WithKeys("enter"))):
		a.devices = a.devicesForCategory()
		if len(a.devices) == 0 {
			a.resultErr = fmt.Errorf("no devices in this category — bring some online first")
			a.step = asResult
			return a, nil, false
		}
		a.deviceIdx = 0
		a.step = asPickDevice
	}
	return a, nil, false
}

func (a *addShareWizard) devicesForCategory() []string {
	switch a.categoryIdx {
	case 0:
		return actions.CoreWorkstations
	case 1:
		return actions.CoreServers
	default:
		out := append([]string{}, actions.CoreServers...)
		return append(out, actions.CoreWorkstations...)
	}
}

func (a *addShareWizard) updateDevicePick(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return a, nil, false
	}
	switch {
	case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
		if a.deviceIdx > 0 {
			a.deviceIdx--
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
		if a.deviceIdx < len(a.devices)-1 {
			a.deviceIdx++
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("backspace"))):
		a.step = asPickCategory
	case key.Matches(km, key.NewBinding(key.WithKeys("enter"))):
		dev := a.devices[a.deviceIdx]
		a.browser = newFileBrowser(dev, "")
		a.step = asPickPath
		return a, a.browser.start(), false
	}
	return a, nil, false
}

func (a *addShareWizard) updatePathPick(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	// SINGLE delegation to the browser — previous code called update twice
	// per event which caused Down to advance two rows per keypress.
	var cmd tea.Cmd
	a.browser, cmd = a.browser.update(msg)
	if a.browser.done {
		if a.browser.canceled {
			a.step = asPickDevice
			a.browser = nil
			return a, nil, false
		}
		a.chosenDir = a.browser.Selected
		a.shareName = sanitizeShareName(path.Base(a.chosenDir))
		a.nameInput.SetValue(a.shareName)
		a.nameInput.Focus()
		a.step = asNameInput
		return a, textinput.Blink, false
	}
	return a, cmd, false
}

func (a *addShareWizard) updateNameInput(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok {
		switch km.String() {
		case "enter":
			// Re-sanitize before commit — the operator may have hand-typed
			// underscores or capital letters or other forbidden chars
			// (Tailscale Drive's validator is strict). Better to coerce
			// silently than to dump a "400 invalid share name" back.
			a.shareName = sanitizeShareName(strings.TrimSpace(a.nameInput.Value()))
			if a.shareName == "" {
				return a, nil, false
			}
			a.step = asConfirm
			return a, nil, false
		case "backspace":
			if a.nameInput.Value() == "" {
				a.step = asPickPath
				a.browser = newFileBrowser(a.devices[a.deviceIdx], a.chosenDir)
				return a, a.browser.start(), false
			}
		}
	}
	var cmd tea.Cmd
	a.nameInput, cmd = a.nameInput.Update(msg)
	return a, cmd, false
}

func (a *addShareWizard) updateConfirm(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return a, nil, false
	}
	switch km.String() {
	case "y", "Y", "enter":
		a.step = asRunning
		return a, a.runAdd(), false
	case "n", "N", "backspace":
		a.step = asNameInput
		a.nameInput.Focus()
		return a, textinput.Blink, false
	}
	return a, nil, false
}

type addDoneMsg struct {
	err error
}

func (a *addShareWizard) runAdd() tea.Cmd {
	dev := a.devices[a.deviceIdx]
	name := a.shareName
	pth := a.chosenDir
	actor := a.actor
	return func() tea.Msg {
		err := actions.ShareOn(dev, name, pth)
		target := dev + ":" + name
		journal.Log(actor, "share.add", target, pth, err)
		return addDoneMsg{err: err}
	}
}

func (a *addShareWizard) updateRunning(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if m, ok := msg.(addDoneMsg); ok {
		a.resultErr = m.err
		if m.err == nil {
			a.resultMsg = fmt.Sprintf("✓ Added %s → %s on %s",
				a.shareName, a.chosenDir, a.devices[a.deviceIdx])
		} else {
			a.resultMsg = fmt.Sprintf("✗ Failed: %s", m.err.Error())
		}
		a.step = asResult
	}
	return a, nil, false
}

// ── View ───────────────────────────────────────────────────────────────

func (a *addShareWizard) View(w, h int) string {
	bw := w * 8 / 10
	if bw < 80 {
		bw = 80
	}
	if bw > 160 {
		bw = 160
	}
	bh := h - 4
	if bh < 18 {
		bh = 18
	}
	if bh > 50 {
		bh = 50
	}

	var content string
	switch a.step {
	case asPickCategory:
		content = a.renderCategoryPick()
	case asPickDevice:
		content = a.renderDevicePick()
	case asPickPath:
		content = a.browser.view(bw-6, bh-6)
	case asNameInput:
		content = a.renderNameInput()
	case asConfirm:
		content = a.renderConfirm()
	case asRunning:
		content = "\n  Running tailscale drive share…"
	case asResult:
		if a.resultErr == nil {
			content = "\n  " + theme.OK.Render(a.resultMsg) +
				"\n\n  " + theme.ItemDim.Render("Press Enter or Esc to close.")
		} else {
			content = "\n  " + theme.Err.Render(a.resultMsg) +
				"\n\n  " + theme.ItemDim.Render("Press Enter or Esc to close.")
		}
	}

	title := lipgloss.NewStyle().
		Foreground(theme.Yellow).Bold(true).Reverse(true).
		Padding(0, 2).Render(" ADD TAILDRIVE SHARES ")
	body := lipgloss.JoinVertical(lipgloss.Left,
		title+"   "+a.stepBadges(), "",
		content)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Yellow).
		Padding(1, 2).
		Width(bw).
		Render(body)
}

// stepBadges renders the wizard's 5-step completion map as a horizontal
// strip of styled tags. Steps to the LEFT of the current one are "done"
// (green-bg, ✓), the current step is "active" (accent-bg, bold), steps to
// the RIGHT are "pending" (muted-bg, dim). Replaces the bare "Step N/5 · X"
// label with a visual progression the operator can read at a glance.
func (a *addShareWizard) stepBadges() string {
	steps := []string{"CATEGORY", "DEVICE", "PATH", "NAME", "CONFIRM"}
	// Map wizard.step to the 0..4 index of the active visual step. Running
	// and result both light up all 5 as done.
	var active int
	switch a.step {
	case asPickCategory:
		active = 0
	case asPickDevice:
		active = 1
	case asPickPath:
		active = 2
	case asNameInput:
		active = 3
	case asConfirm:
		active = 4
	case asRunning, asResult:
		active = 5 // sentinel: all done
	}
	return renderStepBadges(steps, active)
}

func (a *addShareWizard) renderCategoryPick() string {
	options := []struct{ label, hint string }{
		{"WORKSTATION SHARES", "macOS/Linux workstations (n3m-wks-*)"},
		{"SERVER SHARES", "Always-on servers (n3m-srv-*)"},
		{"SERVICE/ALL SHARES", "All hosts that could be share sources"},
	}
	var lines []string
	for i, opt := range options {
		cursor := "  "
		labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
		if i == a.categoryIdx {
			cursor = theme.AccentHiS.Render("▸ ")
			labelStyle = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		lines = append(lines,
			cursor+theme.AccentHiS.Render(fmt.Sprintf("%d.", i+1))+"  "+
				labelStyle.Render(opt.label)+"  "+theme.ItemDim.Render(opt.hint))
	}
	return "  Pick the kind of host you want to share from:\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  ↑↓ move · 1-3 jump · Enter pick · Esc cancel")
}

func (a *addShareWizard) renderDevicePick() string {
	var lines []string
	for i, d := range a.devices {
		cursor := "  "
		labelStyle := lipgloss.NewStyle().Foreground(theme.Text)
		if i == a.deviceIdx {
			cursor = theme.AccentHiS.Render("▸ ")
			labelStyle = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		role := actions.RoleOf(d)
		lines = append(lines,
			cursor+labelStyle.Render(d)+"  "+theme.ItemDim.Render(role))
	}
	return "  Pick the target device:\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  ↑↓ move · Enter pick · Backspace ← back · Esc cancel")
}

func (a *addShareWizard) renderNameInput() string {
	a.nameInput.Width = 50
	raw := strings.TrimSpace(a.nameInput.Value())
	clean := sanitizeShareName(raw)
	// Live preview row so the operator can see what the share will
	// actually be registered as — the Tailscale Drive validator only
	// accepts [a-z0-9-]+ so anything else gets coerced on submit.
	previewLine := ""
	if raw != "" {
		if raw == clean {
			previewLine = "  " + theme.ItemDim.Render("will be registered as: ") +
				lipgloss.NewStyle().Foreground(theme.Green).Render(clean)
		} else {
			previewLine = "  " + theme.ItemDim.Render("will be registered as: ") +
				lipgloss.NewStyle().Foreground(theme.Yellow).Render(clean) +
				"  " + theme.ItemDim.Render("(auto-sanitized)")
		}
	}
	rules := "  " + theme.ItemDim.Render("rules: lowercase a-z, digits, underscores; no hyphens/spaces/dots; max 24")
	return "  Selected path: " + theme.AccentHiS.Render(a.chosenDir) + "\n" +
		"  On device:     " + theme.AccentHiS.Render(a.devices[a.deviceIdx]) + "\n\n" +
		"  Share name (will be exposed at " +
		theme.Info.Render("http://100.100.100.100:8080/<user>/"+a.devices[a.deviceIdx]+"/<name>") + "):\n\n" +
		"  " + a.nameInput.View() + "\n" +
		previewLine + "\n" +
		rules + "\n\n" +
		theme.ItemDim.Render("  Enter to confirm · Backspace ← path picker · Esc cancel")
}

func (a *addShareWizard) renderConfirm() string {
	return "  About to run on " + theme.AccentHiS.Render(a.devices[a.deviceIdx]) + ":\n\n" +
		"    " + theme.Item.Render("tailscale drive share ") +
		theme.AccentHiS.Render(a.shareName) + " " +
		theme.AccentHiS.Render(a.chosenDir) + "\n\n" +
		theme.Warn.Render("  Confirm? [Y/n]") + "\n\n" +
		theme.ItemDim.Render("  Y or Enter to run · N or Backspace to edit name · Esc cancel")
}

// sanitizeShareName converts a directory basename into a name that
// Tailscale Drive will accept.
//
// **Empirically verified rules** (probed against macOS Tailscale 1.96.5
// on n3m-wks-01, 2026-05-18):
//   - ALLOWED: lowercase + uppercase letters, digits, underscores
//   - REJECTED with "400 invalid share name": hyphens, dots, spaces,
//     and anything else outside [A-Za-z0-9_]
//
// This is the OPPOSITE of the URL-label / DNS-label rule I assumed
// earlier (which had us mapping underscores → hyphens, producing the
// 400s the operator hit). The pre-existing shares on this device
// (`n3m_wks_01_taildrops`, `unified_memory_vault`) all use underscores —
// they were the canonical hint, and I should have looked at them first.
//
// Pipeline: lowercase → strip leading dots → map any
// whitespace/hyphen/dot/slash → underscore → strip anything else outside
// [a-z0-9_] → collapse underscore runs → trim leading/trailing
// underscores → cap at 24 (Tailscale Drive's NameMaxLen).
func sanitizeShareName(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimLeft(s, ".")
	for _, r := range []string{" ", "\t", "-", ".", "/", "\\"} {
		s = strings.ReplaceAll(s, r, "_")
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	s = b.String()
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if s == "" {
		return "share"
	}
	if len(s) > 24 {
		s = strings.TrimRight(s[:24], "_")
	}
	return s
}
