// Package cats defines the share category taxonomy for the n3m3sis devnet.
// Categories are matched by regex against device name + share name.
package cats

import (
	"regexp"
	"sort"
)

// Category is a named filter over (device, share) pairs.
type Category struct {
	Name        string
	Description string
	DeviceRE    *regexp.Regexp // nil = match any device
	ShareRE     *regexp.Regexp // nil = match any share
}

// Matches reports whether (device, share) belongs to this category.
func (c Category) Matches(device, share string) bool {
	if c.DeviceRE != nil && !c.DeviceRE.MatchString(device) {
		return false
	}
	if c.ShareRE != nil && !c.ShareRE.MatchString(share) {
		return false
	}
	return true
}

// All is the canonical, ordered list of CONTENT categories.
// Device-type categories (servers / workstations / mobile) live in the Share
// Sources pane instead — Content Categories is strictly about content kind.
var All = []Category{
	{
		Name: "No Active Content Filtering", Description: "Default — every share, no filter applied",
	},
	{
		Name: "taildrops", Description: "Per-device drop zones — fire-and-forget transfers",
		ShareRE: regexp.MustCompile(`(?i)_taildrops?$|^taildrops?`),
	},
	{
		Name: "libraries", Description: "Shared libraries — media, photos, source-of-truth datasets",
		ShareRE: regexp.MustCompile(`(?i)^n3m_library_|library`),
	},
	{
		Name: "media", Description: "Movies, TV, audio libraries",
		ShareRE: regexp.MustCompile(`(?i)media|movies|tv|music|audio`),
	},
	{
		Name: "photos", Description: "Photo libraries (Immich, originals)",
		ShareRE: regexp.MustCompile(`(?i)photo|immich`),
	},
	{
		Name: "storage", Description: "Full-volume mounts — raw disk access",
		ShareRE: regexp.MustCompile(`(?i)storage\d|^.*_dvol$|usb_|external|volume\d`),
	},
	{
		Name: "personal", Description: "Personal home directories, vaults, journals",
		ShareRE: regexp.MustCompile(`(?i)home|vault|journal|personal|memory`),
	},
	{
		Name: "sys-services", Description: "System-level service configs (NixOS, Docker, infra)",
		ShareRE: regexp.MustCompile(`(?i)services_config|nixos|docker|config|iso`),
	},
	{
		Name: "user-services", Description: "User-facing app data (Immich, Plex, GitLab, etc.)",
		ShareRE: regexp.MustCompile(`(?i)plex|immich|gitlab|jellyfin|sonarr|radarr|appdata`),
	},
}

// ByName returns the category with the given name, or (zero, false).
func ByName(name string) (Category, bool) {
	for _, c := range All {
		if c.Name == name {
			return c, true
		}
	}
	return Category{}, false
}

// Names returns category names in display order.
func Names() []string {
	out := make([]string, len(All))
	for i, c := range All {
		out[i] = c.Name
	}
	return out
}

// Classify returns every category that matches (device, share).
// "all" is always included.
func Classify(device, share string) []string {
	var out []string
	for _, c := range All {
		if c.Matches(device, share) {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}
