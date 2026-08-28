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

func TestSanitizeStripsPathAndControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Dracula", "Dracula"},
		{"../../etc/passwd", "etcpasswd"},
		{"/absolute/path", "absolutepath"},
		{`a\b`, "ab"},
		{"tab\tseparated", "tab separated"},
		{"null\x00byte", "nullbyte"},
		{"  spaced   out  ", "spaced out"},
		{"...hidden", "hidden"},
		{"-leading-dash", "leading-dash"},
		{"Alice's Adventures (Illustrated)", "Alice's Adventures (Illustrated)"},
		{"***", ""},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildFilename(t *testing.T) {
	got := buildFilename(&Book{Title: "Dracula", Authors: []string{"Bram Stoker"}, ext: ".epub"})
	if want := "Bram Stoker - Dracula.epub"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	got = buildFilename(&Book{Title: "Untitled Work", ext: ".txt"})
	if want := "Untitled Work.txt"; got != want {
		t.Errorf("no-author case: got %q, want %q", got, want)
	}

	// A title made entirely of stripped characters must still yield a filename.
	got = buildFilename(&Book{Title: "///", ext: ".epub"})
	if want := "untitled.epub"; got != want {
		t.Errorf("empty-title case: got %q, want %q", got, want)
	}

	// Truncation must not split a multi-byte rune.
	long := buildFilename(&Book{Title: strings.Repeat("é", 400), ext: ".epub"})
	if !strings.HasSuffix(long, ".epub") {
		t.Fatalf("truncated name lost its extension: %q", long)
	}
	if strings.ContainsRune(long, '�') {
		t.Errorf("truncation split a rune: %q", long)
	}
}

func TestSafeJoinRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	d := &Downloader{libraryPath: dir}

	if _, err := d.safeJoin("book.epub"); err != nil {
		t.Fatalf("plain name rejected: %v", err)
	}
	for _, bad := range []string{"../escape.epub", "sub/dir.epub", "/etc/passwd"} {
		if _, err := d.safeJoin(bad); err == nil {
			t.Errorf("safeJoin(%q) should have been rejected", bad)
		}
	}
}

func TestIsAllowedDownloadHost(t *testing.T) {
	allowed := []string{"gutenberg.org", "www.gutenberg.org"}
	cases := []struct {
		url  string
		want bool
	}{
		{"https://www.gutenberg.org/ebooks/345.epub3.images", true},
		{"https://gutenberg.org/x.epub", true},
		{"https://GUTENBERG.ORG/x.epub", true},
		{"http://www.gutenberg.org/x.epub", false},  // cleartext, not loopback
		{"https://evil.example.com/x.epub", false},  // not on the list
		{"https://gutenberg.org.evil.com/x", false}, // suffix trick
		{"file:///etc/passwd", false},
		{"http://169.254.169.254/latest/meta-data", false}, // link-local metadata
		{"not a url at all", false},
	}
	for _, c := range cases {
		if got := isAllowedDownloadHost(c.url, allowed); got != c.want {
			t.Errorf("isAllowedDownloadHost(%q) = %v, want %v", c.url, got, c.want)
		}
	}

	if !isAllowedDownloadHost("http://127.0.0.1:9/x.epub", []string{"127.0.0.1"}) {
		t.Error("cleartext loopback should be allowed for local mirrors")
	}
}

func TestPickGutenbergFormatPrefersEpubAndSkipsZips(t *testing.T) {
	hosts := []string{"www.gutenberg.org"}

	url, ext := pickGutenbergFormat(map[string]string{
		"text/plain; charset=utf-8": "https://www.gutenberg.org/x.txt",
		"application/epub+zip":      "https://www.gutenberg.org/x.epub",
	}, hosts)
	if ext != ".epub" || url != "https://www.gutenberg.org/x.epub" {
		t.Errorf("expected the epub, got %q %q", url, ext)
	}

	// Only a .zip archive on offer for the preferred type: fall through to text.
	url, ext = pickGutenbergFormat(map[string]string{
		"application/epub+zip":      "https://www.gutenberg.org/x.epub.zip",
		"text/plain; charset=utf-8": "https://www.gutenberg.org/x.txt",
	}, hosts)
	if ext != ".txt" {
		t.Errorf("expected the txt fallback, got %q %q", url, ext)
	}

	if url, _ := pickGutenbergFormat(map[string]string{
		"application/epub+zip": "https://evil.example.com/x.epub",
	}, hosts); url != "" {
		t.Errorf("off-allowlist URL should not be selected, got %q", url)
	}
}

func TestFlipName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Stoker, Bram", "Bram Stoker"},
		{"Shelley, Mary Wollstonecraft", "Mary Wollstonecraft Shelley"},
		{"Anonymous", "Anonymous"},
		{"Various", "Various"},
	}
	for _, c := range cases {
		if got := flipName(c.in); got != c.want {
			t.Errorf("flipName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeGutendex serves the subset of the Gutendex API the source uses, with
// download links pointing back at itself.
func fakeGutendex(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/x.epub":
			w.Write([]byte("EPUB-BYTES"))
		case strings.HasPrefix(r.URL.Path, "/books/") && r.URL.Path != "/books/":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fakeBookJSON(srv.URL))
		case r.URL.Path == "/books/":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"count":   1,
				"results": []any{fakeBookJSON(srv.URL)},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeBookJSON(base string) map[string]any {
	return map[string]any{
		"id":    345,
		"title": "Dracula",
		"authors": []any{
			map[string]any{"name": "Stoker, Bram"},
		},
		"languages":      []string{"en"},
		"download_count": 12345,
		"copyright":      false,
		"formats": map[string]string{
			"application/epub+zip": base + "/x.epub",
			"image/jpeg":           base + "/cover.jpg",
		},
	}
}

// fixedSettings builds an in-memory-only SettingsStore for tests that talk to
// StumpClient directly, without going through a full Server.
func fixedSettings(t *testing.T, v StumpSettings) *SettingsStore {
	t.Helper()
	s, err := NewSettingsStore("", v, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("building settings store: %v", err)
	}
	return s
}

func newTestServer(t *testing.T, gutendex *httptest.Server, stumpURL, libID string) (*Server, string) {
	t.Helper()
	libDir := t.TempDir()

	host := strings.TrimPrefix(gutendex.URL, "http://")
	host, _, _ = strings.Cut(host, ":")

	cfg := &Config{
		LibraryPath: libDir,
		MaxBytes:    1 << 20,
	}
	src := &GutenbergSource{
		baseURL: gutendex.URL,
		client:  &http.Client{Timeout: 5 * time.Second},
		hosts:   []string{host},
	}
	settings, err := NewSettingsStore("", StumpSettings{URL: stumpURL, APIKey: "test-key", LibraryID: libID}, ZLibrarySettings{})
	if err != nil {
		t.Fatalf("building settings store: %v", err)
	}

	return &Server{
		cfg:     cfg,
		sources: map[string]Source{src.Name(): src},
		downloader: &Downloader{
			libraryPath: libDir,
			maxBytes:    cfg.MaxBytes,
			client:      &http.Client{Timeout: 5 * time.Second},
		},
		stump:    NewStumpClient(settings),
		sessions: NewSessionStore(),
		settings: settings,
	}, libDir
}

func TestSearchHandler(t *testing.T) {
	srv, _ := newTestServer(t, fakeGutendex(t), "http://127.0.0.1:1", "")

	rec := httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=dracula", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out struct{ Results []Book }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out.Results))
	}
	got := out.Results[0]
	if got.Title != "Dracula" || got.ID != "345" {
		t.Errorf("unexpected book: %+v", got)
	}
	if len(got.Authors) != 1 || got.Authors[0] != "Bram Stoker" {
		t.Errorf("author not normalized: %+v", got.Authors)
	}
	if strings.Contains(rec.Body.String(), "downloadURL") {
		t.Error("download URL must not be exposed to the browser")
	}

	// A blank query is a client error, not an upstream call.
	rec = httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty query: status %d, want 400", rec.Code)
	}
}

// fakeStump implements the two GraphQL operations the client uses, at the path
// a real Stump 0.1.x serves them from.
func fakeStump(t *testing.T, scanned *string, scanErr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" || r.Method != http.MethodPost {
			// Mirror the real server: unrouted paths fall through to the web UI.
			w.Write([]byte("<!doctype html><html><body>Stump</body></html>"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Unauthorized","status":401}`))
			return
		}

		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "scanLibrary"):
			if scanErr != "" {
				fmt.Fprintf(w, `{"errors":[{"message":%q}]}`, scanErr)
				return
			}
			if scanned != nil {
				*scanned, _ = req.Variables["id"].(string)
			}
			w.Write([]byte(`{"data":{"scanLibrary":true}}`))
		case strings.Contains(req.Query, "libraries"):
			w.Write([]byte(`{"data":{"libraries":{"nodes":[{"id":"lib-1","name":"Books","path":"/data/books"}]}}}`))
		default:
			w.Write([]byte(`{"errors":[{"message":"unknown operation"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAddHandlerDownloadsAndTriggersScan(t *testing.T) {
	var scanned string
	stump := fakeStump(t, &scanned, "")

	srv, libDir := newTestServer(t, fakeGutendex(t), stump.URL, "lib-1")

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/add",
			strings.NewReader(`{"source":"gutenberg","id":"345"}`))
		srv.handleAdd(rec, req)
		return rec
	}

	rec := post()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "added" {
		t.Errorf("status = %v, want added", out["status"])
	}
	if out["scan"] != "triggered" {
		t.Errorf("scan = %v (%v)", out["scan"], out["scanError"])
	}
	if scanned != "lib-1" {
		t.Errorf("Stump was asked to scan %q, want lib-1", scanned)
	}

	dest := filepath.Join(libDir, "Bram Stoker - Dracula.epub")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("book not written to the library: %v", err)
	}
	if string(data) != "EPUB-BYTES" {
		t.Errorf("unexpected file contents: %q", data)
	}

	// No .part files should survive a successful download.
	entries, _ := os.ReadDir(libDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	// Adding the same book again is a no-op, not a duplicate or an error.
	rec = post()
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "already-present" {
		t.Errorf("second add: status = %v, want already-present", out["status"])
	}
	if entries, _ := os.ReadDir(libDir); len(entries) != 1 {
		t.Errorf("expected 1 file in the library, got %d", len(entries))
	}
}

func TestAddSucceedsWhenScanFails(t *testing.T) {
	// A GraphQL error arrives with HTTP 200, so this also covers the client
	// noticing failures the status code hides.
	stump := fakeStump(t, nil, "library not found")
	srv, libDir := newTestServer(t, fakeGutendex(t), stump.URL, "missing")

	rec := httptest.NewRecorder()
	srv.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add",
		strings.NewReader(`{"source":"gutenberg","id":"345"}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("a failed rescan must not fail the add: %d %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["scan"] != "failed" {
		t.Errorf("scan = %v, want failed", out["scan"])
	}
	if _, err := os.Stat(filepath.Join(libDir, "Bram Stoker - Dracula.epub")); err != nil {
		t.Errorf("file should still have been saved: %v", err)
	}
}

func TestLibrariesParsesGraphQLResponse(t *testing.T) {
	stump := fakeStump(t, nil, "")

	libs, err := NewStumpClient(fixedSettings(t, StumpSettings{URL: stump.URL, APIKey: "test-key"})).Libraries(context.Background())
	if err != nil {
		t.Fatalf("listing libraries: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	if libs[0].ID != "lib-1" || libs[0].Name != "Books" || libs[0].Path != "/data/books" {
		t.Errorf("unexpected library: %+v", libs[0])
	}
}

func TestStumpClientReportsBadCredentials(t *testing.T) {
	stump := fakeStump(t, nil, "")

	_, err := NewStumpClient(fixedSettings(t, StumpSettings{URL: stump.URL, APIKey: "wrong-key"})).Libraries(context.Background())
	if err == nil {
		t.Fatal("expected an error for a bad API key")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention the 401, got: %v", err)
	}
}

// Pointing STUMP_URL at something that isn't Stump's API returns its web UI
// with HTTP 200. That must be reported as a configuration problem, not as an
// unparseable response.
func TestStumpClientDetectsWebUIResponse(t *testing.T) {
	notTheAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<!doctype html><html><body>Stump</body></html>"))
	}))
	defer notTheAPI.Close()

	_, err := NewStumpClient(fixedSettings(t, StumpSettings{URL: notTheAPI.URL, APIKey: "test-key"})).Libraries(context.Background())
	if err == nil {
		t.Fatal("expected an error when the API path returns the web UI")
	}
	if !strings.Contains(err.Error(), "STUMP_URL") {
		t.Errorf("error should point at STUMP_URL, got: %v", err)
	}
}

func TestAddRejectsUnknownSource(t *testing.T) {
	srv, _ := newTestServer(t, fakeGutendex(t), "http://127.0.0.1:1", "")

	rec := httptest.NewRecorder()
	srv.handleAdd(rec, httptest.NewRequest(http.MethodPost, "/api/add",
		strings.NewReader(`{"source":"elsewhere","id":"1"}`)))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}
}

func TestDownloaderEnforcesSizeLimit(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer big.Close()

	dir := t.TempDir()
	d := &Downloader{libraryPath: dir, maxBytes: 1000, client: big.Client()}

	host, _, _ := strings.Cut(strings.TrimPrefix(big.URL, "http://"), ":")
	book := &Book{Title: "Huge", ext: ".epub", downloadURL: big.URL + "/x.epub"}

	if _, err := d.Save(context.Background(), book, []string{host}); err == nil {
		t.Fatal("expected the size limit to reject this download")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("an over-limit download left files behind: %v", entries)
	}
}

func TestDownloaderRejectsDisallowedHost(t *testing.T) {
	d := &Downloader{libraryPath: t.TempDir(), maxBytes: 1 << 20, client: http.DefaultClient}
	book := &Book{Title: "Evil", ext: ".epub", downloadURL: "https://evil.example.com/x.epub"}

	if _, err := d.Save(context.Background(), book, []string{"gutenberg.org"}); err == nil {
		t.Fatal("expected a disallowed host to be refused")
	}
}
