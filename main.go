package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed static
var staticFS embed.FS

const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute
)

type Server struct {
	cfg        *Config
	sources    map[string]Source
	downloader *Downloader
	stump      *StumpClient
	sessions   *SessionStore
	settings   *SettingsStore
}

func main() {
	log.SetFlags(log.Ltime)

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	apiClient := &http.Client{Timeout: apiTimeout}

	settingsPath := filepath.Join(cfg.LibraryPath, ".stumpzlib-settings.json")
	settings, err := NewSettingsStore(settingsPath, cfg.InitialStump, cfg.InitialZLibrary)
	if err != nil {
		log.Fatalf("loading settings: %v", err)
	}

	srv := &Server{
		cfg:     cfg,
		sources: newSources(cfg, apiClient, settings),
		downloader: &Downloader{
			libraryPath: cfg.LibraryPath,
			maxBytes:    cfg.MaxBytes,
			client:      &http.Client{Timeout: downloadTimeout},
		},
		stump:    NewStumpClient(settings),
		sessions: NewSessionStore(),
		settings: settings,
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("loading embedded assets: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", srv.requireAuth(http.FileServer(http.FS(static))))
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.HandleFunc("/login", srv.handleLogin)
	mux.HandleFunc("/logout", srv.handleLogout)
	mux.Handle("/api/status", srv.requireAuth(http.HandlerFunc(srv.handleStatus)))
	mux.Handle("/api/search", srv.requireAuth(http.HandlerFunc(srv.handleSearch)))
	mux.Handle("/api/add", srv.requireAuth(http.HandlerFunc(srv.handleAdd)))
	mux.Handle("/api/settings", srv.requireAuth(http.HandlerFunc(srv.handleSettings)))
	mux.Handle("/api/zlibrary", srv.requireAuth(http.HandlerFunc(srv.handleZLibrary)))
	mux.Handle("/api/zlibrary/login", srv.requireAuth(http.HandlerFunc(srv.handleZLibraryLogin)))
	mux.Handle("/api/zlibrary/discover", srv.requireAuth(http.HandlerFunc(srv.handleZLibraryDiscover)))

	stumpNow := settings.Get()
	log.Printf("library directory : %s", cfg.LibraryPath)
	log.Printf("stump server      : %s", stumpNow.URL)
	if !stumpNow.Configured() {
		log.Printf("stump auth        : not configured — downloads will work, rescans will not. Set it at /settings.html")
	} else if stumpNow.LibraryID == "" {
		log.Printf("stump library id  : not set — set it at /settings.html")
	}
	if !cfg.AuthConfigured() {
		log.Printf("app auth          : not configured — anyone who can reach this app can use it")
	} else {
		log.Printf("app auth          : enabled — log in at /login as %q", cfg.AuthUsername)
	}
	zlibNow := settings.GetZLibrary()
	if zlibNow.BaseURL == "" {
		log.Printf("z-library         : no base URL — set it at /settings.html or run auto-discover")
	} else {
		log.Printf("z-library         : %s", zlibNow.BaseURL)
	}
	log.Printf("listening on http://localhost%s", cfg.Listen)

	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}

// --- handlers --------------------------------------------------------------

// handleHealth is the container healthcheck. It reports unhealthy when the
// library directory isn't writable, which is what a volume that failed to
// mount looks like — the app would otherwise sit there accepting searches and
// failing every download.
//
// It deliberately does not check the Stump server: Stump being down is not a
// reason to restart this container.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !isWritableDir(s.cfg.LibraryPath) {
		http.Error(w, "library directory is not writable: "+s.cfg.LibraryPath,
			http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

type statusResponse struct {
	LibraryPath  string          `json:"libraryPath"`
	Writable     bool            `json:"writable"`
	StumpURL     string          `json:"stumpUrl"`
	StumpOK      bool            `json:"stumpOk"`
	StumpError   string          `json:"stumpError,omitempty"`
	LibraryID    string          `json:"libraryId"`
	Libraries    []StumpLibrary  `json:"libraries,omitempty"`
	SourceLabels []sourceInfo    `json:"sources"`
	AuthEnabled  bool            `json:"authEnabled"`
	ZLibrary     *zlibraryStatus `json:"zlibrary,omitempty"`
}

type zlibraryStatus struct {
	BaseURL        string `json:"baseUrl"`
	HasCredentials bool   `json:"hasCredentials"`
	HasSession     bool   `json:"hasSession"`
}

type sourceInfo struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Browse bool   `json:"browse,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
	defer cancel()

	stumpNow := s.settings.Get()

	out := statusResponse{
		LibraryPath: s.cfg.LibraryPath,
		Writable:    isWritableDir(s.cfg.LibraryPath),
		StumpURL:    stumpNow.URL,
		LibraryID:   stumpNow.LibraryID,
		AuthEnabled: s.cfg.AuthConfigured(),
	}
	for _, src := range s.sourcesInOrder() {
		info := sourceInfo{Name: src.Name(), Label: src.Label()}
		if src.Name() == "zlibrary" {
			info.Browse = true
		}
		out.SourceLabels = append(out.SourceLabels, info)
	}

	zlib := s.settings.GetZLibrary()
	out.ZLibrary = &zlibraryStatus{
		BaseURL:        zlib.BaseURL,
		HasCredentials: zlib.HasCredentials(),
		HasSession:     zlib.HasSession(),
	}

	if stumpNow.Configured() {
		libs, err := s.stump.Libraries(ctx)
		if err != nil {
			out.StumpError = err.Error()
		} else {
			out.StumpOK = true
			out.Libraries = libs
		}
	} else {
		out.StumpError = "no Stump credentials configured"
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	list := strings.TrimSpace(r.URL.Query().Get("list"))
	if query == "" && list == "" {
		writeError(w, http.StatusBadRequest, "a search query is required")
		return
	}

	name := r.URL.Query().Get("source")
	if name == "" {
		name = "gutenberg"
	}
	src, ok := s.sources[name]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q", name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
	defer cancel()

	q := SearchQuery{
		Text:       query,
		Languages:  splitCSV(r.URL.Query().Get("lang")),
		Extensions: splitCSV(r.URL.Query().Get("ext")),
		Order:      strings.TrimSpace(r.URL.Query().Get("order")),
		List:       list,
	}

	books, err := src.Search(ctx, q)
	if err != nil {
		log.Printf("search %q on %s failed: %v", query, name, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"results": books})
}

type addRequest struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

func (s *Server) handleAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req addRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse the request body")
		return
	}

	src, ok := s.sources[req.Source]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q", req.Source))
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeError(w, http.StatusBadRequest, "a book id is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), downloadTimeout)
	defer cancel()

	// Resolve server-side: the browser never supplies the download URL.
	book, err := src.Resolve(ctx, req.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	hosts := append([]string(nil), src.DownloadHosts()...)
	hosts = append(hosts, book.downloadHosts...)
	filename, err := s.downloader.Save(ctx, book, hosts)
	switch {
	case errors.Is(err, ErrAlreadyExists):
		writeJSON(w, http.StatusOK, map[string]any{
			"filename": filename,
			"status":   "already-present",
			"message":  "Already in the library — nothing downloaded.",
		})
		return
	case err != nil:
		log.Printf("download of %s/%s failed: %v", req.Source, req.ID, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	log.Printf("saved %q", filename)

	// The file is on disk and Stump will find it on its next scan either way,
	// so a failed rescan is reported but not treated as a failed add.
	result := map[string]any{
		"filename": filename,
		"status":   "added",
		"message":  fmt.Sprintf("Saved %s", filename),
	}
	stumpNow := s.settings.Get()
	if stumpNow.Configured() && stumpNow.LibraryID != "" {
		if err := s.stump.Scan(ctx, stumpNow.LibraryID); err != nil {
			log.Printf("rescan failed: %v", err)
			result["scan"] = "failed"
			result["scanError"] = err.Error()
			result["message"] = fmt.Sprintf("Saved %s, but the Stump rescan failed", filename)
		} else {
			result["scan"] = "triggered"
			result["message"] = fmt.Sprintf("Saved %s and triggered a Stump rescan", filename)
		}
	} else {
		result["scan"] = "skipped"
	}

	writeJSON(w, http.StatusOK, result)
}

// --- helpers ---------------------------------------------------------------

func (s *Server) sourcesInOrder() []Source {
	order := []string{"zlibrary", "gutenberg"}
	out := make([]Source, 0, len(s.sources))
	seen := make(map[string]bool, len(s.sources))
	for _, name := range order {
		if src, ok := s.sources[name]; ok {
			out = append(out, src)
			seen[name] = true
		}
	}
	for name, src := range s.sources {
		if !seen[name] {
			out = append(out, src)
		}
	}
	return out
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isWritableDir(path string) bool {
	f, err := os.CreateTemp(path, ".stumpzlib-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
