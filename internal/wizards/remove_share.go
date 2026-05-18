package wizards

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/actions"
	"github.com/archn3m3sis/taildrives/internal/journal"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

type rmStep int

const (
	rmPickCategory rmStep = iota
	rmPickDevice
	rmLoadShares
	rmPickShares
	rmConfirm
	rmRunning
	rmResult
)

type removeShareWizard struct {
	actor string
	step  rmStep

	categoryIdx int
	devices     []string
	deviceIdx   int

	loading    bool
	shares     []string
	shareIdx   int
	marked     map[int]bool
	loadErr    error

	results    []removeResult
	resultIdx  int
}

type removeResult struct {
	Share string
	Err   error
}

func NewRemoveShareWizard(actor string) Overlay {
	return &removeShareWizard{actor: actor, step: rmPickCategory, marked: map[int]bool{}}
}

func (r *removeShareWizard) Init() tea.Cmd { return nil }

func (r *removeShareWizard) Update(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		return r, nil, true
	}
	switch r.step {
	case rmPickCategory:
		return r.updateCategoryPick(msg)
	case rmPickDevice:
		return r.updateDevicePick(msg)
	case rmLoadShares:
		return r.updateLoadShares(msg)
	case rmPickShares:
		return r.updatePickShares(msg)
	case rmConfirm:
		return r.updateConfirm(msg)
	case rmRunning:
		return r.updateRunning(msg)
	case rmResult:
		if km, ok := msg.(tea.KeyMsg); ok {
			if km.String() == "enter" || km.String() == "esc" || km.String() == "q" {
				return r, nil, true
			}
		}
	}
	return r, nil, false
}

func (r *removeShareWizard) updateCategoryPick(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil, false
	}
	switch {
	case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
		if r.categoryIdx > 0 {
			r.categoryIdx--
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
		if r.categoryIdx < 2 {
			r.categoryIdx++
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("1"))):
		r.categoryIdx = 0
	case key.Matches(km, key.NewBinding(key.WithKeys("2"))):
		r.categoryIdx = 1
	case key.Matches(km, key.NewBinding(key.WithKeys("3"))):
		r.categoryIdx = 2
	case key.Matches(km, key.NewBinding(key.WithKeys("enter"))):
		r.devices = r.devicesForCategory()
		r.deviceIdx = 0
		r.step = rmPickDevice
	}
	return r, nil, false
}

func (r *removeShareWizard) devicesForCategory() []string {
	switch r.categoryIdx {
	case 0:
		return actions.CoreWorkstations
	case 1:
		return actions.CoreServers
	default:
		out := append([]string{}, actions.CoreServers...)
		return append(out, actions.CoreWorkstations...)
	}
}

func (r *removeShareWizard) updateDevicePick(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil, false
	}
	switch {
	case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
		if r.deviceIdx > 0 {
			r.deviceIdx--
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
		if r.deviceIdx < len(r.devices)-1 {
			r.deviceIdx++
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("backspace"))):
		r.step = rmPickCategory
	case key.Matches(km, key.NewBinding(key.WithKeys("enter"))):
		r.step = rmLoadShares
		r.loading = true
		return r, r.loadShares(), false
	}
	return r, nil, false
}

type sharesLoadedMsg struct {
	shares []string
	err    error
}

func (r *removeShareWizard) loadShares() tea.Cmd {
	dev := r.devices[r.deviceIdx]
	return func() tea.Msg {
		s, err := actions.SharesOn(dev)
		return sharesLoadedMsg{shares: s, err: err}
	}
}

func (r *removeShareWizard) updateLoadShares(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	if m, ok := msg.(sharesLoadedMsg); ok {
		r.shares = m.shares
		r.loadErr = m.err
		r.loading = false
		r.marked = map[int]bool{}
		if len(r.shares) == 0 {
			r.step = rmResult
			r.results = []removeResult{{Err: fmt.Errorf("no shares published by %s (or device unreachable)", r.devices[r.deviceIdx])}}
			return r, nil, false
		}
		r.step = rmPickShares
		r.shareIdx = 0
	}
	return r, nil, false
}

func (r *removeShareWizard) updatePickShares(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil, false
	}
	switch {
	case key.Matches(km, key.NewBinding(key.WithKeys("up", "k"))):
		if r.shareIdx > 0 {
			r.shareIdx--
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("down", "j"))):
		if r.shareIdx < len(r.shares)-1 {
			r.shareIdx++
		}
	case key.Matches(km, key.NewBinding(key.WithKeys(" "))):
		r.marked[r.shareIdx] = !r.marked[r.shareIdx]
	case key.Matches(km, key.NewBinding(key.WithKeys("a"))):
		// mark all
		for i := range r.shares {
			r.marked[i] = true
		}
	case key.Matches(km, key.NewBinding(key.WithKeys("c"))):
		r.marked = map[int]bool{}
	case key.Matches(km, key.NewBinding(key.WithKeys("backspace"))):
		r.step = rmPickDevice
	case key.Matches(km, key.NewBinding(key.WithKeys("enter"))):
		if r.countMarked() == 0 {
			return r, nil, false
		}
		r.step = rmConfirm
	}
	return r, nil, false
}

func (r *removeShareWizard) countMarked() int {
	n := 0
	for _, v := range r.marked {
		if v {
			n++
		}
	}
	return n
}

func (r *removeShareWizard) updateConfirm(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	km, ok := msg.(tea.KeyMsg)
	if !ok {
		return r, nil, false
	}
	switch km.String() {
	case "y", "Y", "enter":
		r.step = rmRunning
		r.results = nil
		r.resultIdx = 0
		return r, r.runRemoveOne(), false
	case "n", "N", "backspace":
		r.step = rmPickShares
	}
	return r, nil, false
}

type removeOneDoneMsg struct {
	share string
	err   error
}

// runRemoveOne removes the next marked share, then re-fires itself for
// the following one. Sequential removal so the journal is ordered and a
// failure on one doesn't cascade.
func (r *removeShareWizard) runRemoveOne() tea.Cmd {
	dev := r.devices[r.deviceIdx]
	// find next marked index >= resultIdx
	idx := -1
	for i := r.resultIdx; i < len(r.shares); i++ {
		if r.marked[i] {
			idx = i
			break
		}
	}
	if idx < 0 {
		return func() tea.Msg { return removeOneDoneMsg{share: "", err: nil} }
	}
	r.resultIdx = idx + 1
	share := r.shares[idx]
	actor := r.actor
	return func() tea.Msg {
		err := actions.UnshareOn(dev, share)
		journal.Log(actor, "share.remove", dev+":"+share, "", err)
		return removeOneDoneMsg{share: share, err: err}
	}
}

func (r *removeShareWizard) updateRunning(msg tea.Msg) (Overlay, tea.Cmd, bool) {
	m, ok := msg.(removeOneDoneMsg)
	if !ok {
		return r, nil, false
	}
	if m.share != "" {
		r.results = append(r.results, removeResult{Share: m.share, Err: m.err})
	}
	// any more marked beyond resultIdx?
	more := false
	for i := r.resultIdx; i < len(r.shares); i++ {
		if r.marked[i] {
			more = true
			break
		}
	}
	if more {
		return r, r.runRemoveOne(), false
	}
	r.step = rmResult
	return r, nil, false
}

// ── View ───────────────────────────────────────────────────────────────

func (r *removeShareWizard) View(w, h int) string {
	bw := w * 8 / 10
	if bw < 80 {
		bw = 80
	}
	if bw > 160 {
		bw = 160
	}

	var content string
	switch r.step {
	case rmPickCategory:
		content = r.renderCategoryPick()
	case rmPickDevice:
		content = r.renderDevicePick()
	case rmLoadShares:
		content = "\n  loading shares from " + theme.AccentHiS.Render(r.devices[r.deviceIdx]) + "…"
	case rmPickShares:
		content = r.renderSharePick()
	case rmConfirm:
		content = r.renderConfirm()
	case rmRunning:
		content = r.renderRunning()
	case rmResult:
		content = r.renderResult()
	}

	title := lipgloss.NewStyle().
		Foreground(theme.Red).Bold(true).Reverse(true).
		Padding(0, 2).Render(" REMOVE TAILDRIVE SHARES ")
	body := lipgloss.JoinVertical(lipgloss.Left, title+"   "+r.stepBadges(), "", content)
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Red).
		Padding(1, 2).
		Width(bw).
		Render(body)
}

// stepBadges renders the remove wizard's 4-step completion map. The
// rmLoadShares state is transient (covered by the spinner), so it shares
// the "active=device" position visually — the picker step only "starts"
// when the share list actually loads.
func (r *removeShareWizard) stepBadges() string {
	steps := []string{"CATEGORY", "DEVICE", "PICK SHARES", "CONFIRM"}
	var active int
	switch r.step {
	case rmPickCategory:
		active = 0
	case rmPickDevice, rmLoadShares:
		active = 1
	case rmPickShares:
		active = 2
	case rmConfirm:
		active = 3
	case rmRunning, rmResult:
		active = 4 // sentinel: all done
	}
	return renderStepBadges(steps, active)
}

func (r *removeShareWizard) renderCategoryPick() string {
	options := []struct{ label, hint string }{
		{"WORKSTATION SHARES", "macOS/Linux workstations"},
		{"SERVER SHARES", "Always-on servers"},
		{"ALL HOSTS", "Pick from any share-publishing device"},
	}
	var lines []string
	for i, o := range options {
		cur := "  "
		ls := lipgloss.NewStyle().Foreground(theme.Text)
		if i == r.categoryIdx {
			cur = theme.AccentHiS.Render("▸ ")
			ls = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		lines = append(lines, cur+theme.AccentHiS.Render(fmt.Sprintf("%d.", i+1))+"  "+
			ls.Render(o.label)+"  "+theme.ItemDim.Render(o.hint))
	}
	return "  Where do you want to remove shares from?\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  ↑↓ move · 1-3 jump · Enter pick · Esc cancel")
}

func (r *removeShareWizard) renderDevicePick() string {
	var lines []string
	for i, d := range r.devices {
		cur := "  "
		ls := lipgloss.NewStyle().Foreground(theme.Text)
		if i == r.deviceIdx {
			cur = theme.AccentHiS.Render("▸ ")
			ls = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		lines = append(lines, cur+ls.Render(d)+"  "+theme.ItemDim.Render(actions.RoleOf(d)))
	}
	return "  Pick the device to remove shares from:\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  ↑↓ move · Enter pick · Backspace ← back · Esc cancel")
}

func (r *removeShareWizard) renderSharePick() string {
	if r.loadErr != nil {
		return "\n  " + theme.Err.Render("✗ "+r.loadErr.Error())
	}
	var lines []string
	for i, s := range r.shares {
		mark := " "
		if r.marked[i] {
			mark = theme.Err.Render("●")
		}
		cur := "  "
		ls := lipgloss.NewStyle().Foreground(theme.Text)
		if i == r.shareIdx {
			cur = theme.AccentHiS.Render("▸ ")
			ls = lipgloss.NewStyle().Foreground(theme.AccentHi).Bold(true)
		}
		lines = append(lines, cur+"["+mark+"] "+ls.Render(s))
	}
	return "  Shares on " + theme.AccentHiS.Render(r.devices[r.deviceIdx]) + ":\n\n" +
		strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  Space toggle · a mark all · c clear · Enter commit · Backspace ← back · Esc cancel") +
		"\n  " + theme.Warn.Render(fmt.Sprintf("%d marked for removal", r.countMarked()))
}

func (r *removeShareWizard) renderConfirm() string {
	var lines []string
	for i, s := range r.shares {
		if r.marked[i] {
			lines = append(lines, "    "+theme.Err.Render("✗ ")+theme.AccentHiS.Render(s))
		}
	}
	return "  " + theme.Err.Render("PERMANENT REMOVAL — confirm:") + "\n\n" +
		"  Device: " + theme.AccentHiS.Render(r.devices[r.deviceIdx]) + "\n" +
		"  Shares:\n" + strings.Join(lines, "\n") + "\n\n" +
		theme.Warn.Render("  Confirm? [Y/n]") + "\n\n" +
		theme.ItemDim.Render("  Y or Enter to run · N or Backspace to re-pick · Esc cancel")
}

func (r *removeShareWizard) renderRunning() string {
	done := len(r.results)
	total := r.countMarked()
	return fmt.Sprintf("\n  removing share %d/%d…\n", done+1, total)
}

func (r *removeShareWizard) renderResult() string {
	var lines []string
	ok, fail := 0, 0
	for _, res := range r.results {
		if res.Err == nil {
			lines = append(lines, "  "+theme.OK.Render("✓ ")+theme.AccentHiS.Render(res.Share))
			ok++
		} else {
			lines = append(lines, "  "+theme.Err.Render("✗ ")+theme.AccentHiS.Render(res.Share)+"  "+theme.Err.Render(res.Err.Error()))
			fail++
		}
	}
	header := theme.OK.Render(fmt.Sprintf("Removed %d shares", ok))
	if fail > 0 {
		header += "  " + theme.Err.Render(fmt.Sprintf("(%d failed)", fail))
	}
	return "  " + header + "\n\n" + strings.Join(lines, "\n") + "\n\n" +
		theme.ItemDim.Render("  Press Enter or Esc to close.")
}
