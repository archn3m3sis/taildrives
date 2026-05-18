// Package desc maps share names to thorough human descriptions for the
// n3m3sis devnet. Lookup is exact-match first, then convention-pattern
// fallback so newly-added shares of a known type get a sensible default.
package desc

import (
	"regexp"
	"strings"
)

// Describe returns a thorough multi-sentence description of a share. Always
// returns a non-empty string — falls back to a generic description if no
// pattern matches.
func Describe(shareName string) string {
	if d, ok := exact[shareName]; ok {
		return d
	}
	for _, p := range patterns {
		if p.re.MatchString(shareName) {
			return p.desc
		}
	}
	return "Custom share — no description registered. Add one in " +
		"internal/desc/desc.go so other operators understand what's stored here."
}

// exact-match descriptions for canonical n3m3sis shares.
var exact = map[string]string{
	// Per-device taildrop folders
	"n3m_srv_01_taildrops":  defaultTaildrop("srv-01 (UGREEN NAS, UGOS)") + " Backed by /volume1 on the UGREEN array — has the most physical storage in the mesh.",
	"n3m_srv_02_taildrops":  defaultTaildrop("srv-02 (NixOS, main services host)") + " Backed by /mnt/storage3, the new 2TB SSD overflow volume — keeps drops off the OS disk.",
	"n3m_srv_03_taildrops":  defaultTaildrop("srv-03 (ASUS Vivobook, NixOS)") + " Backed by the home directory — fine for small/medium drops, watch capacity on the laptop SSD.",
	"n3m_srv_04_taildrops":  defaultTaildrop("srv-04 (Windows Server 2025, Dell rackmount)") + " Backed by D:\\ on the 2TB Windows volume — useful for Windows-specific artifacts.",
	"n3m_srv_05_taildrops":  defaultTaildrop("srv-05 (Windows Server 2025, Dell PowerEdge R420)") + " Backed by D:\\ but the volume is small (~106GB) — used sparingly.",
	"n3m_wks_01_taildrops":  defaultTaildrop("wks-01 (macOS workstation, Apple Silicon)") + " Backed by ~/n3m_wks_01_taildrops in your home directory.",
	"n3m_wks_02_taildrops":  defaultTaildrop("wks-02 (macOS workstation, Neo's Mac)") + " Backed by Neo's home directory — useful for transferring to/from the secondary workstation.",

	// Libraries (NAS-mounted, source-of-truth)
	"n3m_library_media": "The canonical media library for the entire n3m3sis devnet. NFS-mounted on srv-02 from srv-01's /volume1/media (7.4TB). Holds movies, TV, music, audio — anything Plex, Jellyfin, or sonarr/radarr touches. Read-write across the tailnet, but be aware that this is the SOURCE OF TRUTH — accidental deletes here are very hard to undo.",
	"n3m_library_photos": "The canonical photo library for the n3m3sis devnet. NFS-mounted on srv-02 from srv-01's /volume2/photos (2.7TB). Holds the Immich originals + thumbnail cache. Treat as read-mostly — Immich is the primary writer; manual edits here can race with Immich's database state.",

	// srv-01 storage
	"n3m_srv_01_volume1_media":  "Direct passthrough of srv-01's /volume1/media — same data as n3m_library_media, but accessed without going through the NFS-on-srv-02 hop. Faster for srv-01-local operations and avoids hitting the NFS mount when the network path is shaky.",
	"n3m_srv_01_volume2_photos": "Direct passthrough of srv-01's /volume2/photos — same data as n3m_library_photos. Same fast-local-access rationale.",
	"n3m_srv_01_usb_sdc1":       "External 3.6TB USB drive attached to srv-01. General-purpose overflow; commonly used for media-import staging, ad-hoc backups, and Decypharr/debrid scratch.",
	"n3m_srv_01_usb_sdd1":       "External 7.3TB USB drive attached to srv-01. The big one — used for long-term archival, backup landing zone, and anything that would crowd /volume1.",

	// srv-02 storage
	"n3m_srv_02_storage3":         "The new 2TB ext4 SSD on srv-02 (mounted at /mnt/storage3 as of 2026-05-17). Overflow + Taildrive hub. Subfolders include n3m_srv_02_taildrops, drop-zone, share-staging, scratch, and backup-staging. Use this when you'd otherwise pollute storage1 (docs/IAMS) or storage2 (services/iso).",
	"n3m_srv_02_services_config":  "The /mnt/storage2/services directory on srv-02 — the live config repository for every service in the devnet (Traefik, Grafana, Prometheus, NixOS-managed containers, etc.). Maps 1:1 with the n3m3sis-infrastructure GitLab repo. Edits here flow straight into running services after a rebuild.",
	"n3m_srv_02_iso":              "ISO boot images for the iVentoy PXE boot service. Drop ISOs here and they become bootable on any device on the LAN via iVentoy's menu.",

	// srv-03 storage
	"n3m_srv_03_external": "External SSD attached to srv-03 (the Vivobook). General-purpose; commonly used for portable backups and laptop-local scratch.",
	"n3m_srv_03_home":     "The home directory on srv-03 (~archn3m3sis). Useful for accessing srv-03-local tooling, dotfiles, and laptop-only artifacts.",

	// Windows server volumes
	"n3m_srv_04_dvol": "The entire D:\\ volume on srv-04 (2TB). Broad access — careful here, you can see and modify anything on the volume.",
	"n3m_srv_05_dvol": "The entire D:\\ volume on srv-05. Smaller volume (~106GB), mostly Windows install media and tooling. Limited use.",

	// macOS workstation special
	"unified_memory_vault": "The Unified Memory Vault — your Obsidian knowledge base. Lives at ~/Notes/unified_memory_vault on wks-01. Synced bidirectionally between wks-01 and wks-02 via Syncthing (Taildrive here is read access for other devices). Editing the same note simultaneously from multiple devices can cause Obsidian sync conflicts — prefer the local Obsidian app on wks-01/wks-02 for writes.",
}

// patterns: convention-based fallbacks (matched in order).
type pattern struct {
	re   *regexp.Regexp
	desc string
}

var patterns = []pattern{
	{
		re:   regexp.MustCompile(`(?i)_taildrops?$|^taildrops?$`),
		desc: defaultTaildrop("this device"),
	},
	{
		re:   regexp.MustCompile(`(?i)^n3m_library_`),
		desc: "A canonical n3m3sis library share. Single source of truth — only one device hosts it; everyone else accesses via the tailnet.",
	},
	{
		re:   regexp.MustCompile(`(?i)usb_`),
		desc: "External USB-attached storage. General overflow / archival capacity.",
	},
	{
		re:   regexp.MustCompile(`(?i)_dvol$|^.*_[a-z]vol$`),
		desc: "Full-volume share — broad access to an entire mounted disk. Useful for ops/troubleshooting; be careful with destructive operations.",
	},
	{
		re:   regexp.MustCompile(`(?i)home$`),
		desc: "Home-directory share. Holds dotfiles, project checkouts, and personal scratch files for the host's primary user.",
	},
	{
		re:   regexp.MustCompile(`(?i)external|volume\d`),
		desc: "Auxiliary storage volume on this host. General-purpose; use case is host-specific.",
	},
}

// Short returns a one-line summary (~80 chars) for use in compact views.
// Falls back to the full description truncated.
func Short(shareName string) string {
	full := Describe(shareName)
	// First sentence (up to first ". ") is usually the gist.
	if i := strings.Index(full, ". "); i > 0 && i < 140 {
		return full[:i+1]
	}
	if len(full) > 140 {
		return full[:139] + "…"
	}
	return full
}

// defaultTaildrop is the canonical wording for per-device drop zones.
func defaultTaildrop(host string) string {
	return "Per-device drop zone on " + host + ". One of the conventional " +
		"defaults found on every Taildrive source device in the n3m3sis " +
		"devnet — use it for quick device-to-device transfers of active " +
		"workflow data, scratch notes, screenshots, build artifacts, or " +
		"really just about anything you'd otherwise have to email yourself."
}
