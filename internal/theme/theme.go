// Package theme is the lipgloss color/style palette for the taildrives TUI.
// Cyberpunk-aligned to match the Grafana dashboard aesthetic.
package theme

import "github.com/charmbracelet/lipgloss"

// Core palette.
var (
	Bg        = lipgloss.Color("#0a0e1a")
	BgPanel   = lipgloss.Color("#0f1424")
	BgSubtle  = lipgloss.Color("#141a2e")
	Border    = lipgloss.Color("#1f2942")
	BorderHi  = lipgloss.Color("#7c3aed")
	Text      = lipgloss.Color("#e6e9f5")
	TextMuted = lipgloss.Color("#7b8298")
	TextDim   = lipgloss.Color("#4a5168")

	Accent     = lipgloss.Color("#22d3ee") // cyan
	AccentHi   = lipgloss.Color("#67e8f9")
	Magenta    = lipgloss.Color("#e879f9")
	Pink       = lipgloss.Color("#f472b6")
	Purple     = lipgloss.Color("#a78bfa")
	PurpleDeep = lipgloss.Color("#7c3aed")
	Green      = lipgloss.Color("#22c55e")
	Yellow     = lipgloss.Color("#eab308")
	Red        = lipgloss.Color("#ef4444")
	Orange     = lipgloss.Color("#fb923c")
)

// Common styles.
var (
	Panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(Border).
		Padding(0, 1)

	PanelActive = Panel.
			BorderForeground(BorderHi)

	Title = lipgloss.NewStyle().
		Foreground(AccentHi).
		Bold(true)

	TitleActive = lipgloss.NewStyle().
			Foreground(Magenta).
			Bold(true).
			Underline(true)

	Item = lipgloss.NewStyle().
		Foreground(Text)

	ItemDim = lipgloss.NewStyle().
		Foreground(TextMuted)

	// Hot-pink neon on a soft-blue background — high-contrast and unmistakable
	// against the rest of the cyberpunk palette.
	ItemSelected = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#ff2bd6")).
			Background(lipgloss.Color("#1a2a4a")).
			Bold(true)

	ItemMarked = lipgloss.NewStyle().
			Foreground(Pink).
			Bold(true)

	Dir = lipgloss.NewStyle().
		Foreground(Purple).
		Bold(true)

	File = lipgloss.NewStyle().
		Foreground(Text)

	Size = lipgloss.NewStyle().
		Foreground(TextMuted).
		Italic(true)

	StatusBar = lipgloss.NewStyle().
			Foreground(Text).
			Background(BgPanel).
			Padding(0, 1)

	KeyHint = lipgloss.NewStyle().
		Foreground(Accent).
		Bold(true)

	KeyDesc = lipgloss.NewStyle().
		Foreground(TextMuted)

	Banner = lipgloss.NewStyle().
		Foreground(Magenta).
		Bold(true)

	OK    = lipgloss.NewStyle().Foreground(Green).Bold(true)
	Err   = lipgloss.NewStyle().Foreground(Red).Bold(true)
	Warn  = lipgloss.NewStyle().Foreground(Yellow).Bold(true)
	Info  = lipgloss.NewStyle().Foreground(Accent)

	// Style wrappers — call .Render on these (lipgloss.Color can't be rendered directly).
	AccentHiS = lipgloss.NewStyle().Foreground(AccentHi)
	AccentS   = lipgloss.NewStyle().Foreground(Accent)
	MagentaS  = lipgloss.NewStyle().Foreground(Magenta)
	PinkS     = lipgloss.NewStyle().Foreground(Pink)
	PurpleS   = lipgloss.NewStyle().Foreground(Purple)
)

// Logo is the splash banner — drawn at app boot.
const Logo = `
 ████████╗ █████╗ ██╗██╗     ██████╗ ██████╗ ██╗██╗   ██╗███████╗███████╗
 ╚══██╔══╝██╔══██╗██║██║     ██╔══██╗██╔══██╗██║██║   ██║██╔════╝██╔════╝
    ██║   ███████║██║██║     ██║  ██║██████╔╝██║██║   ██║█████╗  ███████╗
    ██║   ██╔══██║██║██║     ██║  ██║██╔══██╗██║╚██╗ ██╔╝██╔══╝  ╚════██║
    ██║   ██║  ██║██║███████╗██████╔╝██║  ██║██║ ╚████╔╝ ███████╗███████║
    ╚═╝   ╚═╝  ╚═╝╚═╝╚══════╝╚═════╝ ╚═╝  ╚═╝╚═╝  ╚═══╝  ╚══════╝╚══════╝
                       n3m3sis-devnet · tailscale drive
`
