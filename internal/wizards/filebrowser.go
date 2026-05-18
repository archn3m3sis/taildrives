package wizards

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/sahilm/fuzzy"

	"github.com/archn3m3sis/taildrives/internal/local"
	"github.com/archn3m3sis/taildrives/internal/theme"
)

// fbEntry is one row in the file browser. Mode is in `ls -lA` octal-letter
// form ("drwxr-xr-x" / "-rw-r--r--") on Unix; on Windows it carries the
// Get-ChildItem attribute string ("Directory" / "ReadOnly" / "Archive").
// ModTime is pre-formatted server-side so we don't need to ferry timezone
// info across the SSH hop.
type fbEntry struct {
	Name       string
	Size       int64
	IsDir      bool
	IsSymlink  bool   // true for symlinks; IsDir holds the target's type
	LinkTarget string // best-effort resolved link target (local fs only)
	Mode       string
	Owner      string
	ModTime    string
}

// fileBrowser is an mc/yazi-style two-pane directory navigator. Works
// against the LOCAL filesystem when device == "" or the running host,
// otherwise shells out to `ssh <user>@<device> ls -lA <path>` and parses
// the result.
type fileBrowser struct {
	device  string // "" or local hostname = local fs; otherwise remote
	cwd     string
	entries []fbEntry // raw listing from listDir
	idx     int
	scroll  int
	err     error
	loading bool

	// showHidden controls whether dot-prefixed entries appear in the
	// rendered listing. Default false — directory roots are otherwise
	// awash in OS metadata (.DS_Store, .file, .VolumeIcon.icns, .Trashes,
	// .Spotlight-V100, ...) that are pure noise for share-picking. The
	// `h` key toggles. visible() is the filtered view.
	showHidden bool

	// Fuzzy search state — `s` opens; Esc closes. Index is a list of
	// paths walked from the device's "home root" with a depth cap; results
	// is the substring-filtered subset shown as the user types.
	searchActive    bool
	searchInput     textinput.Model
	searchIndex     []indexedPath
	searchIndexErr  error
	searchScanning  bool
	searchResults   []indexedPath
	searchIdx       int
	searchScroll    int

	// Returned when the user hits `s` — the full absolute path selected.
	Selected string

	// done flips when the user picks or cancels.
	done     bool
	canceled bool
}

// indexedPath is one entry in the fuzzy search index — the absolute path
// plus a precomputed lowercase substring for cheap match comparison and
// the basename for display.
type indexedPath struct {
	Path     string
	Lower    string
	Base     string
	IsDir    bool
}

// ── Fuzzy search ────────────────────────────────────────────────────────

type searchIndexedMsg struct {
	paths []indexedPath
	err   error
}

// searchRoot returns the path the indexer should walk from for this
// device. Operator-owned content lives under $HOME on workstations; on
// servers there's no single sensible default so we walk from / with a
// stricter depth limit. Remote searches are scoped to the same shape.
func (f *fileBrowser) searchRoot() string {
	if strings.HasPrefix(f.device, "n3m-wks-") ||
		(f.device == "" || f.device == local.HostName()) &&
			strings.HasPrefix(local.HostName(), "n3m-wks-") {
		if home := os.Getenv("HOME"); home != "" {
			return home
		}
		return "/Users"
	}
	return "/"
}

// startSearchIndex walks the filesystem in a goroutine and returns a Cmd
// that emits the gathered index when done. Capped at ~8000 entries and
// 12 directory levels so the walk completes in a few seconds even on a
// crowded home directory. Hidden + sharing-discouraged paths are
// excluded — they're noise here exactly like they are in the main list.
func (f *fileBrowser) startSearchIndex() tea.Cmd {
	dev := f.device
	root := f.searchRoot()
	return func() tea.Msg {
		const maxEntries = 8000
		const maxDepth = 12
		var paths []indexedPath
		if dev == "" || dev == local.HostName() {
			err := walkLocal(root, maxDepth, maxEntries, func(p indexedPath) {
				paths = append(paths, p)
			})
			return searchIndexedMsg{paths: paths, err: err}
		}
		// Remote: shell `find` with depth + count caps. Walks the same way.
		paths, err := walkRemote(dev, root, maxDepth, maxEntries)
		return searchIndexedMsg{paths: paths, err: err}
	}
}

// walkLocal does a bounded BFS-ish filesystem walk; respects the same
// hidden / sharing-discouraged exclusions the main listing uses so the
// fuzzy results read consistently with what the operator sees while
// browsing.
func walkLocal(root string, maxDepth, maxEntries int, emit func(indexedPath)) error {
	type todo struct {
		path  string
		depth int
	}
	queue := []todo{{root, 0}}
	count := 0
	for len(queue) > 0 && count < maxEntries {
		cur := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(cur.path)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := cur.path + "/" + name
			if cur.path == "/" {
				full = "/" + name
			}
			isDir := e.IsDir()
			if !isDir {
				if info, err := e.Info(); err == nil &&
					info.Mode()&os.ModeSymlink != 0 {
					if t, err := os.Stat(full); err == nil {
						isDir = t.IsDir()
					}
				}
			}
			if isDir && sharingDiscouraged(cur.path, name, true) {
				continue
			}
			emit(indexedPath{
				Path:  full,
				Lower: strings.ToLower(full),
				Base:  name,
				IsDir: isDir,
			})
			count++
			if count >= maxEntries {
				return nil
			}
			if isDir && cur.depth+1 < maxDepth {
				queue = append(queue, todo{full, cur.depth + 1})
			}
		}
	}
	return nil
}

// walkRemote runs `find` on the remote host with depth + count caps,
// returning paths in the same indexedPath shape.
func walkRemote(device, root string, maxDepth, maxEntries int) ([]indexedPath, error) {
	user := "archn3m3sis"
	if strings.HasPrefix(device, "n3m-srv-04") {
		// Windows host — skip; find isn't standard there. Returning empty
		// rather than erroring so the search UI can show "no results".
		return nil, nil
	}
	// -mindepth 1 skips the root itself; -not -path "*/.*" prunes hidden
	// dirs; `-maxdepth N` caps recursion; head -N caps output volume.
	cmd := fmt.Sprintf(
		"find %q -mindepth 1 -maxdepth %d -not -path '*/.*' -printf '%%y %%p\\n' 2>/dev/null | head -n %d",
		root, maxDepth, maxEntries)
	c := exec.Command("ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		user+"@"+device, cmd)
	ctx := timeAfter(10 * time.Second)
	doneCh := make(chan struct {
		out []byte
		err error
	}, 1)
	go func() {
		out, err := c.CombinedOutput()
		doneCh <- struct {
			out []byte
			err error
		}{out, err}
	}()
	var out []byte
	select {
	case r := <-doneCh:
		if r.err != nil {
			return nil, fmt.Errorf("remote find: %s", strings.TrimSpace(string(r.out)))
		}
		out = r.out
	case <-ctx:
		_ = c.Process.Kill()
		return nil, fmt.Errorf("remote find timed out after 10s")
	}
	var paths []indexedPath
	for _, ln := range strings.Split(string(out), "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) < 3 {
			continue
		}
		// "d /some/path" or "f /some/file"
		typeChar := ln[0]
		full := strings.TrimSpace(ln[2:])
		paths = append(paths, indexedPath{
			Path:  full,
			Lower: strings.ToLower(full),
			Base:  pathBase(full),
			IsDir: typeChar == 'd',
		})
	}
	return paths, nil
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// applySearch runs the fuzzy-match library against the index using the
// current input value. Empty input shows everything (capped to a reasonable
// display count). Results are sorted by match score descending.
func (f *fileBrowser) applySearch() {
	q := strings.TrimSpace(f.searchInput.Value())
	if q == "" {
		// Show the first N raw entries so the operator has something to
		// scroll through even before typing.
		n := len(f.searchIndex)
		if n > 200 {
			n = 200
		}
		f.searchResults = f.searchIndex[:n]
		f.searchIdx = 0
		return
	}
	// Use fuzzy match on the Path field (sahilm/fuzzy preserves order
	// already by best match — perfect for this).
	sources := make([]string, len(f.searchIndex))
	for i, e := range f.searchIndex {
		sources[i] = e.Path
	}
	matches := fuzzy.Find(q, sources)
	out := make([]indexedPath, 0, len(matches))
	limit := 200
	for i, m := range matches {
		if i >= limit {
			break
		}
		out = append(out, f.searchIndex[m.Index])
	}
	f.searchResults = out
	f.searchIdx = 0
}

// visible returns the entries slice filtered for the showHidden setting —
// the single source of truth for what the user sees, what idx indexes
// into, and what gets joined into a path on Enter.
func (f *fileBrowser) visible() []fbEntry {
	if f.showHidden {
		return f.entries
	}
	out := make([]fbEntry, 0, len(f.entries))
	for _, e := range f.entries {
		if strings.HasPrefix(e.Name, ".") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func newFileBrowser(device, startPath string) *fileBrowser {
	if startPath == "" {
		startPath = "/"
		if device != "" && (strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05")) {
			startPath = "D:\\"
		}
	}
	return &fileBrowser{device: device, cwd: startPath, loading: true}
}

// initial Cmd to load the starting directory
func (f *fileBrowser) start() tea.Cmd { return f.loadCmd() }

// loadCmd asynchronously lists the current directory.
type fbLoadedMsg struct {
	cwd     string
	entries []fbEntry
	err     error
}

func (f *fileBrowser) loadCmd() tea.Cmd {
	dev := f.device
	cwd := f.cwd
	return func() tea.Msg {
		entries, err := listDir(dev, cwd)
		return fbLoadedMsg{cwd: cwd, entries: entries, err: err}
	}
}

// listDir returns directory contents for `path` either locally (if dev is
// empty or matches local hostname) or via SSH.
func listDir(device, p string) ([]fbEntry, error) {
	if device == "" || device == local.HostName() {
		return listLocal(p)
	}
	return listRemote(device, p)
}

func listLocal(p string) ([]fbEntry, error) {
	es, err := local.LocalEntries(p)
	if err != nil {
		return nil, err
	}
	out := make([]fbEntry, 0, len(es))
	for _, e := range es {
		out = append(out, fbEntry{
			Name:       e.Name,
			Size:       e.Size,
			IsDir:      e.IsDir,
			IsSymlink:  e.IsSymlink,
			LinkTarget: e.LinkTarget,
			Mode:       e.Mode,
			ModTime:    e.ModTime,
		})
	}
	sortEntries(out)
	return out, nil
}

// listRemote shells out via SSH and parses `ls -lA --color=never`. Uses
// Administrator user on Windows server-2025 hosts where Tailscale runs
// in server mode.
func listRemote(device, p string) ([]fbEntry, error) {
	user := "archn3m3sis"
	// -L follows symlinks so /etc, /tmp, /var (macOS) appear as the
	// directories they point at, not as filtered-out l-mode entries.
	cmdStr := fmt.Sprintf("ls -lLA --color=never %q", p)
	if strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05") {
		user = "Administrator"
		// Windows: pipe-delimited "isDir|name|size|modtime|mode" per row.
		// Mode is the legacy attribute letter string (d/a/r/h/s/l) — close
		// enough to feel like a chmod string in the right-pane detail view.
		cmdStr = fmt.Sprintf(
			"Get-ChildItem -Force -LiteralPath %q | ForEach-Object { "+
				"\"$($_.PSIsContainer)|$($_.Name)|$($_.Length)|"+
				"$($_.LastWriteTime.ToString('yyyy-MM-dd HH:mm'))|$($_.Mode)\" }",
			p)
	}
	// accept-new mirrors the operator's ~/.ssh/config policy: first-time
	// hosts get auto-trusted, but a CHANGED key still aborts (MITM-safe).
	// Without this, every fresh sidecar/workstation triggers "host key
	// verification failed" the first time the TUI tries to list its fs.
	cmd := exec.Command("ssh",
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5",
		"-o", "StrictHostKeyChecking=accept-new",
		user+"@"+device, cmdStr)
	cmd.Env = append(os.Environ(), "SSH_AUTH_SOCK="+os.Getenv("SSH_AUTH_SOCK"))
	ctx := timeAfter(8 * time.Second)
	doneCh := make(chan struct {
		out []byte
		err error
	}, 1)
	go func() {
		out, err := cmd.CombinedOutput()
		doneCh <- struct {
			out []byte
			err error
		}{out, err}
	}()
	select {
	case r := <-doneCh:
		if r.err != nil {
			return nil, fmt.Errorf("ssh %s: %s", device, strings.TrimSpace(string(r.out)))
		}
		if strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05") {
			return parseWindowsLS(string(r.out)), nil
		}
		return parseUnixLS(string(r.out)), nil
	case <-ctx:
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("ssh %s timed out after 8s", device)
	}
}

func timeAfter(d time.Duration) <-chan time.Time { return time.After(d) }

// parseUnixLS handles `ls -lA --color=never` output. Layout:
//
//	drwxr-xr-x  3 archn3m3sis users 4096 May 10 14:23 documents
//	[0]         [1] [2]         [3]   [4]  [5] [6] [7]    [8+]
//	mode        n   owner       grp   size           date         name(with-spaces)
//
// We carry mode + owner + a compact "May 10 14:23"-style modtime through
// so the right-pane detail view can show them without an extra round-trip.
func parseUnixLS(s string) []fbEntry {
	var out []fbEntry
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "total ") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 9 {
			continue
		}
		mode := fields[0]
		owner := fields[2]
		var size int64
		fmt.Sscanf(fields[4], "%d", &size)
		modtime := fields[5] + " " + fields[6] + " " + fields[7]
		name := strings.Join(fields[8:], " ")
		isDir := strings.HasPrefix(mode, "d")
		out = append(out, fbEntry{
			Name:    name,
			Size:    size,
			IsDir:   isDir,
			Mode:    mode,
			Owner:   owner,
			ModTime: modtime,
		})
	}
	sortEntries(out)
	return out
}

// parseWindowsLS parses our 5-field pipe-delimited PowerShell output:
// "isDir|name|size|modtime|mode". Older Windows hosts may still emit the
// 3-field shape, so we tolerate either.
func parseWindowsLS(s string) []fbEntry {
	var out []fbEntry
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimSuffix(ln, "\r")
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, "|", 5)
		if len(parts) < 2 {
			continue
		}
		isDir := strings.EqualFold(parts[0], "True")
		name := parts[1]
		var size int64
		if len(parts) >= 3 {
			fmt.Sscanf(parts[2], "%d", &size)
		}
		var modtime, mode string
		if len(parts) >= 4 {
			modtime = parts[3]
		}
		if len(parts) >= 5 {
			mode = parts[4]
		}
		out = append(out, fbEntry{
			Name:    name,
			Size:    size,
			IsDir:   isDir,
			Mode:    mode,
			ModTime: modtime,
			Owner:   "(windows)",
		})
	}
	sortEntries(out)
	return out
}

// iconFor returns a SINGLE-CELL nerd-font glyph for an entry. JetBrainsMono
// Nerd Font (and similar patched fonts) render these in one terminal cell
// each, which is what makes the row-layout math reliable. Using emoji here
// is a trap: ️ variation selectors and ZWJ sequences make Width()
// inconsistent across terminals → wrapped rows → visual gaps between items.
// All glyph codepoints below come from https://www.nerdfonts.com/cheat-sheet.
func iconFor(e fbEntry) string {
	if e.IsDir {
		return "" // nf-fa-folder
	}
	name := strings.ToLower(e.Name)
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i:]
	}
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif",
		".bmp", ".tiff", ".tif", ".svg", ".ico":
		return "" // nf-fa-file_image_o
	case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".flv", ".m4v", ".wmv":
		return "" // nf-fa-film
	case ".mp3", ".flac", ".wav", ".m4a", ".ogg", ".oga", ".opus", ".aac", ".aiff":
		return "" // nf-fa-music
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".zst":
		return "" // nf-fa-file_archive_o
	case ".pdf":
		return "" // nf-fa-file_pdf_o
	case ".doc", ".docx", ".rtf", ".odt", ".pages":
		return "" // nf-fa-file_word_o
	case ".md", ".markdown", ".txt", ".rst", ".log":
		return "" // nf-fa-file_text_o
	case ".go", ".py", ".rs", ".c", ".cpp", ".h", ".hpp", ".java", ".kt",
		".rb", ".swift", ".m", ".mm", ".sh", ".bash", ".zsh", ".nix",
		".js", ".ts", ".jsx", ".tsx", ".html", ".css", ".scss", ".lua",
		".php", ".pl", ".sql":
		return "" // nf-fa-code
	case ".yaml", ".yml", ".toml", ".ini", ".conf", ".cfg", ".json",
		".env", ".plist":
		return "" // nf-fa-cog
	case ".key", ".pem", ".crt", ".cer", ".p12", ".pfx", ".gpg", ".asc":
		return "" // nf-fa-key
	case ".db", ".sqlite", ".sqlite3", ".dump":
		return "" // nf-fa-database
	case ".dmg", ".pkg", ".app", ".deb", ".rpm", ".msi", ".exe":
		return "" // nf-fa-cube
	case ".iso", ".img", ".qcow2", ".vmdk", ".vdi":
		return "" // nf-fa-laptop (close-enough disk-image stand-in)
	}
	return "" // nf-fa-file_o (default)
}

// tagInfo returns a (label, background-color) pair for the styled badge
// rendered on the right side of each row. Labels are short uppercase
// codes: per-extension where the extension is itself distinctive ("GO",
// "PY", "MP3"), generic category where it isn't ("FILE"). Colors are
// picked to match each language/format's de-facto brand color so the eye
// can scan a directory listing by color before reading.
func tagInfo(e fbEntry) (label string, bg lipgloss.Color) {
	if e.IsDir {
		return "DIR", lipgloss.Color("#3b82f6") // blue-500
	}
	name := strings.ToLower(e.Name)
	ext := ""
	if i := strings.LastIndex(name, "."); i >= 0 {
		ext = name[i+1:]
	}
	switch "." + ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif",
		".bmp", ".tiff", ".tif", ".svg", ".ico":
		return strings.ToUpper(ext), lipgloss.Color("#10b981") // emerald-500
	case ".mp4", ".mkv", ".mov", ".avi", ".webm", ".flv", ".m4v", ".wmv":
		return strings.ToUpper(ext), lipgloss.Color("#ef4444") // red-500
	case ".mp3", ".flac", ".wav", ".m4a", ".ogg", ".oga", ".opus", ".aac", ".aiff":
		return strings.ToUpper(ext), lipgloss.Color("#a855f7") // purple-500
	case ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".zst":
		return strings.ToUpper(ext), lipgloss.Color("#f59e0b") // amber-500
	case ".pdf":
		return "PDF", lipgloss.Color("#dc2626") // red-600
	case ".doc", ".docx", ".rtf", ".odt", ".pages":
		return strings.ToUpper(ext), lipgloss.Color("#1d4ed8") // blue-700
	case ".md", ".markdown":
		return "MD", lipgloss.Color("#0891b2") // cyan-600
	case ".txt", ".rst", ".log":
		return strings.ToUpper(ext), lipgloss.Color("#737373") // neutral-500
	case ".go":
		return "GO", lipgloss.Color("#00ADD8") // go brand
	case ".py":
		return "PY", lipgloss.Color("#3776AB") // python brand
	case ".rs":
		return "RS", lipgloss.Color("#DEA584") // rust
	case ".js", ".mjs", ".cjs":
		return "JS", lipgloss.Color("#a78a00") // dimmed JS yellow (white text reads)
	case ".ts":
		return "TS", lipgloss.Color("#3178C6") // typescript
	case ".jsx":
		return "JSX", lipgloss.Color("#61DAFB")
	case ".tsx":
		return "TSX", lipgloss.Color("#3178C6")
	case ".html":
		return "HTML", lipgloss.Color("#E34F26")
	case ".css":
		return "CSS", lipgloss.Color("#1572B6")
	case ".scss", ".sass":
		return "SCSS", lipgloss.Color("#C76494")
	case ".sh", ".bash", ".zsh":
		return strings.ToUpper(ext), lipgloss.Color("#4eaa25")
	case ".nix":
		return "NIX", lipgloss.Color("#5277C3")
	case ".c", ".h":
		return strings.ToUpper(ext), lipgloss.Color("#5b6770")
	case ".cpp", ".hpp", ".cc":
		return strings.ToUpper(ext), lipgloss.Color("#004482")
	case ".java":
		return "JAVA", lipgloss.Color("#ED8B00")
	case ".kt":
		return "KT", lipgloss.Color("#7F52FF")
	case ".rb":
		return "RB", lipgloss.Color("#CC342D")
	case ".swift":
		return "SWFT", lipgloss.Color("#FA7343")
	case ".lua":
		return "LUA", lipgloss.Color("#1f1faa")
	case ".sql":
		return "SQL", lipgloss.Color("#0f766e")
	case ".yaml", ".yml":
		return strings.ToUpper(ext), lipgloss.Color("#cb171e")
	case ".toml":
		return "TOML", lipgloss.Color("#9c4221")
	case ".json":
		return "JSON", lipgloss.Color("#525252")
	case ".ini", ".conf", ".cfg":
		return strings.ToUpper(ext), lipgloss.Color("#525252")
	case ".env":
		return "ENV", lipgloss.Color("#525252")
	case ".key", ".pem", ".crt", ".cer", ".p12", ".pfx":
		return strings.ToUpper(ext), lipgloss.Color("#ca8a04") // yellow-600
	case ".gpg", ".asc":
		return strings.ToUpper(ext), lipgloss.Color("#ca8a04")
	case ".db", ".sqlite", ".sqlite3", ".dump":
		return "DB", lipgloss.Color("#0f766e") // teal-700
	case ".dmg", ".pkg", ".app", ".deb", ".rpm", ".msi":
		return strings.ToUpper(ext), lipgloss.Color("#7c3aed") // violet-600
	case ".exe":
		return "EXE", lipgloss.Color("#7c3aed")
	case ".iso", ".img", ".qcow2", ".vmdk", ".vdi":
		return strings.ToUpper(ext), lipgloss.Color("#475569") // slate-600
	}
	if ext != "" && len(ext) <= 4 {
		// Unknown but valid-looking extension — show it raw with neutral bg.
		return strings.ToUpper(ext), lipgloss.Color("#404040")
	}
	return "FILE", lipgloss.Color("#404040")
}

// renderTag produces the lipgloss-styled badge string. Padded ` LABEL `
// with category background + white bold text. Width = len(label) + 2.
func renderTag(e fbEntry) string {
	label, bg := tagInfo(e)
	return lipgloss.NewStyle().
		Background(bg).
		Foreground(lipgloss.Color("#ffffff")).
		Bold(true).
		Padding(0, 1).
		Render(label)
}

// rawTagWidth returns the printable width of the tag string a render would
// produce, without actually rendering it. Used by the row layout math.
func rawTagWidth(e fbEntry) int {
	label, _ := tagInfo(e)
	return len(label) + 2 // 1-cell padding each side
}

// maxTagWidth is the widest tag we ever render, in cells. Tags up to 4
// letters ("YAML", "JSON", "HTML", "JAVA", "SCSS", "TSX") + 2 padding.
const maxTagWidth = 6

// sharingDiscouraged returns true for folders the operator probably should
// NOT share via Taildrive: system paths, hidden config dirs, OS recovery
// folders. The row is dimmed visually but still selectable — this is a
// guardrail, not a block. Operators who know what they're doing aren't
// stopped from sharing e.g. ~/.config if they have a reason to.
func sharingDiscouraged(cwd, name string, isDir bool) bool {
	if !isDir {
		return false
	}
	// Hidden dirs across all OSes.
	if strings.HasPrefix(name, ".") {
		return true
	}
	// Top-level system paths at root.
	if cwd == "/" {
		switch name {
		case "System", "Library", "private", "usr", "bin", "sbin",
			"var", "opt", "tmp", "etc", "dev", "cores",
			"proc", "sys", "boot", "lib", "lib64", "run", "root",
			"Network", "Volumes":
			return true
		}
	}
	// Windows system roots.
	if cwd == "C:\\" || cwd == "D:\\" {
		switch name {
		case "Windows", "Program Files", "Program Files (x86)",
			"ProgramData", "$Recycle.Bin", "System Volume Information",
			"PerfLogs", "Recovery", "Config.Msi":
			return true
		}
	}
	// macOS per-user Library — system config, not a share target.
	if strings.HasPrefix(cwd, "/Users/") && strings.Count(cwd, "/") == 2 && name == "Library" {
		return true
	}
	// Linux per-user system-y paths.
	if strings.HasPrefix(cwd, "/home/") && strings.Count(cwd, "/") == 2 {
		switch name {
		case "snap": // canonical snap mountpoint inside $HOME
			return true
		}
	}
	return false
}

func sortEntries(es []fbEntry) {
	sort.Slice(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir // dirs first
		}
		return strings.ToLower(es[i].Name) < strings.ToLower(es[j].Name)
	})
}

// updateBrowser handles keys when the file browser owns input. Returns
// the (possibly modified) browser and a Cmd. The bool return is true
// when the user pressed `S` (select) or `Esc` (cancel) — the parent
// wizard checks .done + .canceled + .Selected to advance.
//
// When `searchActive` is true, key events route to the fuzzy-search
// overlay instead of normal navigation.
func (f *fileBrowser) update(msg tea.Msg) (*fileBrowser, tea.Cmd) {
	switch m := msg.(type) {
	case fbLoadedMsg:
		if m.cwd == f.cwd {
			f.entries = m.entries
			f.err = m.err
			f.idx = 0
			f.scroll = 0
			f.loading = false
		}
		return f, nil
	case searchIndexedMsg:
		f.searchScanning = false
		f.searchIndex = m.paths
		f.searchIndexErr = m.err
		f.applySearch()
		return f, nil
	case tea.KeyMsg:
		// Search mode owns its own key handling.
		if f.searchActive {
			return f.updateSearch(m)
		}
		// Hard gate: while a directory load is in flight the entries list is
		// stale — DON'T process navigation. Without this, OS-level key
		// repeat (macOS in particular) can fire 2-3 Enter or Down events
		// in the time between Enter-triggers-dir-change and the load Cmd
		// returns. The second Enter would index into the *previous* dir's
		// entries with the *new* cwd → bogus paths like "/System/System".
		// Same mechanism makes Down appear to skip rows.
		if f.loading {
			return f, nil
		}
		// The listing has a synthetic row 0 — "[✓ SELECT THIS DIRECTORY]" —
		// so users can pick the current cwd with Enter, not a hidden `s`.
		// Real fs entries start at index 1, hence the -1 offsets below.
		// vis is the filtered view (hidden files filtered out unless
		// showHidden); navigation indexes into THAT, not the raw slice.
		vis := f.visible()
		realCount := len(vis)
		switch {
		case key.Matches(m, key.NewBinding(key.WithKeys("esc"))):
			f.canceled = true
			f.done = true
			return f, nil
		case key.Matches(m, key.NewBinding(key.WithKeys("S"))):
			f.Selected = f.cwd
			f.done = true
			return f, nil
		case key.Matches(m, key.NewBinding(key.WithKeys("s"))):
			// Open fuzzy search overlay. If we don't have an index yet,
			// kick off a background scan; the input is immediately usable
			// and applySearch fires on every keystroke regardless.
			ti := textinput.New()
			ti.Prompt = "/ "
			ti.Placeholder = "fuzzy-search files & folders…"
			ti.CharLimit = 128
			ti.Width = 60
			ti.Focus()
			f.searchInput = ti
			f.searchActive = true
			f.searchIdx = 0
			f.searchScroll = 0
			var cmd tea.Cmd
			if len(f.searchIndex) == 0 && !f.searchScanning {
				f.searchScanning = true
				cmd = f.startSearchIndex()
			} else {
				f.applySearch()
			}
			return f, tea.Batch(cmd, textinput.Blink)
		case key.Matches(m, key.NewBinding(key.WithKeys("."))):
			// Toggle hidden-file visibility. Reset idx to the synthetic
			// SELECT row so we never land out-of-bounds on the resized
			// list.
			f.showHidden = !f.showHidden
			f.idx = 0
			f.scroll = 0
			return f, nil
		case key.Matches(m, key.NewBinding(key.WithKeys("enter", "right", "l"))):
			if f.idx == 0 {
				f.Selected = f.cwd
				f.done = true
				return f, nil
			}
			ei := f.idx - 1
			if ei < realCount && vis[ei].IsDir {
				next := joinPath(f.device, f.cwd, vis[ei].Name)
				f.cwd = next
				f.loading = true
				return f, f.loadCmd()
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("backspace", "left", "h"))):
			parent := parentPath(f.device, f.cwd)
			if parent != f.cwd {
				f.cwd = parent
				f.loading = true
				return f, f.loadCmd()
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("up", "k"))):
			if f.idx > 0 {
				f.idx--
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("down", "j"))):
			if f.idx < realCount { // realCount == max idx (synthetic + entries.length)
				f.idx++
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("home", "g"))):
			f.idx = 0
		case key.Matches(m, key.NewBinding(key.WithKeys("end", "G"))):
			f.idx = realCount
		case key.Matches(m, key.NewBinding(key.WithKeys("pgup", "ctrl+u"))):
			f.idx -= 10
			if f.idx < 0 {
				f.idx = 0
			}
		case key.Matches(m, key.NewBinding(key.WithKeys("pgdown", "ctrl+d"))):
			f.idx += 10
			if f.idx > realCount {
				f.idx = realCount
			}
		}
	}
	return f, nil
}

func joinPath(device, cwd, name string) string {
	if strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05") {
		// Windows
		if strings.HasSuffix(cwd, "\\") {
			return cwd + name
		}
		return cwd + "\\" + name
	}
	return path.Join(cwd, name)
}

func parentPath(device, cwd string) string {
	if strings.HasPrefix(device, "n3m-srv-04") || strings.HasPrefix(device, "n3m-srv-05") {
		// Windows: D:\foo\bar → D:\foo; D:\foo → D:\; D:\ → D:\
		trimmed := strings.TrimRight(cwd, "\\")
		i := strings.LastIndex(trimmed, "\\")
		if i < 0 {
			return cwd
		}
		if i == 2 { // "D:\"
			return cwd[:i+1]
		}
		return trimmed[:i]
	}
	if cwd == "/" {
		return "/"
	}
	return path.Dir(cwd)
}

// view returns the file browser as a two-pane string fitting into (w × h).
// Left pane: directory listing (with a synthetic top row "[✓ SELECT THIS
// DIRECTORY]" so users can pick the cwd with Enter). Right pane: details
// for the highlighted entry. A footer shows path + key hints.
func (f *fileBrowser) view(w, h int) string {
	if f.searchActive {
		return f.renderSearchView(w, h)
	}
	leftW := w * 5 / 10
	if leftW < 32 {
		leftW = 32
	}
	rightW := w - leftW - 1
	if rightW < 20 {
		rightW = 20
	}
	bodyH := h - 4 // header + footer + 2 spacer

	// Header layout: breadcrumb path on the LEFT, device badge pushed
	// to the RIGHT edge. The badge has its own colored backdrop (matches
	// the type-tag visual language) so the active device reads at a
	// glance, separate from the path. Total width = pane composition
	// width (leftW + gap + sep + gap + rightW = leftW + rightW + 3).
	deviceBadge := f.renderDeviceBadge()
	pathStyled := lipgloss.NewStyle().Foreground(theme.Text).Render(f.cwd)
	totalHeaderW := leftW + rightW + 3
	pathW := lipgloss.Width(pathStyled)
	badgeW := lipgloss.Width(deviceBadge)
	// Truncate the path if it would collide with the badge — keep at
	// least 2 cells of gutter between them.
	maxPathW := totalHeaderW - badgeW - 2
	if maxPathW < 8 {
		maxPathW = 8
	}
	if pathW > maxPathW {
		runes := []rune(f.cwd)
		truncated := f.cwd
		for lipgloss.Width(truncated)+1 > maxPathW && len(runes) > 0 {
			runes = runes[:len(runes)-1]
			truncated = string(runes)
		}
		pathStyled = lipgloss.NewStyle().Foreground(theme.Text).Render("…" + truncated)
		pathW = lipgloss.Width(pathStyled)
	}
	gap := totalHeaderW - pathW - badgeW
	if gap < 2 {
		gap = 2
	}
	header := pathStyled + strings.Repeat(" ", gap) + deviceBadge

	// Build the synthetic "SELECT THIS DIRECTORY" row + real entries view.
	// idx 0 = synthetic; idx 1..N = real entries[0..N-1].
	// vis is the filtered listing — what the user actually sees and what
	// idx indexes into. f.entries is the unfiltered source.
	vis := f.visible()
	maxIdx := len(vis) // synthetic + entries.length - 1
	if f.idx > maxIdx {
		f.idx = maxIdx
	}
	if f.idx-f.scroll >= bodyH {
		f.scroll = f.idx - bodyH + 1
	}
	if f.idx < f.scroll {
		f.scroll = f.idx
	}

	selectLabel := "✓ SELECT THIS DIRECTORY"
	selectRow := func(isSel bool) string {
		raw := "  " + selectLabel
		if isSel {
			// Full pane-width fill — same fix as the entry-row highlight
			// so the right edge doesn't leak the terminal background.
			return lipgloss.NewStyle().
				Background(theme.AccentHi).Foreground(lipgloss.Color("#000000")).
				Bold(true).
				Render(rightpad("  "+selectLabel, leftW))
		}
		return lipgloss.NewStyle().Bold(true).Foreground(theme.Green).Render(raw)
	}

	var leftLines []string
	switch {
	case f.loading:
		leftLines = []string{"  loading…"}
	case f.err != nil:
		leftLines = []string{theme.Err.Render("  ✗ " + f.err.Error())}
	default:
		// row 0 is the synthetic SELECT-this-dir
		if f.scroll == 0 {
			leftLines = append(leftLines, selectRow(f.idx == 0))
		}
		// real entries follow
		start := f.scroll
		if start == 0 {
			start = 1 // already rendered synthetic
		}
		end := f.scroll + bodyH
		if end > maxIdx+1 {
			end = maxIdx + 1
		}
		// Row composition: "  ICON  name…………  [ TAG ]"
		// Widths:           2 + 1 + 1 + N    + 1 + T = 5 + N + T
		// Content area inside the bordered left pane is (leftW - 4)
		// (matching SELECT row's rightpad). Solve N + T ≤ leftW - 9.
		// Tag column reserves maxTagWidth (6) regardless of actual tag,
		// so different rows stay column-aligned.
		nameColW := leftW - 9 - maxTagWidth
		if nameColW < 10 {
			nameColW = 10
		}
		for i := start; i < end; i++ {
			ei := i - 1
			e := vis[ei]
			isSel := i == f.idx
			icon := iconFor(e)
			discouraged := sharingDiscouraged(f.cwd, e.Name, e.IsDir)

			nm := e.Name
			if e.IsDir {
				nm += "/"
			}
			// Reserve 2 cells for the symlink marker (" ↗") so the marker
			// stays visible even when the name has to be truncated. The
			// marker is rendered separately (in cyan) and concatenated
			// below — keeping it OUT of nm here ensures truncation only
			// shortens the name itself.
			markerW := 0
			if e.IsSymlink {
				markerW = 2
			}
			nameBudget := nameColW - markerW
			if nameBudget < 4 {
				nameBudget = 4
			}
			if lipgloss.Width(nm) > nameBudget {
				runes := []rune(nm)
				for lipgloss.Width(string(runes))+1 > nameBudget {
					runes = runes[:len(runes)-1]
				}
				nm = string(runes) + "…"
			}

			// Tag: rendered with category color, but DIMMED when this row
			// is in the "discouraged sharing" set so the operator sees the
			// guardrail without it shouting.
			var tagStr string
			if discouraged {
				tagLabel, _ := tagInfo(e)
				tagStr = lipgloss.NewStyle().
					Foreground(lipgloss.Color("#525252")).
					Background(lipgloss.Color("#1a1a1a")).
					Padding(0, 1).Render(tagLabel)
			} else {
				tagStr = renderTag(e)
			}
			tagW := lipgloss.Width(tagStr)
			tagLeftPad := max0(maxTagWidth - tagW)

			// Build the row. Selected row uses a single background sweep so
			// the highlight covers the FULL pane width (leftW, not
			// leftW-4) — otherwise the rightmost 4 cells fall through to
			// the terminal background, producing the "dark blue bleeding
			// through on the right side" the operator flagged.
			if isSel {
				nmWithMarker := nm
				if e.IsSymlink {
					nmWithMarker += " ↗"
				}
				namePadded := nmWithMarker + strings.Repeat(" ",
					max0(nameColW-lipgloss.Width(nmWithMarker)))
				raw := "  " + icon + " " + namePadded + " " +
					strings.Repeat(" ", tagLeftPad) + tagStr
				leftLines = append(leftLines,
					lipgloss.NewStyle().
						Background(theme.AccentHi).Foreground(lipgloss.Color("#000000")).
						Render(rightpad(raw, leftW)))
				continue
			}

			// Non-selected: pick name style by dir / file / discouraged.
			// Zebra striping uses a very subtle row background on every
			// other entry — enough contrast to scan a long listing, not
			// so much that it competes with the data.
			zebra := (i % 2) == 0
			var rowBg lipgloss.Color
			if zebra {
				rowBg = lipgloss.Color("#161616")
			} else {
				rowBg = lipgloss.Color("#1c1c1c")
			}
			nameFg := theme.Text
			switch {
			case discouraged:
				nameFg = lipgloss.Color("#525252")
			case e.IsDir:
				nameFg = lipgloss.Color("#7dd3fc") // sky-300, premium dir blue
			}
			styledName := lipgloss.NewStyle().
				Background(rowBg).Foreground(nameFg).
				Render(nm)
			// Append symlink marker " ↗" in cyan, ON the row background so
			// the zebra stays seamless. Done as a separate span because
			// lipgloss can only apply one foreground per Render call.
			nameTotalW := lipgloss.Width(nm)
			if e.IsSymlink {
				styledName += lipgloss.NewStyle().
					Background(rowBg).
					Foreground(lipgloss.Color("#22d3ee")).
					Bold(true).
					Render(" ↗")
				nameTotalW += 2 // " ↗" = 2 cells
			}
			styledNamePadded := styledName + lipgloss.NewStyle().
				Background(rowBg).
				Render(strings.Repeat(" ", max0(nameColW-nameTotalW)))

			iconFg := theme.Text
			if discouraged {
				iconFg = lipgloss.Color("#525252")
			}
			iconStr := lipgloss.NewStyle().
				Background(rowBg).Foreground(iconFg).
				Render(icon)

			// Gutter (leading "  ") and the space between name/tag must
			// carry the same row background or the zebra will look broken
			// across the row.
			bgStyle := lipgloss.NewStyle().Background(rowBg)
			gutter := bgStyle.Render("  ")
			sep := bgStyle.Render(" ")
			pad := bgStyle.Render(strings.Repeat(" ", tagLeftPad))

			// Pad the row out to the full pane width so the row's bg sweep
			// reaches the right edge of the pane (otherwise zebra has a
			// 4-cell gap on the right). Compute the actual rendered width
			// and tail-pad with bg-colored spaces.
			rowStr := gutter + iconStr + sep + styledNamePadded + sep + pad + tagStr
			if w := lipgloss.Width(rowStr); w < leftW {
				rowStr += bgStyle.Render(strings.Repeat(" ", leftW-w))
			}
			leftLines = append(leftLines, rowStr)
		}
		// Zebra-filled trailing rows: keep the alternating pattern going
		// all the way to the bottom of the pane even when the directory
		// has fewer items than the visible height. Use the SAME index
		// convention so an empty dir reads as a clean continuation of the
		// pattern, not a sudden plain-black void.
		for len(leftLines) < bodyH {
			rowIdx := len(leftLines)
			var bg lipgloss.Color
			if rowIdx%2 == 0 {
				bg = lipgloss.Color("#161616")
			} else {
				bg = lipgloss.Color("#1c1c1c")
			}
			leftLines = append(leftLines,
				lipgloss.NewStyle().Background(bg).
					Render(strings.Repeat(" ", leftW)))
		}
	}

	// Right pane: details for whatever's highlighted
	var rightLines []string
	if f.idx == 0 {
		rightLines = []string{
			theme.Title.Render("  Action: ") + lipgloss.NewStyle().Foreground(theme.Green).Bold(true).Render("Pick this directory"),
			"",
			theme.ItemDim.Render("  Will share:"),
			"  " + theme.AccentHiS.Render(f.cwd),
		}
	} else if ei := f.idx - 1; ei >= 0 && ei < len(vis) {
		e := vis[ei]
		typeStr := stringIfDir(e.IsDir, theme.Dir.Render("directory"), theme.File.Render("file"))
		// Dot-leader rows ("Field ··············· value") replace the old
		// "Field:    value" alignment. Reads like a table-of-contents, eats
		// less brain than padded columns, and looks substantially more
		// premium. detailRowW is the right pane's usable inner width.
		detailRowW := rightW - 4
		rightLines = []string{
			theme.Title.Render("  ") + iconFor(e) + "  " + theme.AccentHiS.Render(e.Name),
			"",
			"  " + dotLeader("Type", typeStr, detailRowW),
		}
		if !e.IsDir {
			rightLines = append(rightLines,
				"  "+dotLeader("Size", theme.Item.Render(humanSize(e.Size)), detailRowW))
		}
		if e.Mode != "" {
			rightLines = append(rightLines,
				"  "+dotLeader("Permissions", theme.Item.Render(e.Mode), detailRowW))
		}
		if e.Owner != "" && e.Owner != "(windows)" {
			rightLines = append(rightLines,
				"  "+dotLeader("Owner", theme.Item.Render(e.Owner), detailRowW))
		}
		if e.ModTime != "" {
			rightLines = append(rightLines,
				"  "+dotLeader("Modified", theme.Item.Render(e.ModTime), detailRowW))
		}
		if e.IsSymlink {
			// "Symlinked" dot-leader row mirrors the other detail rows so
			// it integrates into the visual rhythm. Value is the literal
			// word "yes" in cyan to match the ↗ marker color used in the
			// left pane — same metaphor across both panes.
			rightLines = append(rightLines,
				"  "+dotLeader("Symlinked",
					lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).
						Bold(true).Render("yes ↗"),
					detailRowW))
		}
		rightLines = append(rightLines, "",
			theme.ItemDim.Render("  Full path:"),
			"  "+theme.AccentHiS.Render(joinPath(f.device, f.cwd, e.Name)))
		if e.IsSymlink && e.LinkTarget != "" {
			rightLines = append(rightLines,
				"",
				theme.ItemDim.Render("  Link target:"),
				"  "+lipgloss.NewStyle().Foreground(lipgloss.Color("#22d3ee")).
					Render(e.LinkTarget))
		}
	}
	for len(rightLines) < bodyH {
		rightLines = append(rightLines, "")
	}

	// Premium layout: drop the heavy adjacent rounded-borders ("dashed
	// vertical lines" the operator flagged were the two box borders
	// stacked across the gutter) and use a single subtle vertical
	// separator in muted color. Left pane is borderless; right pane is
	// borderless. The separator is one cell wide between them.
	leftBox := lipgloss.NewStyle().
		Width(leftW).Height(bodyH + 2).
		Render(strings.Join(leftLines, "\n"))

	// Vertical separator column — one `│` per body row, muted color.
	sepStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#2a2a2a"))
	sepCol := make([]string, bodyH+2)
	for i := range sepCol {
		sepCol[i] = sepStyle.Render("│")
	}
	separator := strings.Join(sepCol, "\n")

	rightBox := lipgloss.NewStyle().
		Width(rightW).Height(bodyH + 2).
		Render(strings.Join(rightLines, "\n"))

	hiddenState := "hidden"
	if f.showHidden {
		hiddenState = "shown"
	}
	hint := lipgloss.NewStyle().Foreground(theme.Text).Render("  ↑↓ ") +
		theme.ItemDim.Render("move · ") +
		lipgloss.NewStyle().Foreground(theme.Text).Render("Enter ") +
		theme.ItemDim.Render("select / drill · ") +
		lipgloss.NewStyle().Foreground(theme.Text).Render("← Bksp ") +
		theme.ItemDim.Render("parent · ") +
		lipgloss.NewStyle().Foreground(theme.Text).Render(". ") +
		theme.ItemDim.Render("hidden: "+hiddenState+" · ") +
		lipgloss.NewStyle().Foreground(theme.Text).Render("Esc ") +
		theme.ItemDim.Render("cancel")

	body := lipgloss.JoinVertical(lipgloss.Left,
		header,
		lipgloss.JoinHorizontal(lipgloss.Top, leftBox, " ", separator, " ", rightBox),
		hint)
	return body
}

// updateSearch handles key events when the fuzzy search overlay is open.
// Esc closes the overlay returning to normal browsing. Enter on a result
// drills the browser into that path (or its parent for files). Up/Down
// navigate results. All other keys flow through textinput, and every
// edit retriggers applySearch().
func (f *fileBrowser) updateSearch(m tea.KeyMsg) (*fileBrowser, tea.Cmd) {
	switch m.String() {
	case "esc":
		f.searchActive = false
		return f, nil
	case "enter":
		if f.searchIdx >= 0 && f.searchIdx < len(f.searchResults) {
			r := f.searchResults[f.searchIdx]
			target := r.Path
			if !r.IsDir {
				// Land in the file's parent directory; the file itself
				// can't be a share target. parentPath handles trailing
				// slash edge cases consistently.
				target = parentPath(f.device, target)
			}
			f.cwd = target
			f.loading = true
			f.searchActive = false
			return f, f.loadCmd()
		}
		return f, nil
	case "up", "ctrl+k":
		if f.searchIdx > 0 {
			f.searchIdx--
		}
		return f, nil
	case "down", "ctrl+j":
		if f.searchIdx < len(f.searchResults)-1 {
			f.searchIdx++
		}
		return f, nil
	case "pgup":
		f.searchIdx -= 10
		if f.searchIdx < 0 {
			f.searchIdx = 0
		}
		return f, nil
	case "pgdown":
		f.searchIdx += 10
		if f.searchIdx >= len(f.searchResults) {
			f.searchIdx = len(f.searchResults) - 1
		}
		return f, nil
	}
	// Any other key -> feed to the text input, then refilter.
	var cmd tea.Cmd
	f.searchInput, cmd = f.searchInput.Update(m)
	f.applySearch()
	return f, cmd
}

// renderSearchView produces the full file-browser pane when search is
// active: input row at the top, scrollable results list below. Replaces
// the normal two-pane layout entirely so the operator has the full width
// for path display.
func (f *fileBrowser) renderSearchView(w, h int) string {
	bodyH := h - 5

	header := f.renderDeviceBadge()
	statusLine := ""
	switch {
	case f.searchScanning:
		statusLine = theme.ItemDim.Render("  indexing filesystem… (")
		statusLine += theme.Info.Render(f.searchRoot())
		statusLine += theme.ItemDim.Render(")")
	case f.searchIndexErr != nil:
		statusLine = theme.Err.Render("  ✗ index error: " + f.searchIndexErr.Error())
	default:
		statusLine = theme.ItemDim.Render(fmt.Sprintf(
			"  %d entries indexed under %s · %d matches",
			len(f.searchIndex), f.searchRoot(), len(f.searchResults)))
	}

	inputBox := lipgloss.NewStyle().
		Background(lipgloss.Color("#1a1a1a")).
		Padding(0, 1).
		Width(w - 2).
		Render(f.searchInput.View())

	// Scroll math for the results list.
	if f.searchIdx < f.searchScroll {
		f.searchScroll = f.searchIdx
	}
	if f.searchIdx-f.searchScroll >= bodyH {
		f.searchScroll = f.searchIdx - bodyH + 1
	}
	end := f.searchScroll + bodyH
	if end > len(f.searchResults) {
		end = len(f.searchResults)
	}

	var lines []string
	for i := f.searchScroll; i < end; i++ {
		r := f.searchResults[i]
		icon := ""
		if r.IsDir {
			icon = ""
		}
		isSel := i == f.searchIdx
		nameStyled := r.Path
		// Truncate from the LEFT so the basename stays visible — most
		// useful when fuzzy-matching by leaf name.
		maxW := w - 6
		if lipgloss.Width(nameStyled) > maxW {
			runes := []rune(nameStyled)
			for lipgloss.Width(string(runes))+1 > maxW {
				runes = runes[1:]
			}
			nameStyled = "…" + string(runes)
		}
		row := "  " + icon + " " + nameStyled
		if isSel {
			row = lipgloss.NewStyle().
				Background(theme.AccentHi).Foreground(lipgloss.Color("#000000")).
				Bold(true).Render(rightpad(row, w-2))
		} else {
			zebra := (i % 2) == 0
			bg := lipgloss.Color("#161616")
			if !zebra {
				bg = lipgloss.Color("#1c1c1c")
			}
			row = lipgloss.NewStyle().Background(bg).
				Render(rightpad(row, w-2))
		}
		lines = append(lines, row)
	}
	for len(lines) < bodyH {
		rowIdx := len(lines) + f.searchScroll
		bg := lipgloss.Color("#161616")
		if rowIdx%2 != 0 {
			bg = lipgloss.Color("#1c1c1c")
		}
		lines = append(lines,
			lipgloss.NewStyle().Background(bg).
				Render(strings.Repeat(" ", w-2)))
	}

	hint := theme.ItemDim.Render(
		"  type to fuzzy-match · ↑↓ navigate · Enter drill into · Esc close search")

	return lipgloss.JoinVertical(lipgloss.Left,
		"  "+header,
		statusLine,
		"",
		inputBox,
		"",
		strings.Join(lines, "\n"),
		hint,
	)
}

func (f *fileBrowser) deviceLabel() string {
	if f.device == "" || f.device == local.HostName() {
		return "[LOCAL]"
	}
	return "[" + f.device + "]"
}

// renderDeviceBadge produces the top-right corner indicator: TWO tags
// side by side. The first is the access-mode (LOCAL = green; REMOTE =
// amber) — answers "how am I touching this filesystem?". The second is
// the device's tailnet hostname (cyan) prefixed with a device-type
// nerd-font icon — answers "which device's filesystem is it?" with a
// glanceable visual cue for what KIND of device. Two pieces of distinct
// information, two distinct badges, side by side at the top-right corner.
func (f *fileBrowser) renderDeviceBadge() string {
	hostname := local.HostName()
	if f.device != "" {
		hostname = f.device
	}
	isLocal := f.device == "" || f.device == local.HostName()

	var modeBG lipgloss.Color
	var modeLabel string
	if isLocal {
		modeBG = lipgloss.Color("#15803d") // green-700
		modeLabel = "LOCAL"
	} else {
		modeBG = lipgloss.Color("#d97706") // amber-600
		modeLabel = "REMOTE"
	}
	modeTag := lipgloss.NewStyle().
		Background(modeBG).
		Foreground(lipgloss.Color("#ffffff")).
		Bold(true).
		Padding(0, 1).
		Render(modeLabel)

	icon := deviceTypeIcon(hostname)
	hostTag := lipgloss.NewStyle().
		Background(lipgloss.Color("#0891b2")). // cyan-600
		Foreground(lipgloss.Color("#ffffff")).
		Bold(true).
		Padding(0, 1).
		Render(icon + " " + hostname)
	return modeTag + " " + hostTag
}

// deviceTypeIcon returns a single-cell nerd-font glyph appropriate for
// the device's role, inferred from the n3m-* hostname prefix. Special-
// cased: n3m-srv-01 is the UGREEN DXP2800 NAS (named srv- but functionally
// network-attached storage) so it gets the NAS icon. Unknown hostnames
// fall back to a generic question-mark glyph.
func deviceTypeIcon(hostname string) string {
	// Special cases first — must precede the prefix dispatch.
	if hostname == "n3m-srv-01" {
		return "" // nf-fa-hdd_o (NAS)
	}
	switch {
	case strings.HasPrefix(hostname, "n3m-srv-"):
		return "" // nf-fa-server
	case strings.HasPrefix(hostname, "n3m-nas-"):
		return "" // nf-fa-hdd_o
	case strings.HasPrefix(hostname, "n3m-wks-"):
		return "" // nf-fa-laptop (operator's wks-* are MacBooks)
	case strings.HasPrefix(hostname, "n3m-mob-"):
		return "" // nf-fa-mobile
	case strings.HasPrefix(hostname, "n3m-ent-"):
		return "" // nf-fa-tv
	case strings.HasPrefix(hostname, "n3m-prt-"):
		return "" // nf-fa-print
	case strings.HasPrefix(hostname, "n3m-idr-"):
		return "" // nf-fa-cogs (iDRAC management)
	}
	return "" // nf-fa-question (unknown)
}

func stringIfDir(isDir bool, t string, fOpt ...string) string {
	if isDir {
		return t
	}
	if len(fOpt) > 0 {
		return fOpt[0]
	}
	return ""
}

func rightpad(s string, w int) string {
	width := lipgloss.Width(s)
	if width >= w {
		return s
	}
	return s + strings.Repeat(" ", w-width)
}

// max0 clamps negatives to 0 — used for padding calculations where a
// negative diff would otherwise produce a runtime panic in strings.Repeat.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// dotLeader renders a `Label ················ Value` row sized to fit
// totalW cells. The label gets the Title style, the value is taken
// pre-rendered (caller styles it however they like), and the dots between
// them use ItemDim. Minimum 3 dots so very long values don't collapse the
// leader to nothing.
func dotLeader(label, value string, totalW int) string {
	labelStyled := theme.Title.Render(label)
	labelW := lipgloss.Width(labelStyled)
	valueW := lipgloss.Width(value)
	// 2 spaces (one each side of the dot run) + the dots themselves.
	gap := totalW - labelW - valueW - 2
	if gap < 3 {
		gap = 3
	}
	dots := theme.ItemDim.Render(strings.Repeat(".", gap))
	return labelStyled + " " + dots + " " + value
}

func humanSize(n int64) string {
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
