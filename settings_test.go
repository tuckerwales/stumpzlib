package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStumpSettingsConfigured(t *testing.T) {
	cases := []struct {
		name string
		v    StumpSettings
		want bool
	}{
		{"empty", StumpSettings{}, false},
		{"url only", StumpSettings{URL: "http://x"}, false},
		{"api key", StumpSettings{URL: "http://x", APIKey: "k"}, true},
		{"user+pass", StumpSettings{URL: "http://x", Username: "u", Password: "p"}, true},
		{"user without pass", StumpSettings{URL: "http://x", Username: "u"}, false},
	}
	for _, c := range cases {
		if got := c.v.Configured(); got != c.want {
			t.Errorf("%s: Configured() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSettingsStorePersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".stumpzlib-settings.json")

	s1, err := NewSettingsStore(path, StumpSettings{URL: "http://initial"}, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	saved := StumpSettings{URL: "http://saved", APIKey: "key", LibraryID: "lib-1"}
	if err := s1.Set(saved); err != nil {
		t.Fatalf("saving: %v", err)
	}

	// A fresh store pointed at the same file should load what was saved,
	// ignoring the "initial" env-derived value passed in this time.
	s2, err := NewSettingsStore(path, StumpSettings{URL: "http://different-env-value"}, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("reloading store: %v", err)
	}
	if got := s2.Get(); got != saved {
		t.Errorf("reloaded settings = %+v, want %+v", got, saved)
	}
}

func TestSettingsStoreSeedsZLibraryOnLegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".stumpzlib-settings.json")
	if err := os.WriteFile(path, []byte(`{"url":"http://stump","apiKey":"k"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewSettingsStore(path, StumpSettings{}, ZLibrarySettings{Email: "from-env", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get().URL; got != "http://stump" {
		t.Errorf("stump URL = %q", got)
	}
	if got := s.GetZLibrary(); got.Email != "from-env" {
		t.Errorf("legacy file should pick up env-seeded zlibrary, got %+v", got)
	}
}

func TestSettingsStoreEmptyPathDoesNotPersist(t *testing.T) {
	s, err := NewSettingsStore("", StumpSettings{URL: "http://x"}, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if err := s.Set(StumpSettings{URL: "http://y"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := s.Get().URL; got != "http://y" {
		t.Errorf("in-memory update lost: got %q", got)
	}
}

func TestSettingsFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".stumpzlib-settings.json")
	s, err := NewSettingsStore(path, StumpSettings{}, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if err := s.Set(StumpSettings{APIKey: "secret"}); err != nil {
		t.Fatalf("set: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("settings file mode = %v, want -rw-------", perm)
	}
}

func settingsTestServer(t *testing.T, initial StumpSettings) *Server {
	t.Helper()
	return &Server{settings: fixedSettings(t, initial)}
}

func TestHandleSettingsGetNeverLeaksSecrets(t *testing.T) {
	srv := settingsTestServer(t, StumpSettings{
		URL: "http://stump", APIKey: "super-secret-key", Username: "alice", Password: "super-secret-pass",
	})

	rec := httptest.NewRecorder()
	srv.handleSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	body := rec.Body.String()
	if strings.Contains(body, "super-secret-key") || strings.Contains(body, "super-secret-pass") {
		t.Fatalf("response leaked a secret: %s", body)
	}

	var out settingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !out.HasAPIKey || !out.HasPassword {
		t.Errorf("expected both secrets flagged as set: %+v", out)
	}
}

func TestHandleSettingsPostKeepsSecretsWhenLeftBlank(t *testing.T) {
	srv := settingsTestServer(t, StumpSettings{URL: "http://stump", APIKey: "original-key", LibraryID: "old"})

	rec := httptest.NewRecorder()
	body := `{"url":"http://stump","libraryId":"new-lib"}`
	srv.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := srv.settings.Get()
	if got.APIKey != "original-key" {
		t.Errorf("API key was cleared by an update that didn't mention it: %+v", got)
	}
	if got.LibraryID != "new-lib" {
		t.Errorf("library id was not updated: %+v", got)
	}
}

func TestHandleSettingsPostClearsSecretWhenAsked(t *testing.T) {
	srv := settingsTestServer(t, StumpSettings{URL: "http://stump", APIKey: "original-key"})

	rec := httptest.NewRecorder()
	body := `{"url":"http://stump","clearApiKey":true}`
	srv.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := srv.settings.Get().APIKey; got != "" {
		t.Errorf("API key should have been cleared, got %q", got)
	}
}

func TestHandleSettingsPostRejectsBadURL(t *testing.T) {
	srv := settingsTestServer(t, StumpSettings{})

	rec := httptest.NewRecorder()
	body := `{"url":"not a url"}`
	srv.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

func TestHandleSettingsPostAllowsClearingURL(t *testing.T) {
	srv := settingsTestServer(t, StumpSettings{URL: "http://stump", APIKey: "k"})

	rec := httptest.NewRecorder()
	body := `{"url":""}`
	srv.handleSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got := srv.settings.Get().URL; got != "" {
		t.Errorf("URL should have been cleared, got %q", got)
	}
}
