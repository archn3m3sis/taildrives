// Package local exposes shares published by THIS host via `tailscale drive list`.
// They aren't visible via the magic WebDAV endpoint to the host itself, only
// to other tailnet devices.
package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Share is a locally-published Tailscale Drive share.
type Share struct {
	Name string
	Path string // filesystem path on this host
	As   string // OS user it's exposed as
}

// HostName returns the Tailscale host name of this device, falling back to
// os.Hostname() on any error. All tailscale CLI calls are bounded with a
// short timeout so a wedged daemon can't hang the TUI.
//
// Parser gotcha (fixed 2026-05-18): we MUST drop stderr (via .Output()
// instead of .CombinedOutput()) AND validate that field[0] of any candidate
// line parses as an IP. Otherwise a Tailscale version-mismatch warning
// (emitted when a host has the formula CLI installed alongside the App's
// older CLI) is parsed as a status row, returning "client" as the hostname
// — which then breaks the device == local.HostName() check in the file
// browser, sending what should be a local browse onto the SSH path.
func HostName() string {
	bin := tsBin()
	if bin == "" {
		h, _ := os.Hostname()
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--self", "--peers=false").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && looksLikeIP(fields[0]) {
				return fields[1]
			}
		}
	}
	h, _ := os.Hostname()
	return h
}

// ListShares returns this host's published Taildrive shares. Tries the
// `tailscale drive list` CLI first (Linux/Windows/standalone-macOS).
// Falls back to parsing `tailscale debug prefs` JSON (macOS GUI variants,
// where the `drive` subcommand isn't exposed).
var ErrUnavailable = errors.New("tailscale CLI not available — local share list unavailable")

func ListShares() ([]Share, error) {
	bin := tsBin()
	if bin == "" {
		return nil, ErrUnavailable
	}
	out, err := runTS(bin, 5*time.Second, "drive", "list")
	if err == nil && !strings.Contains(string(out), "unknown subcommand") {
		return parseDriveList(string(out)), nil
	}
	out, err = runTS(bin, 5*time.Second, "debug", "prefs")
	if err != nil {
		return nil, ErrUnavailable
	}
	return parsePrefsShares(out), nil
}

// runTS is the canonical bounded-context wrapper for tailscale CLI calls.
// Every CLI invocation in this package goes through it so a wedged
// daemon can never lock the TUI.
func runTS(bin string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).CombinedOutput()
}

// parsePrefsShares pulls the DriveShares array out of `tailscale debug prefs`
// JSON output, defensively (only the fields we care about).
func parsePrefsShares(raw []byte) []Share {
	var prefs struct {
		DriveShares []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"DriveShares"`
	}
	if err := json.Unmarshal(raw, &prefs); err != nil {
		return nil
	}
	out := make([]Share, 0, len(prefs.DriveShares))
	for _, s := range prefs.DriveShares {
		out = append(out, Share{Name: s.Name, Path: s.Path, As: ""})
	}
	return out
}

// parseDriveList parses the columnar output:
//
//	name           path            as
//	-----------    ------------    ----
//	share1         /path/to/dir    root
func parseDriveList(s string) []Share {
	var out []Share
	sc := bufio.NewScanner(strings.NewReader(s))
	saw := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !saw {
			// header row contains "name" / "path"
			if strings.HasPrefix(strings.TrimSpace(line), "name") {
				saw = true
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "---") {
			continue
		}
		fields := splitColumns(line)
		if len(fields) < 2 {
			continue
		}
		sh := Share{Name: fields[0], Path: fields[1]}
		if len(fields) >= 3 {
			sh.As = fields[2]
		}
		out = append(out, sh)
	}
	return out
}

// splitColumns splits a line by 2+ spaces (column delimiter in `tailscale drive list`).
func splitColumns(line string) []string {
	parts := strings.Fields(line)
	// `tailscale drive list` outputs columns separated by runs of spaces.
	// Fields() already does the right thing for our purposes.
	return parts
}

// LocalEntries lists the contents of a local share path with a small subset of
// the WebDAV Entry shape so the TUI can render them through the same code path.
//
// Symlink handling: os.ReadDir's DirEntry.IsDir() returns false for symlinks
// because the entry IS a symlink, not a directory. On macOS that means
// /etc, /tmp, /var, /home — all symlinks into /private — appear as files.
// We resolve symlinks via os.Stat (which follows them) and use the target's
// type so the operator can drill into them like the directories they
// effectively are.
func LocalEntries(dir string) ([]LocalEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]LocalEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		isDir := e.IsDir()
		isSymlink := info.Mode()&os.ModeSymlink != 0
		var linkTarget string
		if isSymlink {
			if t, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
				linkTarget = t
			}
			if target, err := os.Stat(filepath.Join(dir, e.Name())); err == nil {
				isDir = target.IsDir()
			}
		}
		out = append(out, LocalEntry{
			Name:       e.Name(),
			Path:       filepath.Join(dir, e.Name()),
			IsDir:      isDir,
			IsSymlink:  isSymlink,
			LinkTarget: linkTarget,
			Size:       info.Size(),
			Mode:       info.Mode().String(),
			ModTime:    info.ModTime().Format("2006-01-02 15:04"),
		})
	}
	return out, nil
}

type LocalEntry struct {
	Name       string
	Path       string
	IsDir      bool
	IsSymlink  bool   // true when the dirent itself is a symlink (target type lives in IsDir)
	LinkTarget string // resolved target, if symlink (best-effort)
	Size       int64
	Mode       string // e.g. "drwxr-xr-x" / "-rw-r--r--"
	ModTime    string // pre-formatted "2006-01-02 15:04" for direct display
}

// TailnetDevice is a peer device known to the local Tailscale daemon, along
// with online status. This is what lets the TUI distinguish "device not in
// Taildrive listing because offline" from "device doesn't exist."
type TailnetDevice struct {
	Hostname string
	Online   bool
	OS       string
	LastSeen string // free-form, e.g. "1h ago"
}

// SelfLoginName returns a best-effort tailnet identity for the operator
// running this binary. Resolution order:
//   1. tailscale status JSON → Self.User → User[id].LoginName (works on
//      workstations where the daemon is logged in as a human)
//   2. tailscale status plain → 3rd column (works on un-tagged nodes)
//   3. fallback: $USER@$HOSTNAME so tagged servers still produce a stable
//      identity string for matching ("archn3m3sis@n3m-srv-02")
//
// Returns "" if neither tailscale nor the env are available.
func SelfLoginName() string {
	if name := selfFromJSON(); name != "" && !looksLikeTagOrNode(name) {
		return name
	}
	if name := selfFromPlain(); name != "" && !looksLikeTagOrNode(name) {
		return name
	}
	// OS fallback — useful for tagged servers where Tailscale doesn't
	// expose a human owner.
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME") // windows
	}
	host, _ := os.Hostname()
	if user != "" && host != "" {
		return user + "@" + host
	}
	return user
}

func selfFromJSON() string {
	bin := tsBin()
	if bin == "" {
		return ""
	}
	out, err := runTS(bin, 5*time.Second, "status", "--json")
	if err != nil {
		return ""
	}
	var st struct {
		Self struct {
			UserID int64 `json:"UserID"`
		} `json:"Self"`
		User map[string]struct {
			LoginName string `json:"LoginName"`
		} `json:"User"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return ""
	}
	if u, ok := st.User[fmt.Sprintf("%d", st.Self.UserID)]; ok {
		return u.LoginName
	}
	return ""
}

func selfFromPlain() string {
	bin := tsBin()
	if bin == "" {
		return ""
	}
	out, err := runTS(bin, 5*time.Second, "status", "--self", "--peers=false")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !looksLikeIP(fields[0]) {
			continue
		}
		return fields[2]
	}
	return ""
}

// looksLikeTagOrNode returns true when Tailscale gave us a tag identity
// (tagged-devices) or a node DNS name instead of a human email — both
// useless for "is this the operator?" matching.
func looksLikeTagOrNode(name string) bool {
	if name == "tagged-devices" {
		return true
	}
	if strings.Contains(name, ".ts.net") {
		return true
	}
	if strings.HasPrefix(name, "userid:") {
		return true
	}
	return false
}

// Unshare runs `tailscale drive unshare <name>` on the local host. Used when
// a user deletes a Taildrive share root via the TUI — we don't only delete
// the folder, we also remove the share registration.
func Unshare(name string) error {
	bin := tsBin()
	if bin == "" {
		return ErrUnavailable
	}
	out, err := runTS(bin, 5*time.Second, "drive", "unshare", name)
	if err != nil {
		// On macOS GUI the CLI doesn't expose drive — propagate the message
		// so the caller can surface it.
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

// TailnetDevices runs `tailscale status` and parses the human-readable output.
// Returns ErrUnavailable if the tailscale CLI isn't installed.
func TailnetDevices() ([]TailnetDevice, error) {
	bin := tsBin()
	if bin == "" {
		return nil, ErrUnavailable
	}
	out, err := runTS(bin, 5*time.Second, "status")
	if err != nil {
		return nil, ErrUnavailable
	}
	return parseStatus(string(out)), nil
}

// parseStatus parses lines like:
//
//	100.85.149.111   n3m-srv-01    archn3m3sis@   linux    -
//	100.96.182.55    n3m-wks-02    archn3m3sis@   macOS    active; relay "nyc"; offline, last seen 1h ago, ...
func parseStatus(s string) []TailnetDevice {
	var out []TailnetDevice
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// Quick sanity check: first field should look like an IP.
		if !looksLikeIP(fields[0]) {
			continue
		}
		dev := TailnetDevice{
			Hostname: fields[1],
			OS:       fields[3],
			Online:   true,
		}
		rest := strings.Join(fields[4:], " ")
		lower := strings.ToLower(rest)
		if strings.Contains(lower, "offline") {
			dev.Online = false
			if i := strings.Index(lower, "last seen "); i >= 0 {
				tail := rest[i+len("last seen "):]
				if c := strings.Index(tail, ","); c >= 0 {
					tail = tail[:c]
				}
				dev.LastSeen = strings.TrimSpace(tail)
			}
		}
		out = append(out, dev)
	}
	return out
}

func looksLikeIP(s string) bool {
	dots := 0
	for _, r := range s {
		switch {
		case r == '.':
			dots++
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return dots == 3
}

// tsBin returns the tailscale CLI path, or "" if not found.
//
// macOS gotcha: /usr/local/bin/tailscale is a shim into Tailscale.app's
// embedded CLI, which is built WITHOUT the `drive` subcommand. The Homebrew
// formula `tailscale` lands at /opt/homebrew/bin and IS a full CLI with
// drive support; it shares the App's tailscaled socket. So on macOS we
// MUST check /opt/homebrew first or all our drive-* calls will fail with
// "unknown subcommand: drive". See [[reference_tailscale_macos_variants]].
func tsBin() string {
	for _, p := range []string{
		"/run/current-system/sw/bin/tailscale",
		"/usr/bin/tailscale",
		"/opt/homebrew/bin/tailscale", // must precede /usr/local/bin on macOS
		"/usr/local/bin/tailscale",
		"C:\\Program Files\\Tailscale\\tailscale.exe",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	return ""
}
