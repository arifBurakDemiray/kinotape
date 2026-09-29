package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// Source is one place videos come from: a local folder or an SMB share.
type Source struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Host   string `json:"host,omitempty"`
	Share  string `json:"share,omitempty"`
	Dir    string `json:"dir,omitempty"`
	User   string `json:"user,omitempty"`
	Domain string `json:"domain,omitempty"`
}

// Config holds the user's settings.
type Config struct {
	Sources  []Source          `json:"sources"`
	Autoplay bool              `json:"autoplay"`
	Artwork  bool              `json:"artwork"`
	TMDBKey  string            `json:"tmdb_key,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
	Folders  []string          `json:"folders,omitempty"`
	Queue    *bool             `json:"queue,omitempty"`
}

// Progress is how far an episode got in the player.
type Progress struct {
	Position float64 `json:"position"`
	At       float64 `json:"at"`
}

// State is what Kinotape remembers about watching.
type State struct {
	Marks    map[string]bool     `json:"marks"`
	Progress map[string]Progress `json:"progress"`
}

// Store loads and saves settings and watch state as JSON files in the config folder.
type Store struct {
	mu     sync.Mutex
	dir    string
	config Config
	state  State
}

// openStore reads the settings and watch state, filling in defaults and older formats.
func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: Config{Autoplay: true, Artwork: true}}
	if err := readJSON(filepath.Join(dir, "config.json"), &s.config); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := readJSON(filepath.Join(dir, "state.json"), &s.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	migrated := false
	if len(s.config.Sources) == 0 && len(s.config.Folders) > 0 {
		for _, folder := range s.config.Folders {
			s.config.Sources = append(s.config.Sources, Source{ID: "local-" + md5Hex(folder)[:8], Kind: "local", Path: folder})
		}
		migrated = true
	}
	if s.config.Queue != nil {
		s.config.Autoplay = *s.config.Queue
		migrated = true
	}
	s.config.Folders, s.config.Queue = nil, nil
	if s.config.Secrets == nil {
		s.config.Secrets = map[string]string{}
	}
	if s.state.Marks == nil {
		s.state.Marks = map[string]bool{}
	}
	if s.state.Progress == nil {
		s.state.Progress = map[string]Progress{}
	}
	if migrated {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.saveConfigLocked(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Config returns a copy of the current settings.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.config
	c.Sources = append([]Source(nil), s.config.Sources...)
	return c
}

// UpdateConfig changes the settings under the lock and saves them.
func (s *Store) UpdateConfig(change func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.config)
	return s.saveConfigLocked()
}

// State returns a copy of the watch state.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := State{Marks: make(map[string]bool, len(s.state.Marks)), Progress: make(map[string]Progress, len(s.state.Progress))}
	for k, v := range s.state.Marks {
		st.Marks[k] = v
	}
	for k, v := range s.state.Progress {
		st.Progress[k] = v
	}
	return st
}

// SetProgress remembers how far an episode got.
func (s *Store) SetProgress(id string, position float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Progress[id] = Progress{Position: position, At: float64(time.Now().UnixNano()) / 1e9}
	return writeJSON(filepath.Join(s.dir, "state.json"), s.state)
}

// SetMark stores a manual watched mark, or clears it when watched is nil.
func (s *Store) SetMark(id string, watched *bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if watched == nil {
		delete(s.state.Marks, id)
	} else {
		s.state.Marks[id] = *watched
	}
	return writeJSON(filepath.Join(s.dir, "state.json"), s.state)
}

// SetSecret keeps a share password in the OS keychain, or in the config when there is none.
func (s *Store) SetSecret(id, secret string) error {
	if secret == "" {
		return nil
	}
	if err := keyring.Set(appName, id, secret); err == nil {
		return nil
	}
	return s.UpdateConfig(func(c *Config) { c.Secrets[id] = secret })
}

// Secret returns a share password saved with SetSecret.
func (s *Store) Secret(id string) string {
	if secret, err := keyring.Get(appName, id); err == nil {
		return secret
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config.Secrets[id]
}

// DeleteSecret forgets a share password.
func (s *Store) DeleteSecret(id string) {
	_ = keyring.Delete(appName, id)
	_ = s.UpdateConfig(func(c *Config) { delete(c.Secrets, id) })
}

// saveConfigLocked writes the settings; the caller holds the lock.
func (s *Store) saveConfigLocked() error {
	return writeJSON(filepath.Join(s.dir, "config.json"), s.config)
}

// readJSON decodes a JSON file into value.
func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

// writeJSON writes a JSON file atomically so an interrupted write never leaves a broken file.
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
