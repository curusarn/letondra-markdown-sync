// Package state keeps what the app knows about every synced file between
// runs: which Drive document is which local file and the last synced
// version of both sides (the "base" of the 3-way merge).
//
// It is one JSON file written atomically after every change, plus a
// content-addressed folder of text blobs, so a crash or a killed process
// never leaves a half-written state behind.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// File is one synced document.
type File struct {
	DriveID  string `json:"drive_id"`
	Mapping  string `json:"mapping"` // Drive folder ID of the mapping root
	RelPath  string `json:"rel_path"`
	Name     string `json:"name"`   // Drive name at last sync
	Parent   string `json:"parent"` // Drive parent folder at last sync
	MimeType string `json:"mime_type"`
	Version  int64  `json:"version"` // Drive version at last sync

	BaseLocal  string `json:"base_local"`  // blob: local text at last sync
	BaseExport string `json:"base_export"` // blob: cleaned Drive export at last sync

	LocalSize  int64 `json:"local_size"`
	LocalMtime int64 `json:"local_mtime"`

	PendingTrash bool      `json:"pending_trash,omitempty"` // deleted locally, Drive bin still to do
	Conflict     *Conflict `json:"conflict,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// Conflict is a merge that needs Simon. Blobs hold the three versions.
type Conflict struct {
	Base          string    `json:"base"`
	Local         string    `json:"local"`
	Remote        string    `json:"remote"`         // Drive version in local form
	RemoteExport  string    `json:"remote_export"`  // the cleaned export it came from
	RemoteVersion int64     `json:"remote_version"` // Drive version when detected
	Detected      time.Time `json:"detected"`
}

// Folder is one Drive folder inside a mapping.
type Folder struct {
	ID      string `json:"id"`
	Mapping string `json:"mapping"`
	RelPath string `json:"rel_path"` // "" for the mapping root
	Name    string `json:"name"`
	Parent  string `json:"parent"`
}

// State is everything persisted.
type State struct {
	PageToken string             `json:"page_token"`
	Files     map[string]*File   `json:"files"`
	Folders   map[string]*Folder `json:"folders"`
	// MassDeleteOK lets a mapping go ahead with many local deletions once
	// Simon confirmed them.
	MassDeleteOK map[string]bool `json:"mass_delete_ok,omitempty"`
}

// Store reads and writes the state in dir.
type Store struct {
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path() string { return filepath.Join(s.dir, "state.json") }

// Load reads the state; a missing file gives an empty state.
func (s *Store) Load() (*State, error) {
	st := &State{}
	data, err := os.ReadFile(s.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, st); err != nil {
			// Keep the broken file for inspection and start over: a full
			// scan rebuilds the pairing from names and content.
			_ = os.Rename(s.path(), s.path()+".broken-"+time.Now().Format("20060102-150405"))
			st = &State{}
		}
	}
	if st.Files == nil {
		st.Files = map[string]*File{}
	}
	if st.Folders == nil {
		st.Folders = map[string]*Folder{}
	}
	if st.MassDeleteOK == nil {
		st.MassDeleteOK = map[string]bool{}
	}
	return st, nil
}

// Save writes the state atomically (temp file, fsync, rename).
func (s *Store) Save(st *State) error {
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path(), data)
}

// Put stores a text blob and returns its key.
func (s *Store) Put(text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	key := hex.EncodeToString(sum[:])
	p := filepath.Join(s.dir, "blobs", key)
	if _, err := os.Stat(p); err == nil {
		return key, nil
	}
	return key, writeAtomic(p, []byte(text))
}

// Get returns a blob ("" for the empty key).
func (s *Store) Get(key string) (string, error) {
	if key == "" {
		return "", nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "blobs", key))
	return string(data), err
}

// Hash is the blob key of a text, without storing it.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// GC removes blobs no file references.
func (s *Store) GC(st *State) {
	keep := map[string]bool{}
	for _, f := range st.Files {
		keep[f.BaseLocal], keep[f.BaseExport] = true, true
		if c := f.Conflict; c != nil {
			keep[c.Base], keep[c.Local], keep[c.Remote], keep[c.RemoteExport] = true, true, true, true
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "blobs"))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.Remove(filepath.Join(s.dir, "blobs", e.Name()))
		}
	}
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
