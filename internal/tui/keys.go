package tui

import "github.com/charmbracelet/bubbles/key"

type Keymap struct {
	Up, Down, Left, Right     key.Binding
	PgUp, PgDn, Home, End     key.Binding
	Tab, ShiftTab             key.Binding
	Enter, Back               key.Binding
	Mark, MarkAll, ClearMarks key.Binding
	Get, Put, Copy, BulkSend  key.Binding
	Mount, Refresh, Search    key.Binding
	Filter, Help, Quit        key.Binding
	Preview                   key.Binding // 'P' — split-pane file preview
	Delete                    key.Binding // 'D' — delete file/dir (God Mode gated)
}

func NewKeymap() Keymap {
	return Keymap{
		Up:          key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:        key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Left:        key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "back")),
		Right:       key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "drill in")),
		PgUp:        key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("PgUp", "page up")),
		PgDn:        key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("PgDn", "page down")),
		Home:        key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top")),
		End:         key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom")),
		Tab:         key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "next pane")),
		ShiftTab:    key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("S-Tab", "prev pane")),
		Enter:       key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "select")),
		Back:        key.NewBinding(key.WithKeys("backspace"), key.WithHelp("BkSp", "parent dir")),
		Mark:        key.NewBinding(key.WithKeys(" "), key.WithHelp("Space", "mark")),
		MarkAll:     key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("^A", "mark all")),
		ClearMarks:  key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("^X", "clear marks")),
		Get:         key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "download")),
		Put:         key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "upload")),
		Copy:        key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "copy")),
		BulkSend:    key.NewBinding(key.WithKeys("b"), key.WithHelp("b", "bulk-send")),
		Mount:       key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mount info")),
		Refresh:     key.NewBinding(key.WithKeys("r", "f5"), key.WithHelp("r", "refresh")),
		Search:      key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "search")),
		Filter:      key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "category filter")),
		Help:        key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:        key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Preview:     key.NewBinding(key.WithKeys("p", "P"), key.WithHelp("P", "preview file")),
		// Capital-only D for delete — irreversible op, refuses to share lowercase.
		Delete:      key.NewBinding(key.WithKeys("D"), key.WithHelp("D", "delete (God Mode)")),
	}
}
