// Package tui is the Bubble Tea application for taildrives.
package tui

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/cats"
	"github.com/archn3m3sis/taildrives/internal/cfg"
	"github.com/archn3m3sis/taildrives/internal/desc"
	"github.com/archn3m3sis/taildrives/internal/envcfg"
	"github.com/sahilm/fuzzy"

	"github.com/archn3m3sis/taildrives/internal/imgview"
	"github.com/archn3m3sis/taildrives/internal/kittyimg"
	"github.com/archn3m3sis/taildrives/internal/local"
	"github.com/archn3m3sis/taildrives/internal/theme"
	"github.com/archn3m3sis/taildrives/internal/webdav"
	"github.com/archn3m3sis/taildrives/internal/xfer"
)

// ── pane enum ──────────────────────────────────────────────────────────────

type pane int

const (
	paneCats       pane = iota // Content Categories (left, slot 1)
	paneSources                // Share Sources (left, slot 2)
	paneEnvConfig              // Environment Configurations (left, slot 3)
	paneGitLab                 // GitLab Repositories (left, slot 4)
	paneFiles                  // Shares & Files (right)
	numPanes
)

// ── messages ───────────────────────────────────────────────────────────────

type rootLoadedMsg struct {
	namespace    string
	devices      []string
	deviceShares map[string][]string
	deviceState  map[string]devState // online / offline / unknown
	localHost    string
	localShares  []local.Share
	loginName    string
	err          error
}

// godUser is the canonical email permitted to delete via the TUI.
// godOSUser is the matching OS username used as a fallback on tagged
// servers where the tailnet identity resolves to a tag, not a human.
const (
	godUser   = "archn3m3sis.bounties@gmail.com"
	godOSUser = "archn3m3sis"
)

type devState struct {
	Online   bool
	LastSeen string // "1h ago" for offline
}
type dirLoadedMsg struct {
	path    string
	entries []webdav.Entry
	err     error
}
type promptMsg struct {
	kind   string // "put" | "get" | "copy-dst" | "bulk-confirm"
	prompt string
}
type promptResultMsg struct {
	kind  string
	value string
	ok    bool
}
type statusMsg struct {
	level string // "ok" | "err" | "info"
	text  string
}
type tickMsg time.Time

// ── model ──────────────────────────────────────────────────────────────────

type Model struct {
	w, h int

	cfg    cfg.Config
	client *webdav.Client

	keys   Keymap
	spin   spinner.Model
	prompt textinput.Model

	loading bool
	err     error

	// data
	namespace    string
	devices      []string
	deviceShares map[string][]string // device name → share names (cached at startup)
	deviceState  map[string]devState // online/offline (from tailscale status)
	localHost    string
	localShares  []local.Share
	loginName    string // current tailnet user — for God Mode delete check
	// per-device current path + entries cache
	currentDev   int
	dirStack     []string         // path stack relative to namespace
	dirEntries   []webdav.Entry   // entries at current path
	dirsCache    map[string][]webdav.Entry

	// pane cursors
	active    pane
	catIdx    int
	catFilter string // active category name
	devIdx    int
	dirIdx    int
	dirOff    int
	envIdx    int
	gitIdx    int

	// curated registries (loaded from ~/.config/taildrives/*.json)
	envConfigs []envcfg.Entry
	gitlab     []envcfg.Entry

	// marks (full webdav href → true)
	marked map[string]bool

	// transfer
	xferMgr *xfer.Manager

	// status
	statusText  string
	statusLevel string

	// prompt mode
	promptKind   string
	promptActive bool

	// help overlay
	helpOpen bool

	// search/filter input (in files pane). searchDirsOnly toggles via
	// Ctrl+D in search mode — when true, the fuzzy filter drops file
	// entries from the result list and keeps only directories.
	search         string
	searchDirsOnly bool

	// copy "clipboard": when set, c captured a source. Next c drops it at
	// the current path; Esc cancels.
	copyArmed bool
	copySrc   string // WebDAV or local:// path of source
	copyKind  string // "file" or "directory"
	copyName  string // basename, used to build destination

	// File preview state — when active, the file pane splits horizontally
	// and the right half renders the file's content.
	previewActive bool
	previewPath   string
	previewText   string // for text/binary
	previewRaw    []byte // raw bytes for image rendering (size-dependent)
	previewKind   string // "text" | "binary" | "image"
	previewBytes  int    // total file size
	previewErr    error

	// Double-Esc exit tracking — single Esc still closes overlays /
	// previews, two presses within doubleEscWindow trigger the outro.
	lastEsc        time.Time
	OutroRequested bool
}

// previewLoadedMsg is sent when the preview content for a file finishes
// loading (or fails).
type previewLoadedMsg struct {
	path  string
	text  string
	raw   []byte // populated for images so the view can re-render on resize
	kind  string // "text" | "binary" | "image"
	bytes int
	err   error
}

// ── constructor ─────────────────────────────────────────────────────────────

func New(c cfg.Config, client *webdav.Client, mgr *xfer.Manager) Model {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(theme.Magenta)

	ti := textinput.New()
	ti.Prompt = "› "
	ti.Placeholder = ""
	ti.CharLimit = 1024

	return Model{
		cfg:       c,
		client:    client,
		keys:      NewKeymap(),
		spin:      sp,
		prompt:    ti,
		dirsCache: map[string][]webdav.Entry{},
		marked:    map[string]bool{},
		active:    paneCats,
		// Always launch on the default — no saved category override.
		// Cursor lands at index 0 (the default) and the file pane shows the
		// full aggregated view. Live-apply on Up/Down still lets the user
		// narrow as they navigate.
		catFilter:   "No Active Content Filtering",
		catIdx:      0,
		envConfigs:  envcfg.LoadEnvConfigs(),
		gitlab:      envcfg.LoadGitLabRepos(),
		xferMgr:     mgr,
		statusText:  "Connecting to Tailscale Drive…",
		statusLevel: "info",
		loading:     true,
	}
}

func cfgOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// ── init ────────────────────────────────────────────────────────────────────

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.spin.Tick,
		m.loadRoot(),
		tea.EnterAltScreen,
		tickEvery(),
	)
}

func tickEvery() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) loadRoot() tea.Cmd {
	return func() tea.Msg {
		entries, err := m.client.List("/")
		if err != nil {
			return rootLoadedMsg{err: err}
		}
		// the single user namespace
		var ns string
		for _, e := range entries {
			if e.IsDir {
				ns = strings.Trim(e.Href, "/")
				break
			}
		}
		if ns == "" {
			return rootLoadedMsg{err: fmt.Errorf("no user namespace at WebDAV root — is Tailscale Drive enabled?")}
		}
		devs, err := m.client.List("/" + ns)
		if err != nil {
			return rootLoadedMsg{namespace: ns, err: err}
		}
		var names []string
		for _, d := range devs {
			if !d.IsDir {
				continue
			}
			if webdav.IsSidecarDevice(d.Name) || isMobileOrEnt(d.Name) {
				continue
			}
			names = append(names, d.Name)
		}
		sort.SliceStable(names, func(i, j int) bool {
			return deviceRank(names[i]) < deviceRank(names[j])
		})

		// Concurrently fetch each device's shares so the categories pane can
		// actually filter the devices column. Each PROPFIND gets a short
		// timeout so one unreachable device (mobile/Apple TV) can't block
		// startup of the whole TUI.
		shareMap := make(map[string][]string, len(names))
		var mu sync.Mutex
		var wg sync.WaitGroup
		fast := m.client.WithTimeout(3 * time.Second)
		for _, d := range names {
			wg.Add(1)
			go func(dev string) {
				defer wg.Done()
				entries, err := fast.List("/" + ns + "/" + dev)
				if err != nil {
					return
				}
				var shares []string
				for _, e := range entries {
					if e.IsDir {
						shares = append(shares, e.Name)
					}
				}
				mu.Lock()
				shareMap[dev] = shares
				mu.Unlock()
			}(d)
		}
		wg.Wait()

		// Local host: ALWAYS synthesize the entry. On macOS the GUI app
		// hides the `drive` CLI subcommand entirely so we can't enumerate
		// our own shares — but the device itself still belongs in the
		// device pane (otherwise wks-01 selecting "workstations" from
		// wks-01 sees nothing, which is confusing). On Linux/Windows we
		// also populate the share list.
		localHost := local.HostName()
		localShares, _ := local.ListShares()
		loginName := local.SelfLoginName()
		if localHost != "" {
			synth := localDeviceName(localHost)
			names = append([]string{synth}, names...)
			ls := make([]string, 0, len(localShares))
			for _, s := range localShares {
				ls = append(ls, s.Name)
			}
			shareMap[synth] = ls
		}

		// Merge in offline tailnet devices so the user can see them with
		// an "(offline)" marker instead of silently missing.
		state := make(map[string]devState)
		if peers, err := local.TailnetDevices(); err == nil {
			known := make(map[string]bool, len(names))
			for _, n := range names {
				known[stripLocalMarker(n)] = true
			}
			for _, p := range peers {
				if webdav.IsSidecarDevice(p.Hostname) {
					continue
				}
				// Mobile + Apple TV devices don't host Taildrive shares and
				// shouldn't pollute the Share Sources pane.
				if isMobileOrEnt(p.Hostname) {
					continue
				}
				state[p.Hostname] = devState{Online: p.Online, LastSeen: p.LastSeen}
				if p.Hostname == localHost {
					state[p.Hostname] = devState{Online: true}
					continue
				}
				if !known[p.Hostname] {
					names = append(names, p.Hostname)
				}
			}
		}

		sort.SliceStable(names, func(i, j int) bool {
			return deviceRank(names[i]) < deviceRank(names[j])
		})

		return rootLoadedMsg{
			namespace:    ns,
			devices:      names,
			deviceShares: shareMap,
			deviceState:  state,
			localHost:    localHost,
			localShares:  localShares,
			loginName:    loginName,
		}
	}
}

func (m Model) loadDir(p string) tea.Cmd {
	// Local-device paths are encoded as "local://<sharename>[/sub/path]".
	if strings.HasPrefix(p, "local://") {
		return m.loadLocalDir(p)
	}
	return func() tea.Msg {
		entries, err := m.client.List(p)
		return dirLoadedMsg{path: p, entries: entries, err: err}
	}
}

// loadLocalDir lists entries on the local filesystem under one of this host's
// own published Taildrive shares. p is "local://<share>[/sub]".
func (m Model) loadLocalDir(p string) tea.Cmd {
	return func() tea.Msg {
		rest := strings.TrimPrefix(p, "local://")
		rest = strings.TrimSuffix(rest, "/")
		if rest == "" {
			// root: list all local shares as synthetic directory entries
			out := make([]webdav.Entry, 0, len(m.localShares))
			for _, s := range m.localShares {
				out = append(out, webdav.Entry{
					Href:  "local://" + s.Name + "/",
					Name:  s.Name,
					IsDir: true,
					Size:  0,
				})
			}
			return dirLoadedMsg{path: p, entries: out}
		}
		// resolve <share>[/sub] → filesystem path
		parts := strings.SplitN(rest, "/", 2)
		shareName := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		var sharePath string
		for _, s := range m.localShares {
			if s.Name == shareName {
				sharePath = s.Path
				break
			}
		}
		if sharePath == "" {
			return dirLoadedMsg{path: p, err: fmt.Errorf("local share %q not found", shareName)}
		}
		fsPath := sharePath
		if sub != "" {
			fsPath = path.Join(sharePath, sub)
		}
		entries, err := local.LocalEntries(fsPath)
		if err != nil {
			return dirLoadedMsg{path: p, err: err}
		}
		out := make([]webdav.Entry, 0, len(entries))
		for _, e := range entries {
			out = append(out, webdav.Entry{
				Href:  p + e.Name,
				Name:  e.Name,
				IsDir: e.IsDir,
				Size:  e.Size,
			})
		}
		return dirLoadedMsg{path: p, entries: out}
	}
}

// ── update ──────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	// Pre-everything: double-Esc → outro exit. Must run BEFORE prompt /
	// overlay / pane handlers because any of them may consume Esc for
	// their own meaning (cancel input, close preview, etc.). 500ms window
	// distinguishes intentional double-tap from a single press the user
	// followed up later. Single Esc still falls through to whatever
	// handler owns it, so existing close-overlay-on-Esc behavior is
	// preserved.
	const doubleEscWindow = 500 * time.Millisecond
	if km, ok := msg.(tea.KeyMsg); ok && km.String() == "esc" {
		if !m.lastEsc.IsZero() && time.Since(m.lastEsc) <= doubleEscWindow {
			m.OutroRequested = true
			return m, tea.Quit
		}
		m.lastEsc = time.Now()
		// Fall through — single-Esc keeps its existing per-pane meaning.
	}

	// prompt mode swallows most keys
	if m.promptActive {
		return m.updatePrompt(msg)
	}

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		// Explicit wheel handling. Without this branch, Bubble Tea's
		// default (or the terminal's alternate-scroll mode, DECSET 1007)
		// synthesizes multiple up/down KEY events per wheel notch — the
		// "half a scroll zaps to bottom" behavior the operator hit. By
		// owning the MouseMsg path we control the scroll step explicitly:
		// one wheel notch = one row movement, with a small accelerator
		// (3 rows) when shift is held for power-scrolling.
		//
		// We return early with the synthesized key event so the existing
		// pane-specific handlers (handleKeyFiles / handleKeyDevs / …)
		// receive a normal up/down KeyMsg they already understand. No
		// duplication of scroll logic across all panes.
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			return m.handleKey(tea.KeyMsg{Type: tea.KeyUp})
		case tea.MouseButtonWheelDown:
			return m.handleKey(tea.KeyMsg{Type: tea.KeyDown})
		case tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight:
			// Horizontal wheel — no-op, swallow so it doesn't fall through
			// to an unintended handler.
			return m, nil
		}
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tickMsg:
		return m, tickEvery()

	case rootLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			m.setStatus("err", msg.err.Error())
			return m, nil
		}
		m.namespace = msg.namespace
		m.devices = msg.devices
		m.deviceShares = msg.deviceShares
		m.deviceState = msg.deviceState
		m.localHost = msg.localHost
		m.localShares = msg.localShares
		m.loginName = msg.loginName
		if len(m.devices) > 0 {
			// Land on the first device that actually matches the saved filter.
			if devs := m.visibleDevices(); len(devs) > 0 {
				m.currentDev = m.indexOfDevice(devs[0])
			}
			return m, m.loadCurrentDev()
		}
		m.setStatus("ok", fmt.Sprintf("Connected — namespace=%s, no devices yet", m.namespace))
		return m, nil

	case dirLoadedMsg:
		// IMPORTANT: only update state if this msg is for the path we're
		// currently viewing. Otherwise late-arriving messages for a
		// previous device can stomp the current view.
		isCurrent := msg.path == m.currentDirPath()
		if msg.err != nil {
			es := msg.err.Error()
			// Always clear stale entries on error so the previous list can't
			// fool the user into thinking it loaded — and so pressing Enter
			// can't drill into a phantom path.
			if isCurrent {
				m.dirEntries = nil
				m.dirIdx = 0
				m.dirOff = 0
			}
			// Friendly messages for the common failures
			switch {
			case strings.Contains(es, "404") && strings.Contains(es, "not supported on platform"):
				m.setStatus("warn", fmt.Sprintf("%s does not host any Taildrive shares (skipping)", m.currentDeviceName()))
			case strings.Contains(es, "500") && strings.Contains(es, "unable to determine address"):
				m.setStatus("err", fmt.Sprintf("%s — Tailscale daemon can't proxy this share. Restart Tailscale on that host.", m.currentDeviceName()))
			case strings.Contains(es, "404"):
				m.setStatus("warn", fmt.Sprintf("path not found: %s", msg.path))
				// Pop the bad dir off the breadcrumb so the user isn't stuck
				if isCurrent && len(m.dirStack) > 0 {
					m.dirStack = m.dirStack[:len(m.dirStack)-1]
				}
			default:
				m.setStatus("err", "list: "+es)
			}
			return m, nil
		}
		m.dirsCache[msg.path] = msg.entries
		if isCurrent {
			m.dirEntries = msg.entries
			m.dirIdx = 0
			m.dirOff = 0
		}
		m.setStatus("ok", fmt.Sprintf("%s — %d items", msg.path, len(msg.entries)))
		return m, nil

	case statusMsg:
		m.setStatus(msg.level, msg.text)
		return m, nil

	case previewLoadedMsg:
		if msg.err != nil {
			m.previewErr = msg.err
			m.previewText = ""
			m.previewRaw = nil
			m.previewKind = ""
			m.setStatus("err", "preview: "+msg.err.Error())
			return m, nil
		}
		m.previewPath = msg.path
		m.previewText = msg.text
		m.previewRaw = msg.raw
		m.previewKind = msg.kind
		m.previewBytes = msg.bytes
		m.previewErr = nil
		m.setStatus("ok", fmt.Sprintf("preview: %s (%s, %s)",
			msg.path, msg.kind, humanBytes(int64(msg.bytes))))
		return m, nil

	case tea.KeyMsg:
		// help overlay swallows
		if m.helpOpen {
			if msg.String() == "esc" || msg.String() == "?" || msg.String() == "q" {
				m.helpOpen = false
			}
			return m, nil
		}
		return m.handleKey(msg)
	}

	for _, c := range cmds {
		_ = c
	}
	return m, nil
}

func (m Model) updatePrompt(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "esc":
			// For search prompts, also clear the active filter so Esc
			// produces "back to the unfiltered list" — what the operator
			// expects when they're done searching. Reset the dirs-only
			// toggle too so the next search session starts fresh.
			if m.promptKind == "search" {
				m.search = ""
				m.searchDirsOnly = false
				m.dirIdx = 0
			}
			m.promptActive = false
			m.prompt.Blur()
			return m, nil
		case "ctrl+d":
			// Toggle dirs-only filter live inside the search prompt.
			// dirIdx snaps to 0 so the highlighted row stays valid after
			// the result set shrinks.
			if m.promptKind == "search" {
				m.searchDirsOnly = !m.searchDirsOnly
				m.dirIdx = 0
				mode := "files+dirs"
				if m.searchDirsOnly {
					mode = "dirs only"
				}
				m.setStatus("info", "Search filter: "+mode)
				return m, nil
			}
		case "enter":
			val := strings.TrimSpace(m.prompt.Value())
			kind := m.promptKind
			// Search mode commits the current filter and closes the input
			// while KEEPING the filter active. Operator can scroll the
			// narrowed list, drill in, then Esc later to clear.
			if kind == "search" {
				m.search = val
				m.promptActive = false
				m.prompt.Blur()
				m.dirIdx = 0
				return m, nil
			}
			m.promptActive = false
			m.prompt.Blur()
			m.prompt.SetValue("")
			return m.handlePromptResult(kind, val)
		}
	}
	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)
	// LIVE filter: while typing in the search prompt, mirror the input
	// into m.search so visibleFiles updates per keystroke (no need to
	// press Enter to see results). Other prompt kinds remain modal.
	if m.promptKind == "search" {
		m.search = m.prompt.Value()
		m.dirIdx = 0
	}
	return m, cmd
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys

	// Esc cancels armed copy or closes preview without disturbing other state.
	if msg.String() == "esc" {
		if m.copyArmed {
			m.copyArmed = false
			m.copySrc = ""
			m.copyName = ""
			m.copyKind = ""
			m.setStatus("info", "Copy cancelled")
			return m, nil
		}
		if m.previewActive {
			m.previewActive = false
			m.previewText = ""
			m.previewPath = ""
			m.setStatus("info", "Preview closed")
			return m, nil
		}
	}

	switch {
	case key.Matches(msg, k.Quit):
		return m, tea.Quit
	case key.Matches(msg, k.Help):
		m.helpOpen = true
		return m, nil
	case key.Matches(msg, k.Tab):
		m.active = (m.active + 1) % numPanes
		return m, nil
	case key.Matches(msg, k.ShiftTab):
		m.active = (m.active + numPanes - 1) % numPanes
		return m, nil
	case key.Matches(msg, k.Refresh):
		// reload current dir
		delete(m.dirsCache, m.currentDirPath())
		return m, m.loadDir(m.currentDirPath())
	case key.Matches(msg, k.MarkAll):
		for _, e := range m.dirEntries {
			m.marked[m.currentDirPath()+e.Name] = true
		}
		m.setStatus("info", fmt.Sprintf("Marked %d items", len(m.dirEntries)))
		return m, nil
	case key.Matches(msg, k.ClearMarks):
		m.marked = map[string]bool{}
		m.setStatus("info", "Marks cleared")
		return m, nil
	case key.Matches(msg, k.Get):
		return m.startDownload()
	case key.Matches(msg, k.Put):
		return m.startUpload()
	case key.Matches(msg, k.Copy):
		return m.startCopy()
	case key.Matches(msg, k.BulkSend):
		return m.startBulkSend()
	case key.Matches(msg, k.Mount):
		m.setStatus("info", "Mount info: http://100.100.100.100:8080  •  davfs2: sudo mount -t davfs http://100.100.100.100:8080 /mnt/taildrive")
		return m, nil
	case key.Matches(msg, k.Preview):
		return m.togglePreview()
	case key.Matches(msg, k.Delete):
		return m.startDelete()
	}

	// pane-specific movement
	switch m.active {
	case paneCats:
		return m.handleKeyCats(msg)
	case paneSources:
		return m.handleKeyDevs(msg)
	case paneEnvConfig:
		return m.handleKeyEntries(msg, m.envConfigs, &m.envIdx)
	case paneGitLab:
		return m.handleKeyEntries(msg, m.gitlab, &m.gitIdx)
	case paneFiles:
		return m.handleKeyFiles(msg)
	}
	return m, nil
}

// handleKeyEntries — shared Up/Down/Enter handler for the curated panes
// (EnvConfigs + GitLab Repos). Enter routes the entry's path into the
// file pane.
func (m Model) handleKeyEntries(msg tea.KeyMsg, entries []envcfg.Entry, idx *int) (tea.Model, tea.Cmd) {
	k := m.keys
	if len(entries) == 0 {
		return m, nil
	}
	switch {
	case key.Matches(msg, k.Up):
		if *idx > 0 {
			*idx--
		}
	case key.Matches(msg, k.Down):
		if *idx < len(entries)-1 {
			*idx++
		}
	case key.Matches(msg, k.Home):
		*idx = 0
	case key.Matches(msg, k.End):
		*idx = len(entries) - 1
	case key.Matches(msg, k.Enter), key.Matches(msg, k.Right):
		sel := entries[*idx]
		if sel.Path == "" || strings.HasPrefix(sel.Path, "(unset") {
			m.setStatus("warn", sel.Name+" — path not configured. Edit ~/.config/taildrives/*.json")
			return m, nil
		}
		return m.openCuratedPath(sel)
	}
	return m, nil
}

// openCuratedPath resolves an EnvConfig / GitLab entry into a (device,
// dirStack) navigation state and loads it in the file pane.
func (m Model) openCuratedPath(e envcfg.Entry) (tea.Model, tea.Cmd) {
	p := strings.TrimRight(e.Path, "/")
	switch {
	case strings.HasPrefix(p, "local://"):
		rest := strings.TrimPrefix(p, "local://")
		parts := strings.Split(rest, "/")
		m.currentDev = m.indexOfDevice(localDeviceName(m.localHost))
		m.dirStack = parts
	case strings.HasPrefix(p, "/"):
		parts := strings.Split(strings.Trim(p, "/"), "/")
		if len(parts) < 2 {
			m.setStatus("err", "curated path too short: "+p)
			return m, nil
		}
		m.currentDev = m.indexOfDevice(parts[1])
		m.dirStack = parts[2:]
	default:
		m.setStatus("warn", "unrecognized path kind for "+e.Name+": "+p)
		return m, nil
	}
	m.active = paneFiles
	m.setStatus("ok", "Opened "+e.Name+" → "+p)
	return m, m.loadDir(m.currentDirPath())
}

func (m Model) handleKeyCats(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	switch {
	case key.Matches(msg, k.Up):
		if m.catIdx > 0 {
			m.catIdx--
			return m.applyCategory()
		}
	case key.Matches(msg, k.Down):
		if m.catIdx < len(cats.All)-1 {
			m.catIdx++
			return m.applyCategory()
		}
	case key.Matches(msg, k.Home):
		m.catIdx = 0
		return m.applyCategory()
	case key.Matches(msg, k.End):
		m.catIdx = len(cats.All) - 1
		return m.applyCategory()
	case key.Matches(msg, k.Enter), key.Matches(msg, k.Right):
		// Filter is already applied live as you move — Enter / → just
		// hands focus to the next pane.
		m.active = paneSources
	}
	return m, nil
}

// applyCategory commits the highlighted category as the active content
// filter. Share Sources is unchanged (it's not filtered by category) — only
// the file pane re-renders with the new filter applied. The category's
// description goes in the status bar so the user always sees what each
// filter actually does.
func (m Model) applyCategory() (tea.Model, tea.Cmd) {
	c := cats.All[m.catIdx]
	m.catFilter = c.Name
	m.cfg.Category = m.catFilter
	_ = cfg.Save(m.cfg)
	m.setStatus("ok", fmt.Sprintf("%s — %s", m.catFilter, c.Description))
	return m, nil
}

func (m Model) handleKeyDevs(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	devs := m.visibleDevices()
	switch {
	case key.Matches(msg, k.Up):
		if m.devIdx > 0 {
			m.devIdx--
			m.currentDev = m.indexOfDevice(devs[m.devIdx])
			return m, m.loadCurrentDev()
		}
	case key.Matches(msg, k.Down):
		if m.devIdx < len(devs)-1 {
			m.devIdx++
			m.currentDev = m.indexOfDevice(devs[m.devIdx])
			return m, m.loadCurrentDev()
		}
	case key.Matches(msg, k.Home):
		m.devIdx = 0
		if len(devs) > 0 {
			m.currentDev = m.indexOfDevice(devs[0])
		}
		return m, m.loadCurrentDev()
	case key.Matches(msg, k.End):
		if len(devs) > 0 {
			m.devIdx = len(devs) - 1
			m.currentDev = m.indexOfDevice(devs[m.devIdx])
		}
		return m, m.loadCurrentDev()
	case key.Matches(msg, k.Enter), key.Matches(msg, k.Right):
		m.active = paneFiles
	case key.Matches(msg, k.Left):
		m.active = paneCats
	}
	return m, nil
}

func (m Model) handleKeyFiles(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := m.keys
	visible := m.visibleFiles()
	switch {
	case key.Matches(msg, k.Search):
		// Open the search prompt — live-filter on every keystroke (the
		// promptKind="search" branch in updatePrompt re-writes m.search
		// per character rather than waiting on Enter). `s` and `/` both
		// trigger; matches the wizard file browser's `s` shortcut.
		m.promptKind = "search"
		m.prompt.SetValue(m.search)
		m.prompt.Placeholder = "fuzzy-match share / file names…"
		m.prompt.Focus()
		m.promptActive = true
		m.setStatus("info", "Search — type to filter live, Esc clears + closes")
		return m, textinput.Blink
	case key.Matches(msg, k.Up):
		if m.dirIdx > 0 {
			m.dirIdx--
		}
	case key.Matches(msg, k.Down):
		if m.dirIdx < len(visible)-1 {
			m.dirIdx++
		}
	case key.Matches(msg, k.PgUp):
		m.dirIdx -= 10
		if m.dirIdx < 0 {
			m.dirIdx = 0
		}
	case key.Matches(msg, k.PgDn):
		m.dirIdx += 10
		if m.dirIdx >= len(visible) {
			m.dirIdx = len(visible) - 1
		}
	case key.Matches(msg, k.Home):
		m.dirIdx = 0
	case key.Matches(msg, k.End):
		m.dirIdx = len(visible) - 1
	case key.Matches(msg, k.Mark):
		if m.dirIdx < len(visible) {
			full := m.currentDirPath() + visible[m.dirIdx].Name
			if m.marked[full] {
				delete(m.marked, full)
			} else {
				m.marked[full] = true
			}
		}
	case key.Matches(msg, k.Enter), key.Matches(msg, k.Right):
		if m.dirIdx >= len(visible) || !visible[m.dirIdx].IsDir {
			return m, nil
		}
		sel := visible[m.dirIdx]
		// Aggregated entries from Content-Categories view have hrefs like
		// "local://<share>/" or "/<ns>/<dev>/<share>/" and Names like
		// "device  /  share". Drilling switches currentDev + dirStack to
		// land inside that share on its owning device.
		if strings.Contains(sel.Name, "  /  ") {
			return m.drillAggregated(sel)
		}
		m.dirStack = append(m.dirStack, sel.Name)
		return m, m.loadDir(m.currentDirPath())
	case key.Matches(msg, k.Left), key.Matches(msg, k.Back):
		if len(m.dirStack) > 0 {
			m.dirStack = m.dirStack[:len(m.dirStack)-1]
			return m, m.loadDir(m.currentDirPath())
		} else {
			m.active = paneSources
		}
	}
	return m, nil
}

// drillAggregated resolves an aggregate-view entry (href "local://share/" or
// "/ns/dev/share/") to its owning device + share, switches the model state,
// and loads the share's contents.
func (m Model) drillAggregated(sel webdav.Entry) (tea.Model, tea.Cmd) {
	if strings.HasPrefix(sel.Href, "local://") {
		shareName := strings.TrimSuffix(strings.TrimPrefix(sel.Href, "local://"), "/")
		m.currentDev = m.indexOfDevice(localDeviceName(m.localHost))
		m.dirStack = []string{shareName}
		m.active = paneFiles
		return m, m.loadDir(m.currentDirPath())
	}
	// Remote: parse "/ns/dev/share/"
	parts := strings.Split(strings.Trim(sel.Href, "/"), "/")
	if len(parts) < 3 {
		return m, nil
	}
	devName := parts[1]
	shareName := parts[2]
	m.currentDev = m.indexOfDevice(devName)
	m.dirStack = []string{shareName}
	m.active = paneFiles
	return m, m.loadDir(m.currentDirPath())
}

// ── prompts ─────────────────────────────────────────────────────────────────

func (m Model) startDownload() (tea.Model, tea.Cmd) {
	visible := m.visibleFiles()
	if len(visible) == 0 || m.dirIdx >= len(visible) {
		m.setStatus("warn", "Nothing selected to download")
		return m, nil
	}
	sel := visible[m.dirIdx]
	// Aggregate-view entries: "device  /  share" — peel off share name for the
	// default destination basename.
	srcName := sel.Name
	if i := strings.LastIndex(srcName, "  /  "); i >= 0 {
		srcName = srcName[i+len("  /  "):]
	}
	// Use e.Href when set (aggregate or local entries); fall back to the
	// current-dir + name composition for normal entries.
	src := sel.Href
	if src == "" {
		src = m.currentDirPath() + sel.Name
	}
	kind := "file"
	if sel.IsDir {
		kind = "directory (recursive)"
	}
	defaultDst := filepath.Join(homeDir(), "Downloads", srcName)
	m.promptKind = "get"
	m.prompt.SetValue(defaultDst)
	m.prompt.Placeholder = "local destination path"
	m.prompt.Focus()
	m.promptActive = true
	m.setStatus("info", fmt.Sprintf("Download %s %s → %s  (Enter to confirm, Esc to cancel)",
		kind, src, defaultDst))
	return m, textinput.Blink
}

func (m Model) startUpload() (tea.Model, tea.Cmd) {
	m.promptKind = "put"
	m.prompt.SetValue("")
	m.prompt.Placeholder = "local file to upload into " + m.currentDirPath()
	m.prompt.Focus()
	m.promptActive = true
	m.setStatus("info", "Upload — type local path  (Enter to confirm, Esc to cancel)")
	return m, textinput.Blink
}

// hasGodMode returns true if the current operator is allowed to delete.
// Accepts either the canonical Tailscale login email OR an OS-fallback
// form like "archn3m3sis@n3m-srv-02" so tagged servers still match.
func (m Model) hasGodMode() bool {
	if strings.EqualFold(m.loginName, godUser) {
		return true
	}
	// OS fallback form is "<user>@<host>" — match on the user portion.
	if i := strings.Index(m.loginName, "@"); i > 0 {
		return strings.EqualFold(m.loginName[:i], godOSUser)
	}
	return false
}

// startDelete is the D-key handler. Guards with the God-Mode user check,
// then arms a typed-YES confirmation prompt that names the target + warns
// if it's a Taildrive share root (which also gets unshared).
func (m Model) startDelete() (tea.Model, tea.Cmd) {
	if !m.hasGodMode() {
		who := m.loginName
		if who == "" {
			who = "unknown user"
		}
		m.setStatus("err", "Delete refused — God Mode required. Logged-in user is "+who+
			", only "+godUser+" can delete via the TUI.")
		return m, nil
	}
	visible := m.visibleFiles()
	if len(visible) == 0 || m.dirIdx >= len(visible) {
		m.setStatus("warn", "Nothing selected to delete")
		return m, nil
	}
	sel := visible[m.dirIdx]
	src := sel.Href
	if src == "" {
		src = m.currentDirPath() + sel.Name
	}
	kind := "file"
	if sel.IsDir {
		kind = "directory (recursive)"
	}
	// Detect share-root: top-level entry in a per-device view, OR an
	// aggregate-view entry (which is always a share root).
	isShareRoot := false
	shareName := sel.Name
	if i := strings.LastIndex(shareName, "  /  "); i >= 0 {
		shareName = shareName[i+len("  /  "):]
		isShareRoot = true
	}
	if !isShareRoot && len(m.dirStack) == 0 && sel.IsDir {
		isShareRoot = true
	}

	warning := ""
	if isShareRoot {
		kind = "TAILDRIVE SHARE ROOT"
		warning = " — this also unshares the Taildrive registration AND removes the folder on disk"
	}

	m.promptKind = "delete:" + boolStr(isShareRoot, shareName) + ":" + src
	m.prompt.SetValue("")
	m.prompt.Placeholder = `type "YES" to confirm permanent deletion`
	m.prompt.Focus()
	m.promptActive = true
	m.setStatus("warn", fmt.Sprintf("DELETE %s: %s%s — type YES to confirm, Esc to cancel",
		kind, src, warning))
	return m, textinput.Blink
}

// boolStr encodes (bool, str) into "1:str" or "0:str" so the prompt-kind
// string can carry both pieces.
func boolStr(b bool, s string) string {
	if b {
		return "1:" + s
	}
	return "0:" + s
}

// togglePreview opens or closes the file preview split. Only opens if the
// highlighted entry is a file (not a directory).
func (m Model) togglePreview() (tea.Model, tea.Cmd) {
	if m.previewActive {
		m.previewActive = false
		m.previewText = ""
		m.previewPath = ""
		m.setStatus("info", "Preview closed")
		return m, nil
	}
	visible := m.visibleFiles()
	if m.dirIdx >= len(visible) {
		m.setStatus("warn", "Nothing selected to preview")
		return m, nil
	}
	e := visible[m.dirIdx]
	if e.IsDir {
		m.setStatus("warn", "Preview only works on files, not directories")
		return m, nil
	}
	full := e.Href
	if full == "" {
		full = m.currentDirPath() + e.Name
	}
	m.previewActive = true
	m.previewPath = full
	m.previewText = ""
	m.setStatus("info", "Loading preview…")
	return m, m.loadPreview(full)
}

// loadPreview fetches the target file. Image files keep raw bytes so the
// view can re-render at the right pane size; text/binary collapse to a
// pre-formatted string capped at 256 KB.
func (m Model) loadPreview(p string) tea.Cmd {
	const maxBytes = 256 * 1024
	const maxImageBytes = 8 * 1024 * 1024 // 8MB upper bound for image decode
	return func() tea.Msg {
		var data []byte
		var err error
		if strings.HasPrefix(p, "local://") {
			rest := strings.TrimPrefix(p, "local://")
			parts := strings.SplitN(rest, "/", 2)
			shareName := parts[0]
			sub := ""
			if len(parts) > 1 {
				sub = parts[1]
			}
			var fsPath string
			for _, s := range m.localShares {
				if s.Name == shareName {
					fsPath = s.Path
					if sub != "" {
						fsPath = path.Join(s.Path, sub)
					}
					break
				}
			}
			if fsPath == "" {
				return previewLoadedMsg{path: p, err: fmt.Errorf("local share %q not found", shareName)}
			}
			data, err = os.ReadFile(fsPath)
		} else {
			data, err = m.client.Get(p)
		}
		if err != nil {
			return previewLoadedMsg{path: p, err: err}
		}
		total := len(data)

		// Image branch: keep the raw bytes so viewPreviewPane can render at
		// whatever pane dimensions are active. Skip huge images that would
		// stall Go's image.Decode.
		if imgview.IsImage(data, path.Base(p)) {
			if total > maxImageBytes {
				return previewLoadedMsg{
					path: p, bytes: total, kind: "image",
					err: fmt.Errorf("image too large to preview (%s — limit %s)",
						humanBytes(int64(total)), humanBytes(int64(maxImageBytes))),
				}
			}
			return previewLoadedMsg{
				path: p, raw: data, kind: "image", bytes: total,
			}
		}

		// Text/binary branch.
		if total > maxBytes {
			data = data[:maxBytes]
		}
		kind := "text"
		if !isTextish(data) {
			kind = "binary"
		}
		return previewLoadedMsg{
			path:  p,
			text:  renderPreviewContent(data, total > maxBytes),
			kind:  kind,
			bytes: total,
		}
	}
}

// renderPreviewContent decides text vs binary. Text: returned as-is (up to
// maxBytes). Binary: 16-byte-per-row hex+ASCII dump of the first 512 bytes.
func renderPreviewContent(data []byte, truncated bool) string {
	if isTextish(data) {
		out := string(data)
		if truncated {
			out += "\n\n--- truncated at 256 KB ---"
		}
		return out
	}
	// Binary hex dump (first 512 bytes)
	limit := 512
	if len(data) < limit {
		limit = len(data)
	}
	var b strings.Builder
	for i := 0; i < limit; i += 16 {
		end := i + 16
		if end > limit {
			end = limit
		}
		fmt.Fprintf(&b, "%08x  ", i)
		for j := i; j < end; j++ {
			fmt.Fprintf(&b, "%02x ", data[j])
		}
		// pad if last row is short
		for j := end; j < i+16; j++ {
			b.WriteString("   ")
		}
		b.WriteString(" |")
		for j := i; j < end; j++ {
			c := data[j]
			if c < 32 || c > 126 {
				c = '.'
			}
			b.WriteByte(c)
		}
		b.WriteString("|\n")
	}
	b.WriteString("\n--- binary file — showing first 512 bytes ---")
	return b.String()
}

// isTextish returns true if the buffer looks like text (mostly printable
// UTF-8, no NUL bytes in the first ~1KB).
func isTextish(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	scan := data
	if len(scan) > 1024 {
		scan = scan[:1024]
	}
	for _, b := range scan {
		if b == 0 {
			return false
		}
	}
	// >85% printable-or-whitespace = call it text
	printable := 0
	for _, b := range scan {
		if (b >= 32 && b < 127) || b == '\n' || b == '\r' || b == '\t' {
			printable++
		}
	}
	return printable*100/len(scan) >= 85
}

// startCopy is the c-key handler. Two modes:
//
//  1. Nothing armed yet: capture the currently highlighted item as the source
//     and arm copy mode. The user then navigates freely.
//  2. Already armed: the current path is the destination — drop the copy
//     there with the source's basename and clear the armed state.
func (m Model) startCopy() (tea.Model, tea.Cmd) {
	if m.copyArmed {
		return m.dropCopy()
	}
	visible := m.visibleFiles()
	if len(visible) == 0 || m.dirIdx >= len(visible) {
		m.setStatus("warn", "Nothing selected to copy")
		return m, nil
	}
	sel := visible[m.dirIdx]
	m.copyArmed = true
	m.copySrc = m.currentDirPath() + sel.Name
	m.copyName = sel.Name
	m.copyKind = "file"
	if sel.IsDir {
		m.copyKind = "directory"
	}
	m.setStatus("info", fmt.Sprintf(
		"COPY armed (%s): %s — navigate to destination dir, press c again to drop · Esc cancels",
		m.copyKind, m.copySrc))
	return m, nil
}

// dropCopy submits the transfer job from the armed source to the current
// directory. Handles all four (local|remote) × (local|remote) combinations
// by resolving local:// paths to file:// before submitting.
func (m Model) dropCopy() (tea.Model, tea.Cmd) {
	src := m.resolveForXfer(m.copySrc)
	dst := m.resolveForXfer(m.currentDirPath() + m.copyName)
	j := m.xferMgr.Submit(src, dst)
	m.setStatus("ok", fmt.Sprintf(
		"Copy queued — job #%d: %s → %s", j.ID, m.copySrc, m.currentDirPath()+m.copyName))
	m.copyArmed = false
	m.copySrc = ""
	m.copyName = ""
	m.copyKind = ""
	return m, nil
}

// resolveForXfer turns a TUI path into something the xfer engine accepts:
// WebDAV paths pass through unchanged; local:// paths become file:// with
// the filesystem path resolved through m.localShares.
func (m Model) resolveForXfer(p string) string {
	if !strings.HasPrefix(p, "local://") {
		return p
	}
	rest := strings.TrimPrefix(p, "local://")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.SplitN(rest, "/", 2)
	shareName := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	for _, s := range m.localShares {
		if s.Name == shareName {
			full := s.Path
			if sub != "" {
				full = path.Join(s.Path, sub)
			}
			return "file://" + full
		}
	}
	return p // not resolvable; let xfer return an error
}

func (m Model) startBulkSend() (tea.Model, tea.Cmd) {
	if len(m.marked) == 0 {
		m.setStatus("warn", "No items marked. Press Space to mark, then b for bulk-send.")
		return m, nil
	}
	m.promptKind = "bulk"
	m.prompt.SetValue("")
	m.prompt.Placeholder = "destination devices (comma-separated, e.g. n3m-srv-01,n3m-wks-01)"
	m.prompt.Focus()
	m.promptActive = true
	m.setStatus("info", fmt.Sprintf("Bulk-send %d marked → ?  (Enter to fan out)", len(m.marked)))
	return m, textinput.Blink
}

func (m Model) handlePromptResult(kind, val string) (tea.Model, tea.Cmd) {
	if val == "" {
		m.setStatus("warn", "Cancelled (empty input)")
		return m, nil
	}
	switch {
	case kind == "get":
		visible := m.visibleFiles()
		if m.dirIdx >= len(visible) {
			return m, nil
		}
		sel := visible[m.dirIdx]
		// Use e.Href when set (aggregate-view / local entries); fall back to
		// composed path otherwise.
		src := sel.Href
		if src == "" {
			src = m.currentDirPath() + sel.Name
		}
		// Translate local:// → file:// so the xfer engine treats it as a
		// local-to-local copy (works whether it's a file or directory).
		src = m.resolveForXfer(src)
		j := m.xferMgr.Submit(src, "file://"+val)
		kindLabel := "file"
		if sel.IsDir {
			kindLabel = "directory"
		}
		m.setStatus("ok", fmt.Sprintf("Download %s queued — job #%d → %s", kindLabel, j.ID, val))
	case kind == "put":
		j := m.xferMgr.Submit("file://"+val, m.currentDirPath()+filepath.Base(val))
		m.setStatus("ok", fmt.Sprintf("Upload queued — job #%d %s → %s", j.ID, val, m.currentDirPath()))
		// refresh dir after a moment
		return m, tea.Batch(
			tea.Tick(800*time.Millisecond, func(time.Time) tea.Msg {
				return statusMsg{level: "info", text: "Refreshing…"}
			}),
			m.loadDir(m.currentDirPath()),
		)
	case strings.HasPrefix(kind, "copy:"):
		src := strings.TrimPrefix(kind, "copy:")
		j := m.xferMgr.Submit(src, val)
		m.setStatus("ok", fmt.Sprintf("Copy queued — job #%d %s → %s", j.ID, src, val))
	case kind == "bulk":
		var dsts []string
		var marked []string
		for k := range m.marked {
			marked = append(marked, k)
		}
		sort.Strings(marked)
		devs := strings.Split(val, ",")
		total := 0
		for _, src := range marked {
			parts := strings.Split(strings.TrimLeft(src, "/"), "/")
			if len(parts) < 3 {
				continue
			}
			name := parts[len(parts)-1]
			for _, d := range devs {
				d = strings.TrimSpace(d)
				if d == "" {
					continue
				}
				// dst = /namespace/<dev>/<dev>_taildrop/<name>
				dst := "/" + m.namespace + "/" + d + "/" + d + "_taildrop/" + name
				dsts = append(dsts, dst)
				total++
				_ = m.xferMgr.Submit(src, dst)
			}
		}
		m.setStatus("ok", fmt.Sprintf("Bulk-send: %d marked × %d devices = %d jobs queued",
			len(marked), len(devs), total))
		// best-effort: clear marks
		m.marked = map[string]bool{}
	case strings.HasPrefix(kind, "delete:"):
		// kind format: "delete:<0|1>:<shareName_or_unused>:<src>"
		raw := strings.TrimPrefix(kind, "delete:")
		parts := strings.SplitN(raw, ":", 3)
		if len(parts) < 3 {
			m.setStatus("err", "delete: malformed prompt kind")
			return m, nil
		}
		isShareRoot := parts[0] == "1"
		shareName := parts[1]
		src := parts[2]
		if val != "YES" {
			m.setStatus("info", "Delete cancelled (confirmation didn't match YES)")
			return m, nil
		}
		return m.performDelete(src, isShareRoot, shareName)
	}
	return m, nil
}

// performDelete executes the actual deletion against the right backend.
//   - file:// paths   → os.Remove / os.RemoveAll
//   - local://        → resolve via m.localShares → os.Remove / os.RemoveAll
//   - WebDAV path     → DELETE via the WebDAV client
//
// If isShareRoot is set AND the target lives on the local host, we also
// run `tailscale drive unshare <name>` so the Tailscale registration is
// removed in lockstep. For remote share roots we surface a clear note —
// the unshare has to happen on the owning host.
func (m Model) performDelete(src string, isShareRoot bool, shareName string) (tea.Model, tea.Cmd) {
	resolved := m.resolveForXfer(src)
	wasLocal := strings.HasPrefix(resolved, "file://")
	var err error
	switch {
	case wasLocal:
		err = os.RemoveAll(strings.TrimPrefix(resolved, "file://"))
	default:
		err = m.client.Delete(src)
	}
	if err != nil {
		m.setStatus("err", "delete failed: "+err.Error())
		return m, nil
	}
	// Share-root cleanup: unshare the Taildrive registration too.
	unshareNote := ""
	if isShareRoot {
		if wasLocal {
			if e := local.Unshare(shareName); e != nil {
				unshareNote = " (folder deleted, but `tailscale drive unshare " + shareName + "` failed: " + e.Error() + ")"
			} else {
				unshareNote = " (Taildrive share also unshared)"
				// Refresh local shares cache so the synthetic device pane updates.
				m.localShares = nil
				if ls, e := local.ListShares(); e == nil {
					m.localShares = ls
				}
			}
		} else {
			unshareNote = " — REMINDER: ssh into the owning host and run `tailscale drive unshare " + shareName + "` to drop the registration"
		}
	}
	m.setStatus("ok", "Deleted "+src+unshareNote)
	// Reload current dir so the deleted entry vanishes.
	return m, m.loadDir(m.currentDirPath())
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (m *Model) setStatus(level, text string) {
	m.statusLevel = level
	m.statusText = text
}

func (m Model) currentDirPath() string {
	if m.namespace == "" || len(m.devices) == 0 {
		return "/"
	}
	dev := m.devices[m.currentDev]
	if isLocalDevice(dev) {
		// local mode — path is "local://<share>/<sub>/"
		if len(m.dirStack) == 0 {
			return "local://"
		}
		return "local://" + strings.Join(m.dirStack, "/") + "/"
	}
	p := "/" + m.namespace + "/" + dev + "/"
	if len(m.dirStack) > 0 {
		p += strings.Join(m.dirStack, "/") + "/"
	}
	return p
}

func (m Model) loadCurrentDev() tea.Cmd {
	m.dirStack = nil
	// Skip WebDAV PROPFIND for known-offline devices — it'll just time out
	// after 30s with the user staring at a frozen screen.
	if len(m.devices) > 0 {
		cur := m.devices[m.currentDev]
		stripped := stripLocalMarker(cur)
		if st, ok := m.deviceState[stripped]; ok && !st.Online && !isLocalDevice(cur) {
			// synthesize an empty dir-loaded msg pointing at the current path
			p := m.currentDirPath()
			return func() tea.Msg { return dirLoadedMsg{path: p, entries: nil} }
		}
	}
	return m.loadDir(m.currentDirPath())
}

// visibleDevices returns the Share Sources list. It is INTENTIONALLY not
// filtered by the active content category — content filtering only narrows
// the file pane on the right. Share Sources just shows every device that
// publishes (or has published) Taildrive shares.
//
//   - Local host: always included (you are always your own share source).
//   - Other devices: included if they currently publish a share, OR if
//     Tailscale knows them and they're offline (so wks-02 stays visible).
//   - Mobile / Apple TV are already excluded upstream in loadRoot.
func (m Model) visibleDevices() []string {
	var out []string
	for _, d := range m.devices {
		if isLocalDevice(d) {
			out = append(out, d)
			continue
		}
		hasShares := len(m.deviceShares[d]) > 0
		offline := false
		if st, ok := m.deviceState[stripLocalMarker(d)]; ok && !st.Online {
			offline = true
		}
		if hasShares || offline {
			out = append(out, d)
		}
	}
	return out
}

// deviceMatchesCategory returns true if the device or any of its shares
// satisfies the category's regex filters.
func deviceMatchesCategory(c cats.Category, device string, shares []string) bool {
	device = stripLocalMarker(device)
	// Pure device-name category (e.g. "servers" → ^n3m-srv-\d+$).
	if c.DeviceRE != nil && c.ShareRE == nil {
		return c.DeviceRE.MatchString(device)
	}
	// Pure share-name category (e.g. "taildrops" → *_taildrop).
	if c.DeviceRE == nil && c.ShareRE != nil {
		for _, sh := range shares {
			if c.ShareRE.MatchString(sh) {
				return true
			}
		}
		return false
	}
	// Both — device must match AND have at least one matching share.
	if c.DeviceRE != nil && c.ShareRE != nil {
		if !c.DeviceRE.MatchString(device) {
			return false
		}
		for _, sh := range shares {
			if c.ShareRE.MatchString(sh) {
				return true
			}
		}
		return false
	}
	// Neither regex set — that's "No Active Content Filtering".
	return true
}

func (m Model) indexOfDevice(name string) int {
	for i, d := range m.devices {
		if d == name {
			return i
		}
	}
	return 0
}

func (m Model) visibleFiles() []webdav.Entry {
	// AGGREGATE MODE: when the user is browsing Content Categories, the file
	// pane shows EVERY matching share from EVERY device — a flat tailnet-
	// wide view. As of 2026-05-18 this returns ALL shares (matching +
	// non-matching), with category-filtering handled visually via
	// isInActiveCategory + grey styling in the renderer. That way the
	// operator can SEE that other content exists on the tailnet rather
	// than feeling like shares vanished. Search filtering still hides
	// (intentional user input narrows the visible set).
	if m.active == paneCats && len(m.dirStack) == 0 {
		visible := m.aggregatedShares()
		visible = fuzzyFilterEntries(visible, m.search, m.searchDirsOnly)
		return visible
	}

	visible := m.dirEntries
	// Per-device root: previously filtered here too — now leave the full
	// list and let the renderer dim non-matching entries. Search still
	// narrows since it's explicit user input.
	visible = fuzzyFilterEntries(visible, m.search, m.searchDirsOnly)
	return visible
}

// fuzzyFilterEntries narrows a webdav.Entry slice via fuzzy match (same
// sahilm/fuzzy lib the wizard file browser uses for its `s` overlay).
// Empty query returns the input unchanged (subject to dirsOnly). When
// dirsOnly is set, non-directory entries are dropped from the candidate
// pool BEFORE the fuzzy match runs, so dir matches don't get drowned out.
// Match-rank order from the fuzzy lib is preserved — best matches first.
func fuzzyFilterEntries(entries []webdav.Entry, q string, dirsOnly bool) []webdav.Entry {
	pool := entries
	if dirsOnly {
		pool = pool[:0:0]
		for _, e := range entries {
			if e.IsDir {
				pool = append(pool, e)
			}
		}
	}
	if q == "" {
		return pool
	}
	names := make([]string, len(pool))
	for i, e := range pool {
		names[i] = e.Name
	}
	matches := fuzzy.Find(q, names)
	out := make([]webdav.Entry, 0, len(matches))
	for _, m := range matches {
		out = append(out, pool[m.Index])
	}
	return out
}

// isInActiveCategory reports whether the entry passes the currently
// selected content-category filter. Used by the renderer to decide
// dim-vs-bright styling. "No Active Content Filtering" passes everything.
// Sub-directories inside a share are always considered matching (the
// category filter only constrains TOP-LEVEL share names).
func (m Model) isInActiveCategory(e webdav.Entry) bool {
	if m.catFilter == "" || m.catFilter == "No Active Content Filtering" {
		return true
	}
	if len(m.dirStack) > 0 {
		// Inside a share — past the level the filter operates on.
		return true
	}
	c, ok := cats.ByName(m.catFilter)
	if !ok {
		return true
	}
	// Aggregate names look like "device  /  share" — extract the share part.
	shareName := e.Name
	if idx := strings.LastIndex(shareName, "  /  "); idx >= 0 {
		shareName = shareName[idx+len("  /  "):]
	}
	if c.ShareRE != nil {
		return c.ShareRE.MatchString(shareName)
	}
	// Device-only categories: in aggregate view they used to drop everything;
	// now they drop nothing visually. Renderer will grey based on device match.
	return true
}

// aggregatedShares flattens every (device, share) pair across the tailnet
// into a single list. Names are shown as "device / share" so the user can
// tell where each came from. Drilling into one resolves the href back to
// the right device + share path. Category filtering is NOT applied here
// anymore (as of 2026-05-18) — the renderer dims non-matching entries
// instead so the operator can SEE every share that exists on the mesh.
func (m Model) aggregatedShares() []webdav.Entry {
	var out []webdav.Entry
	for _, dev := range m.devices {
		shares := m.deviceShares[dev]
		for _, sh := range shares {
			label := stripLocalMarker(dev) + "  /  " + sh
			var href string
			if isLocalDevice(dev) {
				href = "local://" + sh + "/"
			} else {
				href = "/" + m.namespace + "/" + dev + "/" + sh + "/"
			}
			out = append(out, webdav.Entry{
				Href:  href,
				Name:  label,
				IsDir: true,
			})
		}
	}
	return out
}

func (m Model) currentDeviceName() string {
	if len(m.devices) == 0 {
		return ""
	}
	return m.devices[m.currentDev]
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// deviceRank orders devices so the useful ones (servers, the user, workstations)
// come before Apple TV / mobile devices that don't host Taildrive shares.
// The "(this device)" suffix on the local host outranks everything else so
// the local shares are the top entry.
func deviceRank(name string) int {
	if strings.Contains(name, "(this device)") {
		return 1
	}
	switch {
	case strings.HasPrefix(name, "n3m-srv-"):
		return 10
	case strings.HasPrefix(name, "n3m-wks-"):
		return 20
	case strings.HasPrefix(name, "n3m-mob-"):
		return 80
	case strings.HasPrefix(name, "n3m-ent-"):
		return 90
	default:
		return 5 // bare user name like "archn3m3sis"
	}
}

const localMarker = " ★ (this device)"

func localDeviceName(host string) string { return host + localMarker }

func isLocalDevice(name string) bool { return strings.Contains(name, localMarker) }

func stripLocalMarker(name string) string {
	return strings.TrimSuffix(name, localMarker)
}

// isMobileOrEnt returns true for device classes that are never Share Sources —
// phones, tablets, Apple TVs, etc. Filtering these keeps the Share Sources
// pane focused on devices that actually publish Taildrive shares.
func isMobileOrEnt(name string) bool {
	return strings.HasPrefix(name, "n3m-mob-") || strings.HasPrefix(name, "n3m-ent-")
}

// ── view ────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	if m.w == 0 || m.h == 0 {
		return "initializing…"
	}
	header := m.viewHeader()
	footer := m.viewFooter()
	statusBar := m.viewStatusBar()

	bodyH := m.h - lipgloss.Height(header) - lipgloss.Height(footer) - lipgloss.Height(statusBar)
	if bodyH < 8 {
		bodyH = 8
	}

	// New 2-column layout, left column is 4-stacked:
	//   LEFT  (stacked): Content Categories · Share Sources ·
	//                    Environment Configurations · GitLab Repositories
	//   RIGHT (stacked): Shares & Files · Details
	leftW := 34 // wide enough for "Environment Configurations  (N)" without wrapping
	rightW := m.w - leftW

	// Fixed-ish heights for the 4 left panes. Sources gets any leftover
	// because it's the most variable in content.
	catsH := 14 // 9 categories + title + borders
	envH := 8   // 2-3 entries typical
	gitH := 8
	sourcesH := bodyH - catsH - envH - gitH
	if sourcesH < 6 {
		// Terminal is short — give each pane a more even share.
		catsH = bodyH / 4
		envH = bodyH / 4
		gitH = bodyH / 4
		sourcesH = bodyH - catsH - envH - gitH
	}

	previewH := 8
	filesH := bodyH - previewH

	cats := m.viewCats(leftW, catsH)
	sources := m.viewSources(leftW, sourcesH)
	envc := m.viewEnvConfigs(leftW, envH)
	git := m.viewGitLab(leftW, gitH)
	left := lipgloss.JoinVertical(lipgloss.Left, cats, sources, envc, git)

	files := m.viewFiles(rightW, filesH)
	preview := m.viewPreview(rightW, previewH)
	right := lipgloss.JoinVertical(lipgloss.Left, files, preview)

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, right)

	main := lipgloss.JoinVertical(lipgloss.Left, header, body, footer, statusBar)

	if m.helpOpen {
		return overlay(main, m.viewHelp(), m.w, m.h)
	}
	if m.promptActive {
		return overlay(main, m.viewPrompt(), m.w, m.h)
	}
	return main
}

func (m Model) viewHeader() string {
	left := theme.Banner.Render("TAILDRIVES")
	mid := theme.Title.Render(" · n3m3sis devnet")
	hostMarker := ""
	if m.localHost != "" {
		n := len(m.localShares)
		hostMarker = "  " + theme.MagentaS.Render("★ "+m.localHost) +
			theme.ItemDim.Render(fmt.Sprintf(" (%d local)", n))
	}
	right := theme.ItemDim.Render("filter: ") + theme.Info.Render(m.catFilter)
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(mid) - lipgloss.Width(hostMarker) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	line := left + mid + hostMarker + strings.Repeat(" ", gap) + right
	// hard-truncate to one line so it never wraps and shrinks bodyH.
	line = truncate(line, m.w-2)
	return lipgloss.NewStyle().
		Background(theme.BgPanel).
		Foreground(theme.Text).
		Padding(0, 1).
		Width(m.w).
		Render(line)
}

func (m Model) viewCats(w, h int) string {
	body := renderList(cats.Names(), m.catIdx, w-4, h-3, m.active == paneCats, func(name string, _ int) string {
		if name == m.catFilter {
			return "● " + theme.AccentHiS.Render(name)
		}
		return "  " + name
	})
	return panel("Content Categories", "", body, w, h, m.active == paneCats)
}

func (m Model) viewEnvConfigs(w, h int) string {
	if len(m.envConfigs) == 0 {
		body := theme.ItemDim.Render("  (none registered)\n  edit ~/.config/taildrives/env-configs.json")
		return panel("Environment Configurations", "", body, w, h, m.active == paneEnvConfig)
	}
	body := renderList(m.envConfigs, m.envIdx, w-4, h-3, m.active == paneEnvConfig, func(e envcfg.Entry, _ int) string {
		return "  " + e.Name
	})
	subtitle := fmt.Sprintf("  (%d)", len(m.envConfigs))
	return panel("Environment Configurations", subtitle, body, w, h, m.active == paneEnvConfig)
}

func (m Model) viewGitLab(w, h int) string {
	if len(m.gitlab) == 0 {
		body := theme.ItemDim.Render("  (none registered)\n  edit ~/.config/taildrives/gitlab-repos.json")
		return panel("GitLab Repositories", "", body, w, h, m.active == paneGitLab)
	}
	body := renderList(m.gitlab, m.gitIdx, w-4, h-3, m.active == paneGitLab, func(e envcfg.Entry, _ int) string {
		return "  " + e.Name
	})
	subtitle := fmt.Sprintf("  (%d)", len(m.gitlab))
	return panel("GitLab Repositories", subtitle, body, w, h, m.active == paneGitLab)
}

func (m Model) viewSources(w, h int) string {
	devs := m.visibleDevices()
	body := renderList(devs, m.devIdx, w-4, h-3, m.active == paneSources, func(d string, _ int) string {
		marker := "  "
		if i := m.indexOfDevice(d); i == m.currentDev {
			marker = "▸ "
		}
		name := d
		st := m.deviceState[stripLocalMarker(d)]
		if !st.Online && m.deviceState != nil && hasDeviceState(m.deviceState, d) {
			// Offline device — dim it and append last-seen
			label := theme.ItemDim.Render(name)
			suffix := ""
			if st.LastSeen != "" {
				suffix = theme.ItemDim.Render(" (offline " + st.LastSeen + ")")
			} else {
				suffix = theme.ItemDim.Render(" (offline)")
			}
			return marker + label + suffix
		}
		if marker == "▸ " {
			return marker + theme.AccentHiS.Render(name)
		}
		return marker + name
	})
	if len(devs) == 0 {
		body = theme.ItemDim.Render("  (no devices match this category)")
	}
	subtitle := ""
	if n := len(devs); n > 0 {
		subtitle = fmt.Sprintf("  (%d)", n)
	}
	return panel("Share Sources", subtitle, body, w, h, m.active == paneSources)
}

func hasDeviceState(m map[string]devState, name string) bool {
	if _, ok := m[name]; ok {
		return true
	}
	if _, ok := m[stripLocalMarker(name)]; ok {
		return true
	}
	return false
}

func (m Model) isCurrentDeviceOffline() bool {
	if len(m.devices) == 0 {
		return false
	}
	name := stripLocalMarker(m.devices[m.currentDev])
	if st, ok := m.deviceState[name]; ok {
		return !st.Online
	}
	return false
}

func (m Model) viewFiles(w, h int) string {
	// PREVIEW MODE: split horizontally. Left = file listing (narrower).
	// Right = file content. Built recursively by calling viewFiles for the
	// list half + viewPreviewPane for the content half.
	if m.previewActive {
		listW := w * 4 / 10 // 40% for the listing
		if listW < 30 {
			listW = 30
		}
		previewW := w - listW
		listPane := m.viewFilesList(listW, h)
		previewPane := m.viewPreviewPane(previewW, h)
		return lipgloss.JoinHorizontal(lipgloss.Top, listPane, previewPane)
	}
	return m.viewFilesList(w, h)
}

func (m Model) viewFilesList(w, h int) string {
	subtitle := ""
	title := "Shares & Files"
	if m.active == paneCats && len(m.dirStack) == 0 {
		if m.catFilter == "No Active Content Filtering" {
			title = "Every share across the tailnet"
		} else {
			title = "Every " + m.catFilter + " share across the tailnet"
		}
		subtitle = "  (Enter to drill into one · Tab → per-device view)"
	} else if m.namespace != "" && len(m.devices) > 0 {
		subtitle = "  " + truncate(m.currentDirPath(), w-22)
	}
	visible := m.visibleFiles()
	if m.dirIdx >= len(visible) {
		m.dirIdx = len(visible) - 1
		if m.dirIdx < 0 {
			m.dirIdx = 0
		}
	}
	if len(visible) == 0 {
		empty := theme.ItemDim.Render("  (empty)")
		if m.loading {
			empty = "  " + m.spin.View() + " loading…"
		}
		// macOS-friendly hint: local-host on this device, no shares because
		// the GUI app doesn't expose `tailscale drive` CLI.
		cur := ""
		if len(m.devices) > 0 {
			cur = m.devices[m.currentDev]
		}
		if isLocalDevice(cur) && len(m.localShares) == 0 {
			empty = "  " + theme.Warn.Render("Local shares can't be enumerated from this device.") + "\n" +
				"  " + theme.ItemDim.Render("On macOS the GUI Tailscale.app does not expose the `drive` CLI.") + "\n" +
				"  " + theme.ItemDim.Render("Manage shares via Tailscale.app → Settings → File Sharing.") + "\n" +
				"  " + theme.ItemDim.Render("Other tailnet devices CAN see and access your shares normally.")
		}
		// Offline device hint
		if hasDeviceState(m.deviceState, cur) {
			st := m.deviceState[stripLocalMarker(cur)]
			if !st.Online {
				suffix := ""
				if st.LastSeen != "" {
					suffix = " — last seen " + st.LastSeen
				}
				empty = "  " + theme.Warn.Render(stripLocalMarker(cur)+" is offline"+suffix) + "\n" +
					"  " + theme.ItemDim.Render("Bring it online to browse its shares.")
			}
		}
		return panel(title, subtitle, empty, w, h, m.active == paneFiles)
	}
	// Show share descriptions inline whenever we're looking at the top-level
	// of a device (i.e. each entry IS a share) or in the aggregate view.
	// Sub-directories inside a share get the plain (name + size) format.
	showDesc := len(m.dirStack) == 0 && m.namespace != ""
	innerW := w - 4

	// Tally matching / filtered for the status bar.
	matchCount, filterCount := 0, 0
	for _, e := range visible {
		if m.isInActiveCategory(e) {
			matchCount++
		} else {
			filterCount++
		}
	}

	// Reserve one line at the bottom for the filter status bar (when a
	// category is active) — pass h-4 instead of h-3 to renderList so the
	// scrolling math has room.
	listH := h - 3
	hasFilterBar := m.catFilter != "" && m.catFilter != "No Active Content Filtering"
	if hasFilterBar {
		listH--
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#525252"))

	body := renderList(visible, m.dirIdx, innerW, listH, m.active == paneFiles, func(_ webdav.Entry, i int) string {
		e := visible[i]
		matching := m.isInActiveCategory(e)
		full := m.currentDirPath() + e.Name
		mark := "  "
		if m.marked[full] {
			mark = theme.ItemMarked.Render("● ")
		}
		name := e.Name
		var styled string
		switch {
		case !matching:
			// Dim everything for filtered-out rows — name, suffix, the lot.
			// Operator can still see it exists; visually it recedes.
			if e.IsDir {
				styled = dimStyle.Render(name + "/")
			} else {
				styled = dimStyle.Render(name)
			}
		case e.IsDir:
			styled = theme.Dir.Render(name + "/")
		default:
			styled = theme.File.Render(name)
		}
		if showDesc && e.IsDir {
			// Aggregate-view names are "device  /  share" — extract just the
			// share for description lookup. styled already has the trailing
			// "/" so don't add another.
			shareName := e.Name
			if idx := strings.LastIndex(shareName, "  /  "); idx >= 0 {
				shareName = shareName[idx+len("  /  "):]
			}
			summary := desc.Short(shareName)
			if !matching {
				summary = dimStyle.Render(summary)
			}
			return renderWithLeader(mark+styled, summary, innerW)
		}
		size := ""
		if !e.IsDir {
			if matching {
				size = "  " + theme.Size.Render(humanBytes(e.Size))
			} else {
				size = "  " + dimStyle.Render(humanBytes(e.Size))
			}
		}
		return mark + styled + size
	})
	if hasFilterBar {
		body += "\n" + m.renderFilterStatusBar(matchCount, filterCount, innerW)
	}
	return panel(title, subtitle, body, w, h, m.active == paneFiles)
}

// renderFilterStatusBar produces the one-line bottom bar that summarizes
// the active category filter and the live counts. Styled as a tag-strip
// to match the wizard step badges so the visual language stays consistent.
func (m Model) renderFilterStatusBar(matching, filtered, width int) string {
	filterBadge := lipgloss.NewStyle().
		Background(lipgloss.Color("#7c3aed")). // violet-600
		Foreground(lipgloss.Color("#ffffff")).
		Bold(true).
		Padding(0, 1).
		Render("FILTER · " + m.catFilter)
	matchBadge := lipgloss.NewStyle().
		Background(lipgloss.Color("#15803d")). // green-700
		Foreground(lipgloss.Color("#ffffff")).
		Bold(true).
		Padding(0, 1).
		Render(fmt.Sprintf("✓ %d shown", matching))
	filteredBadge := lipgloss.NewStyle().
		Background(lipgloss.Color("#404040")). // neutral-700
		Foreground(lipgloss.Color("#a3a3a3")).
		Padding(0, 1).
		Render(fmt.Sprintf("◌ %d filtered", filtered))
	bar := filterBadge + "  " + matchBadge + "  " + filteredBadge
	return "  " + bar
}

// viewPreviewPane renders the right-half preview content pane shown when
// P has been pressed on a file. Handles text, hex-dumped binary, and
// truecolor half-block image rendering at the active pane size.
func (m Model) viewPreviewPane(w, h int) string {
	title := "Preview"
	if m.previewKind != "" {
		title = "Preview · " + m.previewKind
	}
	subtitle := "  " + truncate(m.previewPath, w-16)
	innerW := w - 4
	innerH := h - 3

	var body string
	switch {
	case m.previewErr != nil:
		body = theme.Err.Render("✗ ") + theme.Item.Render(m.previewErr.Error()) +
			"\n\n" + theme.ItemDim.Render("Press P or Esc to close.")
	case m.previewKind == "image" && len(m.previewRaw) > 0:
		// Kitty graphics protocol path — preferred when the terminal
		// supports it (Ghostty, Kitty, WezTerm). Renders actual pixels at
		// pane resolution instead of the ANSI half-block approximation
		// (which gives ~80x80 visible pixels — unrecognizable for photos).
		// Falls back to the half-block render only if Kitty graphics
		// isn't supported in the current terminal.
		if kittyimg.Supported() {
			body = renderKittyPreview(m.previewRaw, innerW, innerH)
		} else {
			rendered, err := imgview.Render(m.previewRaw, innerW, innerH)
			if err != nil {
				body = theme.Err.Render("image decode failed: ") + err.Error()
			} else {
				body = rendered
			}
		}
	case m.previewText == "":
		body = "  " + m.spin.View() + " loading…"
	default:
		lines := strings.Split(m.previewText, "\n")
		if len(lines) > innerH {
			more := len(lines) - innerH + 1
			lines = lines[:innerH-1]
			lines = append(lines, theme.ItemDim.Render(fmt.Sprintf("… %d more lines, P to close", more)))
		}
		for i, ln := range lines {
			if lipgloss.Width(ln) > innerW {
				lines[i] = truncate(ln, innerW)
			}
		}
		body = strings.Join(lines, "\n")
	}
	return panel(title, subtitle, body, w, h, false)
}

// renderKittyPreview produces the Kitty graphics protocol escape sequence
// for the currently-loaded preview image, sized to fit the supplied inner
// pane dimensions. Decodes+encodes per call (no cache) — Bubble Tea's
// View() is supposed to be pure, so the caching layer (if it becomes a
// perf bottleneck) belongs in the Update side, computed when previewRaw
// arrives. For typical thumbnail sizes this is <100ms.
//
// The graphics sequence draws + advances the cursor by the image's cell
// height. We pad the remaining body height with blank lines so the panel
// fills correctly, then drop a one-line status at the bottom.
func renderKittyPreview(raw []byte, innerW, innerH int) string {
	const cellW, cellH = 9, 18 // Ghostty defaults — close enough
	png, cols, rows, err := kittyimg.DecodeAndEncode(raw, innerW, innerH, cellW, cellH)
	if err != nil {
		return theme.Err.Render("kitty encode failed: ") + err.Error()
	}
	seq := kittyimg.Sequence(png, cols, rows)
	pad := innerH - rows - 1
	if pad < 0 {
		pad = 0
	}
	return seq + strings.Repeat("\n", pad) +
		theme.ItemDim.Render(fmt.Sprintf("  %dx%d cells · Press P or Esc to close", cols, rows))
}

func (m Model) viewPreview(w, h int) string {
	visible := m.visibleFiles()
	if len(visible) == 0 || m.dirIdx >= len(visible) {
		jobs := m.xferMgr.Snapshots()
		if len(jobs) == 0 {
			return panel("Details", "", theme.ItemDim.Render("nothing selected"), w, h, false)
		}
		var lines []string
		for i, j := range jobs {
			if i >= h-3 {
				break
			}
			lines = append(lines, formatJob(j, w-4))
		}
		return panel("Transfers", fmt.Sprintf("  (%d)", len(jobs)), strings.Join(lines, "\n"), w, h, false)
	}
	e := visible[m.dirIdx]
	// For aggregate-view entries, e.Href already holds the proper path
	// (either local:// or /ns/dev/share). For regular per-device entries
	// it's currentDirPath + name.
	full := e.Href
	if full == "" {
		full = m.currentDirPath() + e.Name
	}
	shareName := e.Name
	if i := strings.LastIndex(shareName, "  /  "); i >= 0 {
		shareName = shareName[i+len("  /  "):]
	}
	classes := cats.Classify(m.currentDeviceName(), shareName)
	lines := []string{
		theme.Item.Render("name:        ") + theme.AccentHiS.Render(shareName),
		theme.Item.Render("path:        ") + theme.ItemDim.Render(truncate(full, w-16)),
		theme.Item.Render("type:        ") + map3(e.IsDir, theme.Dir.Render("directory"), theme.File.Render("file")),
		theme.Item.Render("size:        ") + theme.Item.Render(humanBytes(e.Size)),
		theme.Item.Render("categories:  ") + theme.Item.Render(strings.Join(classes, ", ")),
	}
	// Description for shares (top-level dirs). Wrap to fit the pane.
	if e.IsDir {
		descText := desc.Describe(shareName)
		lines = append(lines, "", theme.Item.Render("description: ")+theme.ItemDim.Render(wrapText(descText, w-16)))
	}
	if jobs := m.xferMgr.Snapshots(); len(jobs) > 0 {
		lines = append(lines, "", theme.ItemDim.Render("last transfer: ")+formatJob(jobs[0], w-20))
	}
	return panel("Details", "", strings.Join(lines, "\n"), w, h, false)
}

// wrapText hard-wraps a string to width w on word boundaries, prepending
// 13 spaces (label width) to continuation lines so things line up under
// the "description: " label.
func wrapText(s string, w int) string {
	if w < 20 {
		return s
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return s
	}
	indent := strings.Repeat(" ", 13)
	var b strings.Builder
	line := words[0]
	for _, word := range words[1:] {
		if lipgloss.Width(line)+1+lipgloss.Width(word) > w {
			b.WriteString(line + "\n" + indent)
			line = word
		} else {
			line += " " + word
		}
	}
	b.WriteString(line)
	return b.String()
}

func map3(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func (m Model) viewFooter() string {
	// Layout: navigation (cyan, left) + three colored action groups (right):
	//   📁 DIR-ONLY    (purple)
	//   📄 FILE-ONLY   (cyan)
	//   📁📄 BOTH      (yellow)
	//   ⚠ DESTRUCTIVE  (red) — only shown if hasGodMode
	//
	// Each group is separated by a vertical bar. The group's color carries
	// the meaning so the eye can scan instantly.

	navHints := []string{
		theme.KeyHint.Render("Tab") + theme.KeyDesc.Render(":pane"),
		theme.KeyHint.Render("↑↓") + theme.KeyDesc.Render(":move"),
		theme.KeyHint.Render("→") + theme.KeyDesc.Render(":drill"),
		theme.KeyHint.Render("←") + theme.KeyDesc.Render(":back"),
		theme.KeyHint.Render("r") + theme.KeyDesc.Render(":refresh"),
		theme.KeyHint.Render("?") + theme.KeyDesc.Render(":help"),
		theme.KeyHint.Render("q") + theme.KeyDesc.Render(":quit"),
	}
	navLine := strings.Join(navHints, "  ")

	dirGroup := actionGroup("📁 DIR",
		theme.Purple,
		[][2]string{{"c", "copy"}, {"b", "bulk"}})
	fileGroup := actionGroup("📄 FILE",
		theme.Accent,
		[][2]string{{"P", "preview"}, {"d", "download"}, {"u", "upload"}})
	bothGroup := actionGroup("⇄ BOTH",
		theme.Yellow,
		[][2]string{{"Space", "mark"}, {"M", "mark-all"}})

	groups := []string{dirGroup, fileGroup, bothGroup}
	if m.hasGodMode() {
		danger := actionGroup("⚠ GOD",
			theme.Red,
			[][2]string{{"D", "delete"}})
		groups = append(groups, danger)
	}
	actionLine := strings.Join(groups, "  ")

	gap := m.w - lipgloss.Width(navLine) - lipgloss.Width(actionLine) - 4
	if gap < 2 {
		gap = 2
	}
	full := navLine + strings.Repeat(" ", gap) + actionLine
	full = truncate(full, m.w-2)
	return lipgloss.NewStyle().
		Background(theme.BgPanel).
		Foreground(theme.TextMuted).
		Padding(0, 1).
		Width(m.w).
		Render(full)
}

// actionGroup formats a labeled, color-coded cluster of keybindings:
//
//	「LABEL key:desc  key:desc」
//
// The label and keys take the group's color (bold); descriptions are
// faint in the same color so the cluster reads as one unit.
func actionGroup(label string, color lipgloss.Color, items [][2]string) string {
	lbl := lipgloss.NewStyle().Foreground(color).Bold(true).Reverse(true).Render(" " + label + " ")
	key := lipgloss.NewStyle().Foreground(color).Bold(true)
	desc := lipgloss.NewStyle().Foreground(color).Faint(true)
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, key.Render(it[0])+desc.Render(":"+it[1]))
	}
	return lbl + " " + strings.Join(parts, " ")
}

func (m Model) viewStatusBar() string {
	var st lipgloss.Style
	switch m.statusLevel {
	case "ok":
		st = theme.OK
	case "err":
		st = theme.Err
	case "warn":
		st = theme.Warn
	default:
		st = theme.Info
	}
	left := st.Render(strings.ToUpper(m.statusLevel))
	text := " " + m.statusText
	// Sticky copy-armed banner — overrides the transient status so the user
	// always sees what's in their clipboard.
	if m.copyArmed {
		left = theme.PinkS.Render("COPY ARMED")
		text = " " + theme.ItemMarked.Render(m.copyKind+" "+m.copySrc) +
			theme.ItemDim.Render("  → navigate to destination, press ") +
			theme.AccentHiS.Render("c") +
			theme.ItemDim.Render(" to drop · ") +
			theme.AccentHiS.Render("Esc") +
			theme.ItemDim.Render(" cancels")
	}
	jobs := ""
	if m.xferMgr != nil {
		snaps := m.xferMgr.Snapshots()
		var run, done, fail int
		for _, s := range snaps {
			switch s.State {
			case xfer.Running, xfer.Queued:
				run++
			case xfer.Done:
				done++
			case xfer.Failed:
				fail++
			}
		}
		if run+done+fail > 0 {
			jobs = fmt.Sprintf("  jobs: %s %d  %s %d  %s %d",
				theme.Warn.Render("●"), run,
				theme.OK.Render("●"), done,
				theme.Err.Render("●"), fail)
		}
	}
	marks := ""
	if n := len(m.marked); n > 0 {
		marks = theme.ItemMarked.Render(fmt.Sprintf("  ● %d marked", n))
	}
	full := left + text + marks + jobs
	full = truncate(full, m.w-2)
	return lipgloss.NewStyle().
		Background(theme.Bg).
		Foreground(theme.Text).
		Padding(0, 1).
		Width(m.w).
		Render(full)
}

func (m Model) viewHelp() string {
	help := []string{
		theme.Banner.Render("taildrives — help"),
		"",
		theme.Title.Render("navigation"),
		"  Tab / Shift-Tab     cycle panes (Categories → Devices → Files)",
		"  ↑ ↓ / j k           move within active pane",
		"  → / l   Enter       drill in (next pane, or into a folder)",
		"  ← / h   Backspace   go back (parent folder or previous pane)",
		"  g / G               jump to top / bottom",
		"  Ctrl-U / Ctrl-D     page up / page down",
		"",
		theme.Title.Render("selection"),
		"  Space               mark / unmark current file",
		"  Ctrl-A              mark all in current view",
		"  Ctrl-X              clear all marks",
		"",
		theme.Title.Render("actions"),
		"  d                   download current file or directory",
		"  u                   upload a local file into the current dir",
		"  c                   ARM copy: 1st press captures the highlighted item,",
		"                      navigate to destination dir, 2nd c drops it there",
		"                      Esc cancels the armed copy",
		"  b                   bulk-send marked files to one or more devices",
		"  r / F5              refresh current directory",
		"  m                   show mount info (davfs2 command)",
		"",
		theme.Title.Render("filtering"),
		"  Categories pane     pick filter → automatically prunes devices+shares",
		"",
		theme.Title.Render("global"),
		"  ?                   this help",
		"  q / Ctrl-C          quit",
		"",
		theme.ItemDim.Render("press ? or Esc to close"),
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(theme.Magenta).
		Background(theme.BgPanel).
		Padding(1, 3).
		Render(strings.Join(help, "\n"))
	return box
}

func (m Model) viewPrompt() string {
	body := []string{
		theme.Title.Render("Input required"),
		"",
		theme.ItemDim.Render(m.prompt.Placeholder),
		m.prompt.View(),
		"",
		theme.ItemDim.Render("Enter to confirm · Esc to cancel"),
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(theme.Magenta).
		Background(theme.BgPanel).
		Padding(1, 3).
		Width(min(m.w-10, 90)).
		Render(strings.Join(body, "\n"))
}

// ── small rendering helpers ─────────────────────────────────────────────────

// panel renders a bordered pane of EXACTLY (w cols × h rows) including the
// rounded border. Title is the bare label — color applied per active state.
// Subtitle (optional) is appended to the title line with muted styling.
// renderWithLeader formats a single row as `name.....summary`. The summary
// is capped so the dot leader doesn't blow up to absurd widths on wide
// terminals — the file pane stays scannable.
func renderWithLeader(name, summary string, totalW int) string {
	const maxSummary = 70 // hard cap on summary cell so leader stays modest
	const minDots = 4
	nameW := lipgloss.Width(name)
	// Cap summary width to min(maxSummary, available - name - leader)
	avail := totalW - nameW - 2 - minDots
	if avail < 20 {
		return name
	}
	summaryW := avail
	if summaryW > maxSummary {
		summaryW = maxSummary
	}
	if lipgloss.Width(summary) > summaryW {
		runes := []rune(summary)
		for lipgloss.Width(string(runes))+1 > summaryW {
			runes = runes[:len(runes)-1]
		}
		summary = string(runes) + "…"
	}
	dotsCount := totalW - nameW - 2 - lipgloss.Width(summary)
	if dotsCount < minDots {
		dotsCount = minDots
	}
	dots := theme.ItemDim.Render(strings.Repeat(".", dotsCount))
	descStyled := theme.ItemDim.Render(summary)
	return name + " " + dots + " " + descStyled
}

func panel(title, subtitle, body string, w, h int, active bool) string {
	titleStyle := theme.Title
	borderColor := theme.Border
	if active {
		titleStyle = theme.TitleActive
		borderColor = theme.BorderHi
	}
	titleLine := titleStyle.Render(" " + title + " ")
	if subtitle != "" {
		titleLine += theme.ItemDim.Render(subtitle)
	}
	target := h - 3
	if target < 1 {
		target = 1
	}
	bodyLines := strings.Split(body, "\n")
	for len(bodyLines) < target {
		bodyLines = append(bodyLines, "")
	}
	if len(bodyLines) > target {
		bodyLines = bodyLines[:target]
	}
	contentInside := titleLine + "\n" + strings.Join(bodyLines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(borderColor).
		Width(w).
		Render(contentInside)
}

func renderList[T any](items []T, sel, w, h int, active bool, fmtFn func(T, int) string) string {
	if len(items) == 0 {
		return ""
	}
	if h < 1 {
		h = 1
	}
	// scroll window
	off := 0
	if sel >= h {
		off = sel - h + 1
	}
	end := off + h
	if end > len(items) {
		end = len(items)
	}
	var lines []string
	for i := off; i < end; i++ {
		line := fmtFn(items[i], i)
		if i == sel && active {
			line = theme.ItemSelected.Render(rightpad(line, w))
		}
		lines = append(lines, line)
	}
	// fill remaining with blank lines to keep panel height stable
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func rightpad(s string, w int) string {
	width := lipgloss.Width(s)
	if width >= w {
		return s
	}
	return s + strings.Repeat(" ", w-width)
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// crude rune-safe truncate
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"K", "M", "G", "T"}
	v := float64(n) / 1024
	u := 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %sB", v, units[u])
}

func formatJob(s xfer.Snapshot, w int) string {
	pct := 0
	if s.Bytes > 0 {
		pct = int(100 * s.Written / s.Bytes)
	}
	color := theme.Warn
	switch s.State {
	case xfer.Done:
		color = theme.OK
	case xfer.Failed:
		color = theme.Err
	case xfer.Running:
		color = theme.Info
	}
	src := abbrPath(s.Src, 28)
	dst := abbrPath(s.Dst, 28)
	state := color.Render(fmt.Sprintf("%-7s", s.State))
	line := fmt.Sprintf("#%d %s %s → %s  %d%%  %s",
		s.ID, state, src, dst, pct, humanBytes(s.Written))
	if s.Err != "" {
		line += theme.Err.Render("  ! " + s.Err)
	}
	return truncate(line, w)
}

func abbrPath(p string, w int) string {
	p = strings.TrimPrefix(p, "file://")
	if len(p) <= w {
		return p
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + path.Base(p)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// overlay centers an overlay box on top of the main view.
func overlay(main, box string, w, h int) string {
	bw := lipgloss.Width(box)
	bh := lipgloss.Height(box)
	x := (w - bw) / 2
	y := (h - bh) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	// composite: split main into lines, replace lines starting at y with box lines starting at x
	mainLines := strings.Split(main, "\n")
	boxLines := strings.Split(box, "\n")
	for i, bl := range boxLines {
		row := y + i
		if row >= len(mainLines) {
			break
		}
		mainLine := mainLines[row]
		// strip ANSI? Too complex. Just overwrite by truncating the line and prepending up to x spaces.
		prefix := ""
		if mw := lipgloss.Width(mainLine); mw >= x {
			prefix = truncatePadRunes(mainLine, x)
		} else {
			prefix = mainLine + strings.Repeat(" ", x-mw)
		}
		mainLines[row] = prefix + bl
	}
	return strings.Join(mainLines, "\n")
}

func truncatePadRunes(s string, n int) string {
	// ANSI-aware truncation is hard — instead pad with spaces from scratch.
	return strings.Repeat(" ", n)
}
