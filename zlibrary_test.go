package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseZLibID(t *testing.T) {
	id, hash, err := parseZLibID("123:deadbeef")
	if err != nil || id != "123" || hash != "deadbeef" {
		t.Fatalf("got %q %q %v", id, hash, err)
	}
	for _, bad := range []string{"", "123", "123:", ":hash", "123:ab/cd", "123:.."} {
		if _, _, err := parseZLibID(bad); err == nil {
			t.Errorf("parseZLibID(%q) should have failed", bad)
		}
	}
}

func TestValidateZLibraryBaseURL(t *testing.T) {
	got, err := validateZLibraryBaseURL("z-lib.example")
	if err != nil || got != "https://z-lib.example" {
		t.Errorf("bare host: got %q %v", got, err)
	}
	got, err = validateZLibraryBaseURL("https://z-lib.example/")
	if err != nil || got != "https://z-lib.example" {
		t.Errorf("trailing slash: got %q %v", got, err)
	}
	if _, err := validateZLibraryBaseURL("https://host/path"); err == nil {
		t.Error("path should be rejected")
	}
	if _, err := validateZLibraryBaseURL("not a url"); err == nil {
		t.Error("spaces should be rejected")
	}
	if got, err := validateZLibraryBaseURL(""); err != nil || got != "" {
		t.Errorf("empty should be allowed, got %q %v", got, err)
	}
}

func TestUsableCoverURL(t *testing.T) {
	if got := usableCoverURL("/img/cover-not-exists.png"); got != "" {
		t.Errorf("relative placeholder should be dropped, got %q", got)
	}
	if got := usableCoverURL("https://cdn.example/c.jpg"); got == "" {
		t.Error("absolute https cover should be kept")
	}
}

func TestBookExt(t *testing.T) {
	if got := bookExt("EPUB"); got != ".epub" {
		t.Errorf("got %q", got)
	}
	if got := bookExt("a/b"); got != "" {
		t.Errorf("slash should be rejected, got %q", got)
	}
}

func TestLooksLikeBotChallenge(t *testing.T) {
	if !looksLikeBotChallenge([]byte("<html>Verifying your browser</html>")) {
		t.Error("expected a challenge page to be detected")
	}
	if looksLikeBotChallenge([]byte(`{"success":1}`)) {
		t.Error("JSON should not look like a challenge")
	}
}

func TestDownloaderRejectsHTMLPage(t *testing.T) {
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<!doctype html><html>quota</html>"))
	}))
	t.Cleanup(html.Close)

	dir := t.TempDir()
	d := &Downloader{libraryPath: dir, maxBytes: 1 << 20, client: html.Client()}
	host, _, _ := strings.Cut(strings.TrimPrefix(html.URL, "http://"), ":")
	book := &Book{Title: "Quota", ext: ".epub", downloadURL: html.URL + "/x.epub"}
	if _, err := d.Save(context.Background(), book, []string{host}); err == nil {
		t.Fatal("expected an HTML body to be rejected")
	}

	// Same page with a lying Content-Type: the first-bytes peek still catches it.
	lying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("<html>quota</html>"))
	}))
	t.Cleanup(lying.Close)
	host, _, _ = strings.Cut(strings.TrimPrefix(lying.URL, "http://"), ":")
	d = &Downloader{libraryPath: t.TempDir(), maxBytes: 1 << 20, client: lying.Client()}
	book = &Book{Title: "Quota2", ext: ".epub", downloadURL: lying.URL + "/x.epub"}
	if _, err := d.Save(context.Background(), book, []string{host}); err == nil {
		t.Fatal("expected an HTML body to be rejected even without a text/html Content-Type")
	}
}

// fakeZlib serves the subset of the eAPI the source uses. downloadLink points
// back at itself so Resolve + Save can run end to end over loopback HTTP.
func fakeZlib(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/dl/book.epub":
			if !strings.Contains(r.Header.Get("Cookie"), "remix_userid=99") {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"Please login"}`))
				return
			}
			w.Header().Set("Content-Type", "application/epub+zip")
			w.Write([]byte("EPUB-BYTES"))
		case r.URL.Path == "/eapi/info/ok":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":1}`))
		case r.URL.Path == "/eapi/user/login" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			if r.Form.Get("email") != "a@b.c" || r.Form.Get("password") != "pw" {
				w.Write([]byte(`{"success":0,"error":"Incorrect email or password"}`))
				return
			}
			w.Write([]byte(`{"success":1,"user":{"id":99,"remix_userkey":"abc"}}`))
		case r.URL.Path == "/eapi/book/search" && r.Method == http.MethodPost:
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(zlibSearchJSON(srv.URL, r.Form.Get("languages[0]"), r.Form.Get("extensions[0]"), r.Form.Get("order"))))
		case r.URL.Path == "/eapi/book/most-popular":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":1,"books":[` + zlibBookJSON(srv.URL) + `]}`))
		case r.URL.Path == "/eapi/user/book/recommended":
			if !strings.Contains(r.Header.Get("Cookie"), "remix_userid=99") {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"success":0,"error":"Please login"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":1,"books":[` + zlibBookJSON(srv.URL) + `]}`))
		case r.URL.Path == "/eapi/user/profile":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"success":1,"user":{"downloads_today":2,"downloads_limit":10}}`))
		case strings.HasSuffix(r.URL.Path, "/file"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"success":1,"file":{"downloadLink":%q,"extension":"epub","allowDownload":true}}`, srv.URL+"/dl/book.epub")
		case strings.HasPrefix(r.URL.Path, "/eapi/book/"):
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"success":1,"book":%s}`, zlibBookJSON(srv.URL))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func zlibBookJSON(base string) string {
	cover := base + "/cover.jpg"
	return fmt.Sprintf(`{"id":42,"hash":"deadbeef","title":"Neuromancer","author":"William Gibson","year":1984,"extension":"EPUB","filesizeString":"1.2 MB","language":"english","cover":%q,"href":"/book/42/deadbeef/neuromancer"}`, cover)
}

func zlibSearchJSON(base, lang, ext, order string) string {
	note := zlibBookJSON(base)
	return fmt.Sprintf(`{"success":1,"echo":{"lang":%q,"ext":%q,"order":%q},"books":[%s]}`, lang, ext, order, note)
}

func newZlibServer(t *testing.T, upstream *httptest.Server, email, password string) (*Server, string) {
	t.Helper()
	libDir := t.TempDir()
	settings, err := NewSettingsStore("", StumpSettings{}, ZLibrarySettings{
		BaseURL:  strings.TrimRight(upstream.URL, "/"),
		Email:    email,
		Password: password,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	src := NewZLibrarySource(settings)
	return &Server{
		cfg:     &Config{LibraryPath: libDir, MaxBytes: 1 << 20},
		sources: map[string]Source{src.Name(): src},
		downloader: &Downloader{
			libraryPath: libDir,
			maxBytes:    1 << 20,
			client:      &http.Client{Timeout: 5 * time.Second},
		},
		stump:    NewStumpClient(settings),
		sessions: NewSessionStore(),
		settings: settings,
	}, libDir
}

func TestZLibrarySearch(t *testing.T) {
	upstream := fakeZlib(t)
	srv, _ := newZlibServer(t, upstream, "a@b.c", "pw")

	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=neuromancer&source=zlibrary&lang=english&ext=EPUB&order=bestmatch", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct{ Results []Book }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("got %d results", len(out.Results))
	}
	got := out.Results[0]
	if got.Title != "Neuromancer" || got.ID != "42:deadbeef" {
		t.Errorf("unexpected book: %+v", got)
	}
	if strings.Contains(rec.Body.String(), "downloadURL") {
		t.Error("download URL must not be exposed to the browser")
	}
	if got.CoverURL == "" {
		t.Error("expected a cover URL")
	}
}

func TestZLibrarySearchWorksWithoutCredentials(t *testing.T) {
	upstream := fakeZlib(t)
	srv, _ := newZlibServer(t, upstream, "", "")

	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=neuromancer&source=zlibrary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestZLibraryPopularAndRecommended(t *testing.T) {
	upstream := fakeZlib(t)
	srv, _ := newZlibServer(t, upstream, "a@b.c", "pw")

	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?source=zlibrary&list=popular", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("popular: status %d: %s", rec.Code, rec.Body.String())
	}

	// Recommended requires an account; this one has credentials so login runs.
	rec = httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?source=zlibrary&list=recommended", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("recommended: status %d: %s", rec.Code, rec.Body.String())
	}

	noAuth, _ := newZlibServer(t, upstream, "", "")
	rec = httptest.NewRecorder()
	noAuth.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?source=zlibrary&list=recommended", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("recommended without credentials: status %d, want 502", rec.Code)
	}
}

func TestZLibraryAddDownloadsAndLogsIn(t *testing.T) {
	upstream := fakeZlib(t)
	srv, libDir := newZlibServer(t, upstream, "a@b.c", "pw")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/add",
		strings.NewReader(`{"source":"zlibrary","id":"42:deadbeef"}`))
	srv.handleAdd(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "added" {
		t.Errorf("status = %v, want added", out["status"])
	}

	dest := filepath.Join(libDir, "William Gibson - Neuromancer.epub")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("book not written: %v", err)
	}
	if string(data) != "EPUB-BYTES" {
		t.Errorf("contents: %q", data)
	}

	if !srv.settings.GetZLibrary().HasSession() {
		t.Error("a successful download should have stored a session")
	}
}

func TestZLibraryLoginRejectsBadPassword(t *testing.T) {
	upstream := fakeZlib(t)
	srv, _ := newZlibServer(t, upstream, "a@b.c", "wrong")

	rec := httptest.NewRecorder()
	srv.handleZLibraryLogin(rec, httptest.NewRequest(http.MethodPost, "/api/zlibrary/login", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestZLibraryLoginReportsQuota(t *testing.T) {
	upstream := fakeZlib(t)
	srv, _ := newZlibServer(t, upstream, "a@b.c", "pw")

	rec := httptest.NewRecorder()
	srv.handleZLibraryLogin(rec, httptest.NewRequest(http.MethodPost, "/api/zlibrary/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["downloadsToday"] != float64(2) || out["downloadsLimit"] != float64(10) {
		t.Errorf("quota: %+v", out)
	}
}

func TestZLibrarySettingsNeverLeakSecrets(t *testing.T) {
	srv, _ := newZlibServer(t, fakeZlib(t), "a@b.c", "super-secret-pass")
	cur := srv.settings.GetZLibrary()
	cur.UserID = "99"
	cur.UserKey = "super-secret-key"
	if err := srv.settings.SetZLibrary(cur); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.handleZLibrary(rec, httptest.NewRequest(http.MethodGet, "/api/zlibrary", nil))
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-pass") || strings.Contains(body, "super-secret-key") {
		t.Fatalf("leaked a secret: %s", body)
	}
	if !strings.Contains(body, `"hasPassword":true`) {
		t.Errorf("expected hasPassword: %s", body)
	}
}

func TestZLibrarySettingsPostKeepsPasswordWhenBlank(t *testing.T) {
	srv, _ := newZlibServer(t, fakeZlib(t), "a@b.c", "original-pass")

	rec := httptest.NewRecorder()
	body := `{"baseUrl":"https://z-lib.example","email":"a@b.c","languages":["english"],"extensions":["EPUB"]}`
	srv.handleZLibrary(rec, httptest.NewRequest(http.MethodPost, "/api/zlibrary", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := srv.settings.GetZLibrary()
	if got.Password != "original-pass" {
		t.Errorf("password was cleared: %+v", got)
	}
	if got.BaseURL != "https://z-lib.example" {
		t.Errorf("base URL: %q", got.BaseURL)
	}
}

func TestSettingsStoreKeepsZLibraryWhenSavingStump(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".stumpzlib-settings.json")
	s, err := NewSettingsStore(path, StumpSettings{URL: "http://stump"}, ZLibrarySettings{Email: "a@b.c", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set(StumpSettings{URL: "http://stump2", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}

	s2, err := NewSettingsStore(path, StumpSettings{URL: "http://other"}, ZLibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get().URL; got != "http://stump2" {
		t.Errorf("stump URL = %q", got)
	}
	if got := s2.GetZLibrary(); got.Email != "a@b.c" || got.Password != "pw" {
		t.Errorf("zlibrary was lost: %+v", got)
	}
}

func TestZLibraryDiscoverPicksAWorkingMirror(t *testing.T) {
	good := fakeZlib(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(513)
		w.Write([]byte("<html>Verifying your browser DiamWall</html>"))
	}))
	t.Cleanup(bad.Close)

	settings, err := NewSettingsStore("", StumpSettings{}, ZLibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	z := NewZLibrarySource(settings)
	z.seeds = []string{bad.URL, good.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	chosen, err := z.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if chosen != strings.TrimRight(good.URL, "/") && chosen != good.URL {
		// setBaseURL trims trailing slash; httptest URLs have none
		if strings.TrimRight(chosen, "/") != strings.TrimRight(good.URL, "/") {
			t.Errorf("chose %q, want the working server %q", chosen, good.URL)
		}
	}
}

func TestZLibraryBotChallengeIsReported(t *testing.T) {
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("<html>Just a moment... Checking your browser</html>"))
	}))
	t.Cleanup(blocked.Close)

	srv, _ := newZlibServer(t, blocked, "", "")
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=x&source=zlibrary", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "refusing automated access") {
		t.Errorf("error should mention the block: %s", rec.Body.String())
	}
}

func TestZLibraryDownloadLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/eapi/user/login":
			w.Write([]byte(`{"success":1,"user":{"id":99,"remix_userkey":"abc"}}`))
		case strings.HasSuffix(r.URL.Path, "/file"):
			w.Write([]byte(`{"success":1,"file":{"allowDownload":false}}`))
		case strings.HasPrefix(r.URL.Path, "/eapi/book/"):
			w.Write([]byte(`{"success":1,"book":{"id":1,"hash":"abc","title":"X","author":"Y","extension":"EPUB"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	srv, _ := newZlibServer(t, upstream, "a@b.c", "pw")
	rec := httptest.NewRecorder()
	srv.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add",
		strings.NewReader(`{"source":"zlibrary","id":"1:abc"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "download limit") {
		t.Errorf("expected a quota message: %s", rec.Body.String())
	}
}

func TestZLibraryAuthRetryOnPleaseLogin(t *testing.T) {
	var searches int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/eapi/user/login":
			w.Write([]byte(`{"success":1,"user":{"id":99,"remix_userkey":"abc"}}`))
		case r.URL.Path == "/eapi/book/search":
			searches++
			if searches == 1 {
				w.Write([]byte(`{"success":0,"error":"Please login"}`))
				return
			}
			w.Write([]byte(`{"success":1,"books":[{"id":1,"hash":"abc","title":"X","author":"Y","extension":"EPUB"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	srv, _ := newZlibServer(t, upstream, "a@b.c", "pw")
	// Pretend we already have a stale session so the first search is attempted
	// authenticated and comes back "Please login".
	cur := srv.settings.GetZLibrary()
	cur.UserID = "stale"
	cur.UserKey = "stale"
	if err := srv.settings.SetZLibrary(cur); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=x&source=zlibrary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if searches < 2 {
		t.Errorf("expected a retry after re-login, searches=%d", searches)
	}
}
