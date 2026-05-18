// Package exportkit centralizes the cross-cutting export plumbing that
// every report-like overlay needs: clipboard via OSC 52, print via
// temp-file + system-default-viewer, the centered 5-second "exported for
// printing" notification popup, and ANSI-stripping for clean text
// output. Used by wizards (perf/device-status/journal reports) and by
// netmap (the topology diagram).
package exportkit

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/archn3m3sis/taildrives/internal/theme"
)

// ANSIRe matches CSI/SGR escape sequences emitted by lipgloss styling.
// Used to plain-ify content before sending it to the clipboard or to a
// printable temp file — what lands on the clipboard / in the editor is
// readable text, not escape-soup.
var ANSIRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// PrintDismissMsg fires from the tea.Tick set up when Ctrl+P is pressed.
// Receiving it should clear the caller's print-notification deadline so
// the next View drops the popup. Exposed so overlays can match on it.
type PrintDismissMsg struct{}

// CopyToClipboardCmd returns a tea.Cmd that pushes content to the system
// clipboard via OSC 52. Strips ANSI before encoding so the clipboard
// payload is plain text.
func CopyToClipboardCmd(content string) tea.Cmd {
	return func() tea.Msg {
		stripped := ANSIRe.ReplaceAllString(content, "")
		seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(stripped)) + "\x07"
		os.Stdout.WriteString(seq)
		return nil
	}
}

// PrintReportCmd writes content (ANSI-stripped) to a timestamped temp
// file, then spawns the OS default-handler so the operator gets a native
// print dialog (Cmd+P / Ctrl+P inside that editor).
//
// macOS: `open` → TextEdit by default
// Linux: `xdg-open` → desktop MIME handler
// Windows: `cmd /c start` → associated app
//
// Returns nil — fire-and-forget; the spawn detaches.
func PrintReportCmd(content, title string) tea.Cmd {
	return func() tea.Msg {
		stripped := ANSIRe.ReplaceAllString(content, "")
		slug := SlugifyTitle(title)
		fname := "taildrives-" + slug + "-" + time.Now().Format("20060102-150405") + ".txt"
		path := filepath.Join(os.TempDir(), fname)
		if err := os.WriteFile(path, []byte(stripped), 0o644); err != nil {
			return nil
		}
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", path)
		case "linux":
			c = exec.Command("xdg-open", path)
		case "windows":
			c = exec.Command("cmd", "/c", "start", "", path)
		}
		if c != nil {
			_ = c.Start()
		}
		return nil
	}
}

// PrintDismissCmd returns a tea.Tick that fires PrintDismissMsg after
// the standard 5-second display window. Pair with PrintReportCmd via
// tea.Batch to set up the auto-dismiss timer.
func PrintDismissCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return PrintDismissMsg{}
	})
}

// SlugifyTitle turns a free-form title into a filesystem-safe slug for
// the temp filename. "TAILSCALE DEVICE STATUS" → "tailscale-device-status".
func SlugifyTitle(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// RenderPrintNotification produces the centered popup body shown for the
// 5-second window after Ctrl+P fires. Amber-bordered, explains the
// export-to-editor flow + countdown.
func RenderPrintNotification(secondsLeft, w int) string {
	bw := 72
	if w < 80 {
		bw = w - 8
	}
	icon := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fbbf24")).Bold(true).
		Render("")
	title := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#fbbf24")).Bold(true).Reverse(true).
		Padding(0, 2).
		Render("  " + icon + "  EXPORTED FOR PRINTING ")
	msgLines := []string{
		"",
		lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("  TAILSCALE-CLI has exported your summary to your"),
		lipgloss.NewStyle().Foreground(theme.Text).Bold(true).
			Render("  device's local default text editing program."),
		"",
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  For a physical print of your summary, please print"),
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  from the context menu inside of your text editor"),
		lipgloss.NewStyle().Foreground(theme.AccentHi).
			Render("  (typically Cmd+P on macOS, Ctrl+P on Linux/Windows)."),
		"",
		lipgloss.NewStyle().Foreground(theme.ItemDim.GetForeground()).Italic(true).
			Render(fmt.Sprintf("  This notification auto-dismisses in %ds…", secondsLeft)),
		"",
	}
	body := title + "\n" + strings.Join(msgLines, "\n")
	return lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color("#fbbf24")).
		Background(lipgloss.Color("#1a1a1a")).
		Padding(0, 1).
		Width(bw).
		Render(body)
}

// OverlayCenter composites `popup` centered onto `underlay`. Splits into
// lines, overwrites the middle rows with the popup so it floats on top
// of whatever was being shown.
func OverlayCenter(underlay, popup string, w, h int) string {
	bw := lipgloss.Width(popup)
	bh := lipgloss.Height(popup)
	x := (w - bw) / 2
	y := (h - bh) / 2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	mainLines := strings.Split(underlay, "\n")
	popupLines := strings.Split(popup, "\n")
	for i, pl := range popupLines {
		row := y + i
		if row >= len(mainLines) {
			break
		}
		mainLines[row] = strings.Repeat(" ", x) + pl
	}
	return strings.Join(mainLines, "\n")
}
