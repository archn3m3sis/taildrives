// Command taildrives is a TUI + CLI for browsing and transferring files via
// Tailscale Drive (WebDAV at http://100.100.100.100:8080).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/archn3m3sis/taildrives/internal/actions"
	"github.com/archn3m3sis/taildrives/internal/cats"
	"github.com/archn3m3sis/taildrives/internal/cfg"
	"github.com/archn3m3sis/taildrives/internal/local"
	"github.com/archn3m3sis/taildrives/internal/outro"
	"github.com/archn3m3sis/taildrives/internal/splash"
	"github.com/archn3m3sis/taildrives/internal/theme"
	"github.com/archn3m3sis/taildrives/internal/tui"
	"github.com/archn3m3sis/taildrives/internal/webdav"
	"github.com/archn3m3sis/taildrives/internal/wizards"
	"github.com/archn3m3sis/taildrives/internal/xfer"
)

// Version is overridden via -ldflags="-X main.Version=…".
var Version = "dev"

const (
	repoURL    = "https://github.com/archn3m3sis/taildrives"
	issuesURL  = repoURL + "/issues"
	featureURL = repoURL + "/issues/new?labels=enhancement&title=%5Bfeature%5D+"
)

func openBrowser(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("cmd", "/c", "start", "", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

const usage = `taildrives — Tailscale Drive TUI + CLI for the n3m3sis devnet

USAGE
  taildrives                       launch the TUI
  taildrives <command> [args]

COMMANDS
  list                             list all shares on every device, with categories
  types                            list available categories and what they match
  set --type CAT                   set the default filter category
  ls PATH                          list contents of a WebDAV path
  cat PATH                         print a file's contents to stdout
  get SRC [DST]                    download SRC to DST (defaults to ~/Downloads/basename)
  put SRC DST                      upload local SRC to WebDAV DST
  copy SRC DST                     copy SRC to DST (server-side when same share, else stream)
  bulk-send SRC DEV1 [DEV2 ...]    fan SRC out to <dev>_taildrop on each device
  mount [DIR]                      print davfs2 mount command for DIR (default /mnt/taildrive)
  serve                            print the WebDAV URL
  devices                          list device names visible in your tailnet
  version                          print version

EXAMPLES
  taildrives list
  taildrives set --type taildrops
  taildrives ls /archn3m3sis.bounties@gmail.com/n3m-srv-02/
  taildrives get /archn3m3sis.bounties@gmail.com/n3m-srv-02/n3m_srv_02_taildrop/notes.md
  taildrives copy /archn3m3sis…/n3m-srv-02/n3m_srv_02_taildrop/a.txt /archn3m3sis…/n3m-wks-01/n3m_wks_01_taildrop/a.txt
  taildrives bulk-send ~/Pictures/cat.png n3m-srv-01 n3m-srv-02 n3m-wks-01
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, theme.Err.Render("error: ")+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return runTUI()
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	case "version", "--version", "-v":
		fmt.Printf("taildrives %s  %s %s/%s\n", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	case "list":
		return cmdList(args[1:])
	case "types":
		return cmdTypes()
	case "set":
		return cmdSet(args[1:])
	case "ls":
		return cmdLs(args[1:])
	case "cat":
		return cmdCat(args[1:])
	case "get":
		return cmdGet(args[1:])
	case "put":
		return cmdPut(args[1:])
	case "copy":
		return cmdCopy(args[1:])
	case "bulk-send":
		return cmdBulkSend(args[1:])
	case "mount":
		return cmdMount(args[1:])
	case "serve":
		fmt.Println("http://100.100.100.100:8080")
		fmt.Println("On macOS Finder: Cmd+K → enter URL → Connect as Guest")
		fmt.Println("On Linux davfs2: sudo mount -t davfs http://100.100.100.100:8080 /mnt/taildrive")
		return nil
	case "devices":
		return cmdDevices(args[1:]...)
	case "selftest":
		return cmdSelfTest()
	case "status-report":
		actions.RunDeviceStatusReport(local.SelfLoginName())
		return nil
	case "performance-report":
		actions.RunPerformanceReport(local.SelfLoginName())
		return nil
	case "journal":
		actions.RunJournalViewer(local.SelfLoginName())
		return nil
	}
	return fmt.Errorf("unknown command %q — try `taildrives help`", args[0])
}

// ── shared bits ────────────────────────────────────────────────────────────

func client() *webdav.Client {
	c := cfg.Load()
	return webdav.New(c.Endpoint)
}

func resolveNamespace(c *webdav.Client) (string, error) {
	saved := cfg.Load().UserNamespace
	if saved != "" {
		return saved, nil
	}
	ns, err := c.AutoUserNamespace()
	if err != nil {
		return "", fmt.Errorf("could not discover WebDAV namespace — is Tailscale running and Drive enabled? (%w)", err)
	}
	cur := cfg.Load()
	cur.UserNamespace = ns
	_ = cfg.Save(cur)
	return ns, nil
}

// ── runTUI ─────────────────────────────────────────────────────────────────

func runTUI() error {
	if os.Getenv("TAILDRIVES_NO_SPLASH") != "1" {
		// Splash hosts global-action overlays IN-PROCESS via this factory.
		// Returning from the splash program only happens when the user picks
		// an action that genuinely leaves the TUI (Enter/Help/GitHub/etc).
		factory := splash.OverlayFactory(func(a splash.Action) splash.Overlay {
			actor := local.SelfLoginName()
			switch a {
			case splash.ActionDeviceStatusReport:
				return wizards.NewDeviceStatusReport(actor)
			case splash.ActionPerformanceReport:
				return wizards.NewPerformanceReport(actor)
			case splash.ActionLifecycleJournal:
				return wizards.NewJournalViewer(actor)
			case splash.ActionAddShares:
				return wizards.NewAddShareWizard(actor)
			case splash.ActionRemoveShares:
				return wizards.NewRemoveShareWizard(actor)
			}
			return nil
		})
		sp := tea.NewProgram(splash.NewWithFactory(factory), tea.WithAltScreen())
		final, err := sp.Run()
		if err != nil {
			return err
		}
		s := final.(splash.Model)
		// Double-Esc exit from anywhere inside the splash. Play the outro
		// then quit — skip the action switch below since the user already
		// signaled they want out.
		if s.OutroRequested {
			_ = outro.Play()
			return nil
		}
		switch s.Action {
		case splash.ActionQuit:
			return nil
		case splash.ActionGitHub:
			return openBrowser(repoURL)
		case splash.ActionIssues:
			return openBrowser(issuesURL)
		case splash.ActionFeatureRequest:
			return openBrowser(featureURL)
		case splash.ActionHelp:
			fmt.Print(usage)
			return nil
		case splash.ActionEnter, splash.ActionNone:
			return runMainTUI()
		}
		return nil
	}
	return runMainTUI()
}

func runMainTUI() error {
	c := cfg.Load()
	dav := webdav.New(c.Endpoint)
	mgr := xfer.New(dav, c.Concurrency)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()

	p := tea.NewProgram(tui.New(c, dav, mgr), tea.WithAltScreen(), tea.WithMouseCellMotion())
	final, err := p.Run()
	if err != nil {
		return err
	}
	// Double-Esc anywhere in the main TUI sets OutroRequested before quit.
	// Same outro program as splash uses — one farewell animation, called
	// from whichever pane the operator exited from.
	if m, ok := final.(tui.Model); ok && m.OutroRequested {
		_ = outro.Play()
	}
	return nil
}

// ── list ───────────────────────────────────────────────────────────────────

type discoveredShare struct {
	Device string
	Share  string
	Path   string
}

func discoverShares(dav *webdav.Client) (string, []discoveredShare, error) {
	ns, err := resolveNamespace(dav)
	if err != nil {
		return "", nil, err
	}
	devEntries, err := dav.List("/" + ns)
	if err != nil {
		return ns, nil, fmt.Errorf("list devices: %w", err)
	}
	var out []discoveredShare
	for _, d := range devEntries {
		if !d.IsDir {
			continue
		}
		if webdav.IsSidecarDevice(d.Name) {
			continue
		}
		shareEntries, err := dav.List("/" + ns + "/" + d.Name)
		if err != nil {
			continue
		}
		for _, s := range shareEntries {
			if !s.IsDir {
				continue
			}
			out = append(out, discoveredShare{
				Device: d.Name,
				Share:  s.Name,
				Path:   "/" + ns + "/" + d.Name + "/" + s.Name,
			})
		}
	}
	// Augment with locally-published shares (Tailscale Drive hides a host's
	// own shares from itself via WebDAV — only visible to peers).
	if locals, err := local.ListShares(); err == nil {
		hn := local.HostName()
		for _, s := range locals {
			out = append(out, discoveredShare{
				Device: hn + " (this device)",
				Share:  s.Name,
				Path:   "file://" + s.Path,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].Share < out[j].Share
	})
	return ns, out, nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	cat := fs.String("type", "", "filter by category (default = saved category, or 'all')")
	jsonOut := fs.Bool("json", false, "emit JSON instead of the table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dav := client()
	ns, shares, err := discoverShares(dav)
	if err != nil {
		return err
	}
	filter := *cat
	if filter == "" {
		filter = cfg.Load().Category
	}
	if filter == "" {
		filter = "No Active Content Filtering"
	}
	catObj, ok := cats.ByName(filter)
	if !ok {
		return fmt.Errorf("unknown category %q — `taildrives types` to list", filter)
	}
	if *jsonOut {
		return emitJSONShares(ns, shares, catObj)
	}
	return emitTableShares(ns, shares, catObj, filter)
}

func emitJSONShares(ns string, shares []discoveredShare, c cats.Category) error {
	fmt.Printf("{\n  \"namespace\": %q,\n  \"shares\": [\n", ns)
	first := true
	for _, s := range shares {
		if !c.Matches(s.Device, s.Share) {
			continue
		}
		if !first {
			fmt.Println(",")
		}
		first = false
		fmt.Printf("    {\"device\": %q, \"share\": %q, \"path\": %q, \"categories\": [%s]}",
			s.Device, s.Share, s.Path, quoteJoin(cats.Classify(s.Device, s.Share)))
	}
	fmt.Print("\n  ]\n}\n")
	return nil
}

func quoteJoin(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}

func emitTableShares(ns string, shares []discoveredShare, c cats.Category, filterName string) error {
	fmt.Println(theme.Banner.Render("taildrives — share inventory"))
	fmt.Println(theme.ItemDim.Render("namespace: ") + theme.Info.Render(ns))
	fmt.Println(theme.ItemDim.Render("filter:    ") + theme.Info.Render(filterName))
	fmt.Println()

	curDev := ""
	count := 0
	for _, s := range shares {
		dev := strings.TrimSuffix(s.Device, " (this device)")
		if !c.Matches(dev, s.Share) {
			continue
		}
		if s.Device != curDev {
			if curDev != "" {
				fmt.Println()
			}
			fmt.Println(theme.TitleActive.Render("● " + s.Device))
			curDev = s.Device
		}
		classes := cats.Classify(dev, s.Share)
		fmt.Printf("    %s  %s\n",
			theme.AccentHiS.Render(s.Share),
			theme.ItemDim.Render("["+strings.Join(filterCats(classes), ", ")+"]"))
		fmt.Printf("    %s\n", theme.ItemDim.Render(s.Path))
		count++
	}
	fmt.Println()
	fmt.Printf("%s %d shares match category %s\n",
		theme.OK.Render("✓"), count, theme.Info.Render(filterName))
	return nil
}

func filterCats(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s != "No Active Content Filtering" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{"No Active Content Filtering"}
	}
	return out
}

// ── types ──────────────────────────────────────────────────────────────────

func cmdTypes() error {
	fmt.Println(theme.Banner.Render("taildrives — categories"))
	fmt.Println()
	maxName := 0
	for _, c := range cats.All {
		if l := len(c.Name); l > maxName {
			maxName = l
		}
	}
	cur := cfg.Load().Category
	for _, c := range cats.All {
		marker := "  "
		if c.Name == cur {
			marker = theme.AccentHiS.Render("● ")
		}
		fmt.Printf("%s%s  %s\n", marker,
			theme.Title.Render(padRight(c.Name, maxName)),
			theme.ItemDim.Render(c.Description))
	}
	fmt.Println()
	fmt.Println(theme.ItemDim.Render("Active category: ") + theme.Info.Render(cur))
	fmt.Println(theme.ItemDim.Render("Change with: ") + theme.Info.Render("taildrives set --type <name>"))
	return nil
}

// ── set ────────────────────────────────────────────────────────────────────

func cmdSet(args []string) error {
	fs := flag.NewFlagSet("set", flag.ContinueOnError)
	t := fs.String("type", "", "category name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *t == "" {
		return errors.New("usage: taildrives set --type <category>  (try `taildrives types`)")
	}
	if _, ok := cats.ByName(*t); !ok {
		return fmt.Errorf("unknown category %q — try `taildrives types`", *t)
	}
	c := cfg.Load()
	c.Category = *t
	if err := cfg.Save(c); err != nil {
		return err
	}
	fmt.Println(theme.OK.Render("✓"), "default category set to", theme.Info.Render(*t))
	return nil
}

// ── ls / cat / get / put / copy / bulk-send / mount / devices ──────────────

func cmdLs(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: taildrives ls <PATH>")
	}
	dav := client()
	entries, err := dav.List(args[0])
	if err != nil {
		return err
	}
	for _, e := range entries {
		typ := "f"
		if e.IsDir {
			typ = "d"
		}
		size := "-"
		if !e.IsDir {
			size = humanBytes(e.Size)
		}
		styled := theme.File.Render(e.Name)
		if e.IsDir {
			styled = theme.Dir.Render(e.Name + "/")
		}
		fmt.Printf("%s  %8s  %s\n", typ, size, styled)
	}
	return nil
}

func cmdCat(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: taildrives cat <PATH>")
	}
	dav := client()
	b, err := dav.Get(args[0])
	if err != nil {
		return err
	}
	_, err = io.Copy(os.Stdout, strings.NewReader(string(b)))
	return err
}

func cmdGet(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: taildrives get <SRC> [DST]")
	}
	src := args[0]
	var dst string
	if len(args) == 2 {
		dst = args[1]
	} else {
		home, _ := os.UserHomeDir()
		dst = filepath.Join(home, "Downloads", path.Base(src))
	}
	dav := client()
	c := cfg.Load()
	mgr := xfer.New(dav, c.Concurrency)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()
	j := mgr.Submit(src, "file://"+dst)
	return waitOne(mgr, j.ID, fmt.Sprintf("Downloading → %s", dst))
}

func cmdPut(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: taildrives put <LOCAL_SRC> <REMOTE_DST>")
	}
	src := args[0]
	dst := args[1]
	if _, err := os.Stat(src); err != nil {
		return err
	}
	dav := client()
	c := cfg.Load()
	mgr := xfer.New(dav, c.Concurrency)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()
	j := mgr.Submit("file://"+src, dst)
	return waitOne(mgr, j.ID, fmt.Sprintf("Uploading → %s", dst))
}

func cmdCopy(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: taildrives copy <SRC> <DST>  (both WebDAV paths)")
	}
	dav := client()
	c := cfg.Load()
	mgr := xfer.New(dav, c.Concurrency)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()
	j := mgr.Submit(args[0], args[1])
	return waitOne(mgr, j.ID, fmt.Sprintf("Copying %s → %s", args[0], args[1]))
}

func cmdBulkSend(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: taildrives bulk-send <SRC> <DEVICE> [DEVICE ...]")
	}
	src := args[0]
	devs := args[1:]
	dav := client()
	c := cfg.Load()
	ns, err := resolveNamespace(dav)
	if err != nil {
		return err
	}
	mgr := xfer.New(dav, c.Concurrency)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr.Start(ctx)
	defer mgr.Stop()

	// Detect source kind: local file exists on disk → file://, else assume remote
	// WebDAV path on the tailnet.
	srcRef := src
	if _, err := os.Stat(src); err == nil {
		srcRef = "file://" + mustAbs(src)
	}
	name := path.Base(strings.TrimPrefix(srcRef, "file://"))

	fmt.Printf("%s fanning %s → %d devices\n",
		theme.Banner.Render("BULK-SEND"), theme.Info.Render(name), len(devs))

	var jobIDs []int64
	for _, d := range devs {
		taildrop, err := findTaildropShare(dav, ns, d)
		if err != nil {
			fmt.Printf("  %s %s — %v\n", theme.Err.Render("✗"), d, err)
			continue
		}
		dst := fmt.Sprintf("/%s/%s/%s/%s", ns, d, taildrop, name)
		j := mgr.Submit(srcRef, dst)
		jobIDs = append(jobIDs, j.ID)
		fmt.Printf("  queued #%d → %s\n", j.ID, theme.AccentHiS.Render(dst))
	}
	if len(jobIDs) == 0 {
		return errors.New("no destinations were reachable")
	}
	return waitAll(mgr, jobIDs)
}

// findTaildropShare queries a device's published shares and returns the first
// one that looks like a taildrop (matches *taildrop* / *taildrops*).
func findTaildropShare(dav *webdav.Client, ns, device string) (string, error) {
	entries, err := dav.List("/" + ns + "/" + device)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		lc := strings.ToLower(e.Name)
		if strings.Contains(lc, "taildrop") {
			return e.Name, nil
		}
	}
	return "", fmt.Errorf("no taildrop share found on %s", device)
}

func cmdMount(args []string) error {
	dir := "/mnt/taildrive"
	if len(args) > 0 {
		dir = args[0]
	}
	switch runtime.GOOS {
	case "linux":
		fmt.Println(theme.Banner.Render("Linux mount (davfs2)"))
		fmt.Printf("  sudo mkdir -p %s\n", dir)
		fmt.Printf("  sudo mount -t davfs http://100.100.100.100:8080 %s\n", dir)
		fmt.Println()
		fmt.Println(theme.ItemDim.Render("Browse without mounting: http://100.100.100.100:8080 in any browser"))
	case "darwin":
		fmt.Println(theme.Banner.Render("macOS mount (Finder)"))
		fmt.Println("  Cmd+K → http://100.100.100.100:8080 → Connect as Guest")
		fmt.Println()
		fmt.Println(theme.ItemDim.Render("Or use Tailscale.app's built-in browser at the same URL."))
	case "windows":
		fmt.Println(theme.Banner.Render("Windows mount (Explorer)"))
		fmt.Println(`  Open Explorer → \\100.100.100.100@8080`)
	}
	// Try to test reachability
	if _, err := client().List("/"); err != nil {
		fmt.Println(theme.Err.Render("⚠ "), "Drive endpoint is unreachable:", err)
		fmt.Println(theme.ItemDim.Render("  Verify Tailscale is running and Drive is enabled in the ACL."))
	} else {
		fmt.Println(theme.OK.Render("✓ "), "Drive endpoint reachable")
	}
	return nil
}

// cmdSelfTest dumps everything the TUI's loadRoot would compute so we can
// debug "device shows empty" reports remotely.
func cmdSelfTest() error {
	dav := client()
	fmt.Println(theme.Banner.Render("taildrives selftest"))
	fmt.Println(theme.ItemDim.Render("runtime:    ") + runtime.GOOS + "/" + runtime.GOARCH)
	fmt.Println(theme.ItemDim.Render("hostname:   ") + local.HostName())
	fmt.Println(theme.ItemDim.Render("endpoint:   ") + cfg.Load().Endpoint)
	ns, err := resolveNamespace(dav)
	if err != nil {
		fmt.Println(theme.Err.Render("namespace: ") + err.Error())
	} else {
		fmt.Println(theme.ItemDim.Render("namespace:  ") + ns)
	}

	fmt.Println()
	fmt.Println(theme.Title.Render("local.ListShares() result"))
	shares, err := local.ListShares()
	if err != nil {
		fmt.Println(theme.Err.Render("  error: ") + err.Error())
	} else {
		fmt.Printf("  count = %d\n", len(shares))
		for i, s := range shares {
			fmt.Printf("  [%d] name=%q path=%q\n", i, s.Name, s.Path)
		}
	}

	fmt.Println()
	fmt.Println(theme.Title.Render("local.TailnetDevices() result"))
	peers, err := local.TailnetDevices()
	if err != nil {
		fmt.Println(theme.Err.Render("  error: ") + err.Error())
	} else {
		on, off := 0, 0
		for _, p := range peers {
			if p.Online {
				on++
			} else {
				off++
			}
		}
		fmt.Printf("  total=%d online=%d offline=%d\n", len(peers), on, off)
		for _, p := range peers {
			if !webdav.IsSidecarDevice(p.Hostname) {
				status := "online"
				if !p.Online {
					status = "offline " + p.LastSeen
				}
				fmt.Printf("    %-30s %s\n", p.Hostname, status)
			}
		}
	}

	fmt.Println()
	fmt.Println(theme.Title.Render("WebDAV root devices (after sidecar/mob/ent filter)"))
	rootEntries, err := dav.List("/" + ns)
	if err != nil {
		fmt.Println(theme.Err.Render("  error: ") + err.Error())
	} else {
		count := 0
		for _, e := range rootEntries {
			if !e.IsDir {
				continue
			}
			if webdav.IsSidecarDevice(e.Name) {
				continue
			}
			fmt.Printf("    %s\n", e.Name)
			count++
		}
		fmt.Printf("  total kept = %d\n", count)
	}
	return nil
}

func cmdDevices(args ...string) error {
	showAll := false
	for _, a := range args {
		if a == "--all" || a == "-a" {
			showAll = true
		}
	}
	dav := client()
	ns, err := resolveNamespace(dav)
	if err != nil {
		return err
	}
	devs, err := dav.List("/" + ns)
	if err != nil {
		return err
	}
	fmt.Println(theme.Banner.Render("taildrives — devices visible in tailnet"))
	var hosts, sidecars []string
	for _, d := range devs {
		if !d.IsDir {
			continue
		}
		if webdav.IsSidecarDevice(d.Name) {
			sidecars = append(sidecars, d.Name)
		} else {
			hosts = append(hosts, d.Name)
		}
	}
	for _, h := range hosts {
		fmt.Println("  " + theme.AccentHiS.Render(h))
	}
	if showAll && len(sidecars) > 0 {
		fmt.Println()
		fmt.Println(theme.ItemDim.Render(fmt.Sprintf("  ── container sidecars (%d) ──", len(sidecars))))
		for _, s := range sidecars {
			fmt.Println("  " + theme.ItemDim.Render(s))
		}
	} else if len(sidecars) > 0 {
		fmt.Println()
		fmt.Println(theme.ItemDim.Render(fmt.Sprintf("  + %d container sidecars hidden (run `taildrives devices --all` to show)", len(sidecars))))
	}
	return nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
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

// waitOne blocks until the job reaches a terminal state, printing a single
// updating progress line on stderr.
func waitOne(mgr *xfer.Manager, id int64, label string) error {
	fmt.Fprintln(os.Stderr, theme.Info.Render(label))
	last := ""
	for {
		s, ok := mgr.Get(id)
		if !ok {
			return fmt.Errorf("job %d vanished", id)
		}
		line := fmt.Sprintf("  [%s] %s  %s",
			s.State.String(), humanBytes(s.Written), abbrPath(s.Dst, 50))
		if line != last {
			fmt.Fprintln(os.Stderr, "  "+line)
			last = line
		}
		if s.State == xfer.Done {
			fmt.Fprintln(os.Stderr, theme.OK.Render("  ✓ done — ")+humanBytes(s.Written)+" in "+s.Elapsed.Round(time.Millisecond).String())
			return nil
		}
		if s.State == xfer.Failed {
			return fmt.Errorf("transfer failed: %s", s.Err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitAll(mgr *xfer.Manager, ids []int64) error {
	pending := map[int64]bool{}
	for _, id := range ids {
		pending[id] = true
	}
	var firstErr error
	for len(pending) > 0 {
		for id := range pending {
			s, ok := mgr.Get(id)
			if !ok {
				delete(pending, id)
				continue
			}
			if s.State == xfer.Done {
				fmt.Println(theme.OK.Render("  ✓ #")+fmt.Sprint(id), abbrPath(s.Dst, 60),
					theme.ItemDim.Render("  "+humanBytes(s.Written)))
				delete(pending, id)
			} else if s.State == xfer.Failed {
				fmt.Println(theme.Err.Render("  ✗ #")+fmt.Sprint(id), abbrPath(s.Dst, 60),
					theme.Err.Render("  "+s.Err))
				if firstErr == nil {
					firstErr = errors.New("one or more transfers failed")
				}
				delete(pending, id)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return firstErr
}

func abbrPath(p string, w int) string {
	p = strings.TrimPrefix(p, "file://")
	if len(p) <= w {
		return p
	}
	return ".../" + path.Base(p)
}

