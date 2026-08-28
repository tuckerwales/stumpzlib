package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Book is the normalized shape every source is flattened into before it
// reaches the UI.
type Book struct {
	Source   string   `json:"source"`
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Authors  []string `json:"authors"`
	Language string   `json:"language"`
	CoverURL string   `json:"coverUrl"`
	Note     string   `json:"note"`

	// downloadURL is resolved server-side and deliberately not sent to the
	// browser: /api/add takes a source + id and re-resolves the URL here, so a
	// crafted client can never point the downloader at an arbitrary host.
	downloadURL     string
	downloadHeaders map[string]string
	downloadHosts   []string
	href            string
	ext             string
}

// SearchQuery is what a source's Search method receives. Sources that don't
// support filters or named lists ignore the extra fields.
type SearchQuery struct {
	Text       string
	Languages  []string
	Extensions []string
	Order      string
	List       string // "popular" or "recommended"; empty means a text search
}

// Source is a searchable catalog of downloadable books.
//
// Adding a catalog means implementing these methods and registering the result
// in newSources below — see README.md ("Adding a source"). Whatever a source
// returns from Resolve is fetched by the server, so a source must only ever
// hand back URLs on hosts it controls, and DownloadHosts must list exactly
// those hosts: the downloader refuses anything else.
type Source interface {
	Name() string
	Label() string
	DownloadHosts() []string
	Search(ctx context.Context, q SearchQuery) ([]Book, error)
	Resolve(ctx context.Context, id string) (*Book, error)
}

func newSources(cfg *Config, client *http.Client, settings *SettingsStore) map[string]Source {
	list := []Source{
		NewZLibrarySource(settings),
		&GutenbergSource{baseURL: cfg.GutendexURL, client: client},
	}
	m := make(map[string]Source, len(list))
	for _, s := range list {
		m[s.Name()] = s
	}
	return m
}

// ---------------------------------------------------------------------------
// Project Gutenberg, via the Gutendex API (https://gutendex.com).
//
// Everything Gutendex indexes is public domain in the US, and the download
// URLs it returns all live on gutenberg.org.
// ---------------------------------------------------------------------------

type GutenbergSource struct {
	baseURL string
	client  *http.Client

	// hosts overrides the download allowlist; empty means gutenberg.org.
	hosts []string
}

func (g *GutenbergSource) Name() string  { return "gutenberg" }
func (g *GutenbergSource) Label() string { return "Project Gutenberg" }

func (g *GutenbergSource) DownloadHosts() []string {
	if len(g.hosts) > 0 {
		return g.hosts
	}
	return []string{"gutenberg.org", "www.gutenberg.org"}
}

type gutendexResponse struct {
	Count   int            `json:"count"`
	Results []gutendexBook `json:"results"`
}

type gutendexBook struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Subjects []string `json:"subjects"`
	Authors  []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Languages     []string          `json:"languages"`
	Formats       map[string]string `json:"formats"`
	DownloadCount int               `json:"download_count"`
	Copyright     *bool             `json:"copyright"`
}

func (g *GutenbergSource) Search(ctx context.Context, q SearchQuery) ([]Book, error) {
	if q.List != "" {
		return nil, fmt.Errorf("Project Gutenberg does not have a %q list", q.List)
	}
	u := fmt.Sprintf("%s/books/?search=%s", g.baseURL, url.QueryEscape(q.Text))

	var resp gutendexResponse
	if err := getJSON(ctx, g.client, u, &resp); err != nil {
		return nil, fmt.Errorf("gutendex search: %w", err)
	}

	books := make([]Book, 0, len(resp.Results))
	for _, r := range resp.Results {
		if b := g.toBook(r); b != nil {
			books = append(books, *b)
		}
	}
	return books, nil
}

func (g *GutenbergSource) Resolve(ctx context.Context, id string) (*Book, error) {
	u := fmt.Sprintf("%s/books/%s/", g.baseURL, url.PathEscape(id))

	var r gutendexBook
	if err := getJSON(ctx, g.client, u, &r); err != nil {
		return nil, fmt.Errorf("gutendex lookup of %q: %w", id, err)
	}

	b := g.toBook(r)
	if b == nil {
		return nil, fmt.Errorf("book %q has no downloadable ebook format", id)
	}
	return b, nil
}

func (g *GutenbergSource) toBook(r gutendexBook) *Book {
	dl, ext := pickGutenbergFormat(r.Formats, g.DownloadHosts())
	if dl == "" {
		return nil
	}
	// Gutendex only indexes public-domain texts, but the field exists, so we
	// respect it rather than assume.
	if r.Copyright != nil && *r.Copyright {
		return nil
	}

	authors := make([]string, 0, len(r.Authors))
	for _, a := range r.Authors {
		authors = append(authors, flipName(a.Name))
	}

	lang := ""
	if len(r.Languages) > 0 {
		lang = r.Languages[0]
	}

	note := ""
	if r.DownloadCount > 0 {
		note = fmt.Sprintf("%s downloads on Gutenberg", humanCount(r.DownloadCount))
	}

	return &Book{
		Source:      g.Name(),
		ID:          fmt.Sprint(r.ID),
		Title:       strings.TrimSpace(r.Title),
		Authors:     authors,
		Language:    lang,
		CoverURL:    r.Formats["image/jpeg"],
		Note:        note,
		downloadURL: dl,
		ext:         ext,
	}
}

// pickGutenbergFormat prefers EPUB (what Stump reads best), then falls back
// through the other formats Stump can open.
func pickGutenbergFormat(formats map[string]string, hosts []string) (string, string) {
	preferred := []struct{ mime, ext string }{
		{"application/epub+zip", ".epub"},
		{"application/x-mobipocket-ebook", ".mobi"},
		{"text/plain; charset=utf-8", ".txt"},
	}
	for _, p := range preferred {
		for mime, link := range formats {
			if !strings.HasPrefix(mime, p.mime) {
				continue
			}
			// Gutendex lists .zip variants alongside the plain file; skip them
			// so we never drop an archive into the library.
			if strings.HasSuffix(link, ".zip") {
				continue
			}
			if isAllowedDownloadHost(link, hosts) {
				return link, p.ext
			}
		}
	}
	return "", ""
}

// flipName turns Gutenberg's "Stoker, Bram" into "Bram Stoker", leaving names
// that aren't in that form alone.
func flipName(name string) string {
	name = strings.TrimSpace(name)
	last, first, ok := strings.Cut(name, ", ")
	if !ok || strings.Contains(first, ",") {
		return name
	}
	return strings.TrimSpace(first) + " " + strings.TrimSpace(last)
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprint(n)
	}
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
