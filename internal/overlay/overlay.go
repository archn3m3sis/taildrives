// Package overlay defines the Overlay contract shared by splash (the
// host) and wizards (the concrete popups). Lives in its own package so
// both can depend on it without import cycles.
package overlay

import tea "github.com/charmbracelet/bubbletea"

// Overlay is a Bubble Tea sub-model rendered as a centered popup on top
// of the splash. Update returns whether the overlay is done so the host
// can dismiss it.
type Overlay interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Overlay, tea.Cmd, bool)
	View(w, h int) string
}

// Factory creates an Overlay for a given numeric kind. Splash holds an
// implementation that knows how to map its Action enum to concrete
// wizards. Defined as int rather than the Action enum to keep this
// package dependency-free.
type Factory func(kind int) Overlay
