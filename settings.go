package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// StumpSettings is everything needed to talk to a Stump server: where it is
// and how to authenticate. It's edited on the Settings page and persisted by
// a SettingsStore.
type StumpSettings struct {
	URL       string `json:"url"`
	APIKey    string `json:"apiKey"`
	Username  string `json:"username"`
	Password  string `json:"password"`
	LibraryID string `json:"libraryId"`
}

// Configured reports whether there's enough here to talk to the Stump API at
// all. Downloading still works without it; only the rescan trigger is lost.
func (s StumpSettings) Configured() bool {
	return s.URL != "" && (s.APIKey != "" || (s.Username != "" && s.Password != ""))
}

// ZLibrarySettings is the Z-library account and search preferences. Email and
// password are the user's Z-library login; userId/userKey are the session
// cookies returned after a successful login (remix_userid / remix_userkey).
type ZLibrarySettings struct {
	BaseURL    string           `json:"baseUrl"`
	Email      string           `json:"email"`
	Password   string           `json:"password"`
	UserID     string           `json:"userId"`
	UserKey    string           `json:"userKey"`
	Languages  []string         `json:"languages,omitempty"`
	Extensions []string         `json:"extensions,omitempty"`
	Order      string           `json:"order,omitempty"`
	Blocked    map[string]int64 `json:"blockedMirrors,omitempty"`
}

func (z ZLibrarySettings) HasCredentials() bool {
	return z.Email != "" && z.Password != ""
}

func (z ZLibrarySettings) HasSession() bool {
	return z.UserID != "" && z.UserKey != ""
}

// persistedSettings is the on-disk shape. Stump fields stay at the top level
// so existing .stumpzlib-settings.json files keep loading; zlibrary is new.
type persistedSettings struct {
	URL       string            `json:"url"`
	APIKey    string            `json:"apiKey"`
	Username  string            `json:"username"`
	Password  string            `json:"password"`
	LibraryID string            `json:"libraryId"`
	ZLibrary  *ZLibrarySettings `json:"zlibrary"`
}

// SettingsStore holds live settings and persists them to a JSON file so they
// survive a restart. With an empty path, it holds settings in memory only.
type SettingsStore struct {
	mu       sync.RWMutex
	path     string
	stump    StumpSettings
	zlibrary ZLibrarySettings
}

// NewSettingsStore starts from `initial` (normally env vars / flags), then
// loads the persisted file over it if one exists — a prior save from the
// Settings page always wins over the process's current environment.
//
// initialZ is used only when the file is missing, or is an older file with
// no zlibrary key yet. Once the file contains that key, the saved value
// wins even if it is empty.
func NewSettingsStore(path string, initial StumpSettings, initialZ ZLibrarySettings) (*SettingsStore, error) {
	s := &SettingsStore{path: path, stump: initial, zlibrary: initialZ}

	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var saved persistedSettings
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	s.stump = StumpSettings{
		URL:       saved.URL,
		APIKey:    saved.APIKey,
		Username:  saved.Username,
		Password:  saved.Password,
		LibraryID: saved.LibraryID,
	}
	if saved.ZLibrary != nil {
		s.zlibrary = *saved.ZLibrary
	}
	return s, nil
}

func (s *SettingsStore) Get() StumpSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stump
}

func (s *SettingsStore) GetZLibrary() ZLibrarySettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneZLibrary(s.zlibrary)
}

// Set replaces the Stump settings wholesale and persists them before applying
// them in memory, so a failed write never leaves the store out of sync with disk.
func (s *SettingsStore) Set(v StumpSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(v, s.zlibrary); err != nil {
		return err
	}
	s.stump = v
	return nil
}

// SetZLibrary replaces the Z-library settings and persists the whole file.
func (s *SettingsStore) SetZLibrary(v ZLibrarySettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(s.stump, v); err != nil {
		return err
	}
	s.zlibrary = v
	return nil
}

func (s *SettingsStore) persistLocked(stump StumpSettings, z ZLibrarySettings) error {
	if s.path == "" {
		return nil
	}
	zCopy := cloneZLibrary(z)
	data, err := json.MarshalIndent(persistedSettings{
		URL:       stump.URL,
		APIKey:    stump.APIKey,
		Username:  stump.Username,
		Password:  stump.Password,
		LibraryID: stump.LibraryID,
		ZLibrary:  &zCopy,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("saving %s: %w", s.path, err)
	}
	return nil
}

func cloneZLibrary(z ZLibrarySettings) ZLibrarySettings {
	out := z
	if z.Languages != nil {
		out.Languages = append([]string(nil), z.Languages...)
	}
	if z.Extensions != nil {
		out.Extensions = append([]string(nil), z.Extensions...)
	}
	if z.Blocked != nil {
		out.Blocked = make(map[string]int64, len(z.Blocked))
		for k, v := range z.Blocked {
			out.Blocked[k] = v
		}
	}
	return out
}

// --- HTTP handler -----------------------------------------------------------

// settingsResponse never carries secret values back to the browser — only
// whether one is currently set. The Settings page leaves those fields blank
// and only overwrites a secret when the user types a new one (or explicitly
// asks to clear it).
type settingsResponse struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	LibraryID   string `json:"libraryId"`
	HasAPIKey   bool   `json:"hasApiKey"`
	HasPassword bool   `json:"hasPassword"`
	Configured  bool   `json:"configured"`
}

func toSettingsResponse(v StumpSettings) settingsResponse {
	return settingsResponse{
		URL:         v.URL,
		Username:    v.Username,
		LibraryID:   v.LibraryID,
		HasAPIKey:   v.APIKey != "",
		HasPassword: v.Password != "",
		Configured:  v.Configured(),
	}
}

type settingsUpdateRequest struct {
	URL           string `json:"url"`
	APIKey        string `json:"apiKey"`
	ClearAPIKey   bool   `json:"clearApiKey"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	ClearPassword bool   `json:"clearPassword"`
	LibraryID     string `json:"libraryId"`
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, toSettingsResponse(s.settings.Get()))

	case http.MethodPost:
		var req settingsUpdateRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "could not parse the request body")
			return
		}

		next := s.settings.Get()
		next.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
		next.Username = strings.TrimSpace(req.Username)
		next.LibraryID = strings.TrimSpace(req.LibraryID)

		switch {
		case req.ClearAPIKey:
			next.APIKey = ""
		case req.APIKey != "":
			next.APIKey = req.APIKey
		}
		switch {
		case req.ClearPassword:
			next.Password = ""
		case req.Password != "":
			next.Password = req.Password
		}

		if next.URL != "" {
			u, err := url.Parse(next.URL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				writeError(w, http.StatusBadRequest, "Stump URL must be a full http(s) URL, e.g. http://stump:10801")
				return
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				writeError(w, http.StatusBadRequest, "Stump URL must use http or https")
				return
			}
		}

		if err := s.settings.Set(next); err != nil {
			writeError(w, http.StatusInternalServerError, "saving settings: "+err.Error())
			return
		}

		writeJSON(w, http.StatusOK, toSettingsResponse(next))

	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}
