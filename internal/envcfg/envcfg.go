// Package envcfg holds the operator-curated lists of Environment
// Configurations and GitLab Repositories rendered in the bottom two
// sections of the taildrives TUI left column.
//
// Both registries are loaded from disk so the operator can edit them
// without rebuilding the app:
//
//	~/.config/taildrives/env-configs.json
//	~/.config/taildrives/gitlab-repos.json
//
// If a file is missing, a sensible default is seeded on first read so
// the user has examples to edit. Entries are simple — each is a label
// plus a path (WebDAV /ns/dev/share/sub or local:// or git URL).
package envcfg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Entry is a single curated item — env config or gitlab repo.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Path is a WebDAV path (/ns/dev/share/sub), a local:// share path,
	// or a git/HTTPS URL for the GitLab case.
	Path string `json:"path"`
	// Kind: "share" (drillable in the file pane) or "url" (opens in
	// browser when user presses Enter).
	Kind string `json:"kind,omitempty"`
}

// configDir mirrors cfg.Dir() but is repeated here to avoid an import cycle.
func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "taildrives")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "taildrives")
}

// LoadEnvConfigs reads ~/.config/taildrives/env-configs.json, seeding it
// with defaults on first read.
func LoadEnvConfigs() []Entry {
	return loadOrSeed("env-configs.json", defaultEnvConfigs())
}

// LoadGitLabRepos reads ~/.config/taildrives/gitlab-repos.json, seeding
// with the canonical n3m3sis-infrastructure repo on first read.
func LoadGitLabRepos() []Entry {
	return loadOrSeed("gitlab-repos.json", defaultGitLabRepos())
}

func loadOrSeed(filename string, seed []Entry) []Entry {
	path := filepath.Join(configDir(), filename)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// First-run seed
		_ = os.MkdirAll(configDir(), 0o755)
		buf, _ := json.MarshalIndent(seed, "", "  ")
		_ = os.WriteFile(path, buf, 0o644)
		return seed
	}
	if err != nil {
		return seed
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return seed
	}
	return entries
}

// defaultEnvConfigs is the initial seed for env-configs.json. Operator
// edits the file to add/remove/relocate entries — these are placeholders
// describing what kind of thing belongs here.
func defaultEnvConfigs() []Entry {
	return []Entry{
		{
			Name:        "Obsidian Vault Config",
			Description: "Shared Obsidian config + plugins + workspace settings — sync this across every workstation so the vault behaves identically.",
			Path:        "(unset — designate via env-configs.json)",
			Kind:        "share",
		},
		{
			Name:        "Devnet Shell Profiles",
			Description: "Curated .zshrc / .bashrc fragments + aliases for the n3m3sis devnet hosts.",
			Path:        "(unset — designate via env-configs.json)",
			Kind:        "share",
		},
	}
}

// defaultGitLabRepos seeds with what we know about — the in-house GitLab
// instance at git.archn3m3sis.com.
func defaultGitLabRepos() []Entry {
	return []Entry{
		{
			Name:        "n3m3sis-infrastructure",
			Description: "Live infrastructure repo — every NixOS module, container compose file, ACL, dashboard. The taildrives source ships here too.",
			Path:        "/archn3m3sis.bounties@gmail.com/n3m-srv-02/n3m_srv_02_services_config",
			Kind:        "share",
		},
	}
}
