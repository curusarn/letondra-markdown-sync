// Package config loads and saves the app's settings file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Mapping pairs one local folder with one Google Drive folder.
type Mapping struct {
	Local       string   `json:"local"`        // e.g. C:\Users\simon\Vault\Škola
	DriveFolder string   `json:"drive_folder"` // folder ID or its Drive URL
	Exclude     []string `json:"exclude,omitempty"`
}

// Config is the settings file (config.json in the app folder).
type Config struct {
	ServiceAccountKey string    `json:"service_account_key"` // path to the JSON key
	OAuthClient       string    `json:"oauth_client"`        // path to the "Desktop app" client JSON
	Launch            string    `json:"launch"`              // program or URL started with the app
	PollSeconds       int       `json:"poll_seconds"`        // how often to ask Drive for changes
	Mappings          []Mapping `json:"mappings"`
}

// DefaultLaunch opens Obsidian through its URL handler, which works wherever
// Obsidian is installed.
const DefaultLaunch = "obsidian://open"

// Dir is the app folder: %APPDATA%\LetondraMarkdownSync on Windows. The
// LMS_HOME environment variable overrides it (tests, a second profile).
func Dir() (string, error) {
	if d := os.Getenv("LMS_HOME"); d != "" {
		return d, os.MkdirAll(d, 0o700)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "LetondraMarkdownSync")
	return d, os.MkdirAll(d, 0o700)
}

// Path is the config file path.
func Path(dir string) string { return filepath.Join(dir, "config.json") }

// Load reads the config. A missing file gives the defaults and created=true.
func Load(dir string) (cfg *Config, created bool, err error) {
	cfg = &Config{Launch: DefaultLaunch, PollSeconds: 30}
	data, err := os.ReadFile(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return cfg, true, Save(dir, cfg)
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("config.json is not valid JSON: %w", err)
	}
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = 30
	}
	return cfg, false, nil
}

// Save writes the config atomically.
func Save(dir string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := Path(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(dir))
}

var folderURLRe = regexp.MustCompile(`/folders/([A-Za-z0-9_-]+)`)
var folderIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,}$`)

// FolderID extracts the folder ID from an ID or a Drive folder URL.
func FolderID(s string) (string, error) {
	s = strings.TrimSpace(s)
	if m := folderURLRe.FindStringSubmatch(s); m != nil {
		return m[1], nil
	}
	if folderIDRe.MatchString(s) {
		return s, nil
	}
	return "", fmt.Errorf("%q is not a Google Drive folder link or ID", s)
}

// Validate reports problems that stop syncing.
func (c *Config) Validate() []string {
	var probs []string
	if c.ServiceAccountKey == "" {
		probs = append(probs, "Chybí cesta k JSON klíči service accountu.")
	} else if _, err := os.Stat(c.ServiceAccountKey); err != nil {
		probs = append(probs, "JSON klíč service accountu nejde přečíst: "+c.ServiceAccountKey)
	}
	if len(c.Mappings) == 0 {
		probs = append(probs, "Není nastavená žádná dvojice složek.")
	}
	seen := map[string]bool{}
	for i, m := range c.Mappings {
		id, err := FolderID(m.DriveFolder)
		if err != nil {
			probs = append(probs, fmt.Sprintf("Složka %d: %v", i+1, err))
		} else if seen[id] {
			probs = append(probs, fmt.Sprintf("Složka %d: stejná Drive složka je nastavená dvakrát.", i+1))
		}
		seen[id] = true
		if m.Local == "" || !filepath.IsAbs(m.Local) {
			probs = append(probs, fmt.Sprintf("Složka %d: lokální cesta musí být celá (např. C:\\Users\\...).", i+1))
		}
	}
	return probs
}
