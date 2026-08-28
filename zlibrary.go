package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The Z-library HTTP client is a Go port of the protocol used by
// https://github.com/ZlibraryKO/zlibrary.koplugin (the KOReader plugin).
// Endpoints, session cookies, search form fields, download-link resolution,
// health checks, and bot-challenge handling follow that client.

//go:embed zlibrary_domains.json
var embeddedZlibDomains []byte

const (
	zlibUserAgent    = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/96.0.4664.110 Safari/537.36"
	zlibSearchLimit  = 30
	zlibMaxRedirects = 5
	blockedMirrorTTL = 180 * 24 * time.Hour
	zlibProbeTimeout = 8 * time.Second
	zlibMaxBody      = 8 << 20
)

var zlibSeedURLs = []string{
	"https://z-lib.fo",
	"https://library-oceania.sk",
	"https://library-latin.sk",
	"https://z-lib.fm",
	"https://library-asia.sk",
	"https://lib-africa.sk",
	"https://z-library.do",
	"https://z-lib.gd",
	"https://1lib.sk",
	"https://z-lib.gl",
	"https://z-library.rs",
	"https://z-lib.do",
	"https://z-lib.gs",
}

var zlibDomainCDNs = []string{
	"https://fastly.jsdelivr.net/gh/ZlibraryKO/zlibrary.koplugin@main/assets/domains.json",
	"https://cdn.jsdelivr.net/gh/ZlibraryKO/zlibrary.koplugin@main/assets/domains.json",
	"https://raw.githubusercontent.com/ZlibraryKO/zlibrary.koplugin/main/assets/domains.json",
}

var zlibChallengeMarkers = []string{
	"Verifying your browser",
	"DiamWall",
	"/cdn-cgi/mitigation/",
	"__cf_chl",
	"Just a moment",
	"Checking your browser",
}

var zlibLanguageOpts = []namedOption{
	{Name: "English", Value: "english"},
	{Name: "Español", Value: "spanish"},
	{Name: "Français", Value: "french"},
	{Name: "Deutsch", Value: "german"},
	{Name: "Português", Value: "portuguese"},
	{Name: "Brazilian Portuguese", Value: "brazilian"},
	{Name: "Italiano", Value: "italian"},
	{Name: "Русский", Value: "russian"},
	{Name: "简体中文", Value: "chinese"},
	{Name: "繁體中文", Value: "traditional chinese"},
	{Name: "日本語", Value: "japanese"},
	{Name: "한국어", Value: "korean"},
	{Name: "العربية", Value: "arabic"},
	{Name: "हिन्दी", Value: "hindi"},
	{Name: "Nederlands", Value: "dutch"},
	{Name: "Polski", Value: "polish"},
	{Name: "Türkçe", Value: "turkish"},
	{Name: "Українська", Value: "ukrainian"},
	{Name: "Tiếng Việt", Value: "vietnamese"},
	{Name: "Bahasa Indonesia", Value: "indonesian"},
	{Name: "Svenska", Value: "swedish"},
	{Name: "Norsk", Value: "norwegian"},
	{Name: "Dansk", Value: "danish"},
	{Name: "Suomi", Value: "finnish"},
	{Name: "Čeština", Value: "czech"},
	{Name: "Română", Value: "romanian"},
	{Name: "Magyar", Value: "hungarian"},
	{Name: "Ελληνικά", Value: "greek"},
	{Name: "עברית", Value: "hebrew"},
	{Name: "ไทย", Value: "thai"},
	{Name: "فارسی", Value: "persian"},
	{Name: "Latin", Value: "latin"},
}

var zlibExtensionOpts = []namedOption{
	{Name: "EPUB", Value: "EPUB"},
	{Name: "PDF", Value: "PDF"},
	{Name: "MOBI", Value: "MOBI"},
	{Name: "AZW3", Value: "AZW3"},
	{Name: "AZW", Value: "AZW"},
	{Name: "FB2", Value: "FB2"},
	{Name: "DJVU", Value: "DJVU"},
	{Name: "DJV", Value: "DJV"},
	{Name: "CBZ", Value: "CBZ"},
	{Name: "TXT", Value: "TXT"},
	{Name: "RTF", Value: "RTF"},
	{Name: "LIT", Value: "LIT"},
}

var zlibOrderOpts = []namedOption{
	{Name: "Best match", Value: "bestmatch"},
	{Name: "Most popular", Value: "popular"},
	{Name: "Recently added", Value: "date"},
	{Name: "Title (A-Z)", Value: "titleA"},
	{Name: "Title (Z-A)", Value: "title"},
	{Name: "Year", Value: "year"},
	{Name: "File size ↓", Value: "filesize"},
	{Name: "File size ↑", Value: "filesizeA"},
}

type namedOption struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ZLibrarySource searches and downloads from a Z-library mirror via the eAPI.
type ZLibrarySource struct {
	settings *SettingsStore
	http     *http.Client
	probe    *http.Client

	mu      sync.Mutex
	realURL string // last cross-host redirect origin; preferred over the saved base
	loginMu sync.Mutex

	// seeds, if set, replaces the built-in mirror list. Tests use this so
	// discovery can run against httptest servers instead of the public net.
	seeds []string
}

func NewZLibrarySource(settings *SettingsStore) *ZLibrarySource {
	jar, _ := cookiejar.New(nil)
	noFollow := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &ZLibrarySource{
		settings: settings,
		http: &http.Client{
			Jar:           jar,
			CheckRedirect: noFollow,
		},
		probe: &http.Client{
			Timeout:       zlibProbeTimeout,
			CheckRedirect: noFollow,
		},
	}
}

func (z *ZLibrarySource) Name() string  { return "zlibrary" }
func (z *ZLibrarySource) Label() string { return "Z-library" }

func (z *ZLibrarySource) DownloadHosts() []string {
	var hosts []string
	add := func(raw string) {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return
		}
		host := strings.ToLower(u.Hostname())
		for _, h := range hosts {
			if h == host {
				return
			}
		}
		hosts = append(hosts, host)
	}
	add(z.base())
	add(z.settings.GetZLibrary().BaseURL)
	return hosts
}

func (z *ZLibrarySource) Search(ctx context.Context, q SearchQuery) ([]Book, error) {
	if z.base() == "" {
		return nil, fmt.Errorf("Z-library server URL is not set — open Settings and enter a base URL, or run auto-discover")
	}

	cfg := z.settings.GetZLibrary()
	switch q.List {
	case "popular":
		return z.listBooks(ctx, "/eapi/book/most-popular", false)
	case "recommended":
		if !cfg.HasCredentials() {
			return nil, fmt.Errorf("recommended books require a Z-library account — set email and password on the Settings page")
		}
		return z.listBooks(ctx, "/eapi/user/book/recommended", true)
	case "":
	default:
		return nil, fmt.Errorf("unknown list %q", q.List)
	}

	if strings.TrimSpace(q.Text) == "" {
		return nil, fmt.Errorf("a search query is required")
	}
	if len(q.Languages) == 0 {
		q.Languages = cfg.Languages
	}
	if len(q.Extensions) == 0 {
		q.Extensions = cfg.Extensions
	}
	if q.Order == "" {
		q.Order = cfg.Order
	}
	return z.searchBooks(ctx, q)
}

func (z *ZLibrarySource) Resolve(ctx context.Context, id string) (*Book, error) {
	bookID, hash, err := parseZLibID(id)
	if err != nil {
		return nil, err
	}

	var book *Book
	err = z.withAuth(ctx, true, func() error {
		details, err := z.getBookDetails(ctx, bookID, hash)
		if err != nil {
			return err
		}
		link, ext, err := z.getDownloadLink(ctx, bookID, hash)
		if err != nil {
			return err
		}
		u, err := url.Parse(link)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("Z-library returned an unusable download link")
		}
		if u.Scheme != "https" && !isLoopback(u.Hostname()) {
			return fmt.Errorf("refusing non-HTTPS download link from Z-library")
		}

		details.downloadURL = link
		if ext != "" {
			details.ext = bookExt(ext)
		}
		if details.ext == "" {
			details.ext = ".epub"
		}
		details.downloadHeaders = z.fileHeaders(details.href)
		details.downloadHosts = []string{strings.ToLower(u.Hostname())}
		book = details
		return nil
	})
	return book, err
}

// --- search / lists --------------------------------------------------------

func (z *ZLibrarySource) searchBooks(ctx context.Context, q SearchQuery) ([]Book, error) {
	var books []Book
	err := z.withAuth(ctx, false, func() error {
		body := encodeSearchBody(q)
		res, err := z.api(ctx, http.MethodPost, "/eapi/book/search", body, z.optionalAuthHeaders(), true)
		if err != nil {
			return err
		}
		parsed, err := decodeZlibJSON(res.body)
		if err != nil {
			return fmt.Errorf("invalid search response from Z-library")
		}
		if msg := parsed.errorMessage(); msg != "" {
			return fmt.Errorf("search failed: %s", msg)
		}
		rawBooks := parsed.books()
		books = z.transformBooks(rawBooks)
		return nil
	})
	return books, err
}

func (z *ZLibrarySource) listBooks(ctx context.Context, path string, needAuth bool) ([]Book, error) {
	var books []Book
	err := z.withAuth(ctx, needAuth, func() error {
		res, err := z.api(ctx, http.MethodGet, path, "", z.optionalAuthHeaders(), true)
		if err != nil {
			return err
		}
		parsed, err := decodeZlibJSON(res.body)
		if err != nil {
			return fmt.Errorf("invalid list response from Z-library")
		}
		if !parsed.successFlag() || parsed.Books == nil {
			if msg := parsed.errorMessage(); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("Z-library returned no books")
		}
		books = z.transformBooks(parsed.Books)
		return nil
	})
	return books, err
}

func encodeSearchBody(q SearchQuery) string {
	var parts []string
	parts = append(parts, "message="+url.QueryEscape(q.Text))
	parts = append(parts, "page=1")
	parts = append(parts, "limit="+strconv.Itoa(zlibSearchLimit))
	for i, lang := range q.Languages {
		if lang = strings.TrimSpace(lang); lang == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("languages[%d]=%s", i, url.QueryEscape(lang)))
	}
	for i, ext := range q.Extensions {
		if ext = strings.TrimSpace(ext); ext == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("extensions[%d]=%s", i, url.QueryEscape(ext)))
	}
	if o := strings.TrimSpace(q.Order); o != "" {
		parts = append(parts, "order="+url.QueryEscape(o))
	}
	return strings.Join(parts, "&")
}

func (z *ZLibrarySource) transformBooks(raw []zlibBook) []Book {
	out := make([]Book, 0, len(raw))
	for _, b := range raw {
		if book := z.toBook(b); book != nil {
			out = append(out, *book)
		}
	}
	return out
}

func (z *ZLibrarySource) toBook(b zlibBook) *Book {
	id := stringifyJSON(b.ID)
	hash := strings.TrimSpace(b.Hash)
	if id == "" || hash == "" {
		return nil
	}
	title := strings.TrimSpace(b.Title)
	if title == "" {
		title = "Unknown Title"
	}

	noteParts := make([]string, 0, 4)
	if ext := strings.TrimSpace(b.Extension); ext != "" && ext != "N/A" {
		noteParts = append(noteParts, ext)
	}
	if sz := strings.TrimSpace(b.FilesizeString); sz != "" && sz != "N/A" {
		noteParts = append(noteParts, sz)
	}
	if year := stringifyJSON(b.Year); year != "" && year != "0" && year != "N/A" {
		noteParts = append(noteParts, year)
	}

	lang := strings.TrimSpace(b.Language)
	if lang == "N/A" {
		lang = ""
	}

	return &Book{
		Source:   z.Name(),
		ID:       id + ":" + hash,
		Title:    title,
		Authors:  zlibAuthors(b.Author),
		Language: lang,
		CoverURL: usableCoverURL(b.Cover),
		Note:     strings.Join(noteParts, " · "),
		href:     strings.TrimSpace(b.Href),
		ext:      bookExt(b.Extension),
	}
}

func zlibAuthors(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || s == "Unknown Author" || s == "N/A" {
		return nil
	}
	return []string{s}
}

func bookExt(format string) string {
	format = strings.TrimSpace(format)
	if format == "" || format == "N/A" {
		return ""
	}
	if len(format) > 8 {
		return ""
	}
	for _, r := range format {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return ""
		}
	}
	return "." + strings.ToLower(format)
}

func usableCoverURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return s
}

func parseZLibID(id string) (bookID, hash string, err error) {
	id = strings.TrimSpace(id)
	bookID, hash, ok := strings.Cut(id, ":")
	if !ok || bookID == "" || hash == "" {
		return "", "", fmt.Errorf("invalid Z-library book id")
	}
	for _, r := range hash {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return "", "", fmt.Errorf("invalid Z-library book id")
	}
	return bookID, hash, nil
}

// --- login / session -------------------------------------------------------

func (z *ZLibrarySource) withAuth(ctx context.Context, require bool, fn func() error) error {
	cfg := z.settings.GetZLibrary()
	if require {
		if err := z.ensureLogin(ctx); err != nil {
			return err
		}
	} else if cfg.HasCredentials() && !cfg.HasSession() {
		// Search works signed-out; a stored password is used only if the
		// server actually demands a session.
		_ = z.ensureLogin(ctx)
	}

	err := fn()
	if err != nil && isZlibAuthError(err) && z.settings.GetZLibrary().HasCredentials() {
		z.clearSession()
		if err2 := z.ensureLogin(ctx); err2 != nil {
			return err2
		}
		return fn()
	}
	return err
}

func (z *ZLibrarySource) ensureLogin(ctx context.Context) error {
	z.loginMu.Lock()
	defer z.loginMu.Unlock()

	cur := z.settings.GetZLibrary()
	if cur.HasSession() {
		return nil
	}
	if !cur.HasCredentials() {
		return fmt.Errorf("Z-library email and password are not set — open Settings and save them")
	}
	return z.loginLocked(ctx, cur)
}

func (z *ZLibrarySource) loginLocked(ctx context.Context, cur ZLibrarySettings) error {
	body := "email=" + url.QueryEscape(cur.Email) + "&password=" + url.QueryEscape(cur.Password)
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	hdr.Set("X-Requested-With", "XMLHttpRequest")

	res, err := z.api(ctx, http.MethodPost, "/eapi/user/login", body, hdr, true)
	if err != nil {
		return err
	}
	if len(res.body) == 0 {
		return fmt.Errorf("login failed: empty response from server")
	}

	parsed, err := decodeZlibJSON(res.body)
	if err != nil {
		return fmt.Errorf("login failed: invalid response format")
	}

	session := parsed.User
	if session == nil {
		session = parsed.Response
	}

	if !parsed.successFlag() {
		if msg := parsed.errorMessage(); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		if session != nil {
			if msg := stringifyAny(session["message"]); msg != "" {
				return fmt.Errorf("%s", msg)
			}
		}
		return fmt.Errorf("credentials rejected or invalid response")
	}
	if session == nil {
		return fmt.Errorf("login failed: invalid session data")
	}

	userID := firstString(session, "id", "user_id")
	userKey := firstString(session, "remix_userkey", "user_key")
	if userID == "" || userKey == "" {
		if msg := firstString(session, "message"); msg != "" {
			return fmt.Errorf("login failed: %s", msg)
		}
		return fmt.Errorf("credentials rejected or invalid response")
	}

	cur.UserID = userID
	cur.UserKey = userKey
	return z.settings.SetZLibrary(cur)
}

func (z *ZLibrarySource) clearSession() {
	cur := z.settings.GetZLibrary()
	if cur.UserID == "" && cur.UserKey == "" {
		return
	}
	cur.UserID = ""
	cur.UserKey = ""
	_ = z.settings.SetZLibrary(cur)
}

func (z *ZLibrarySource) Login(ctx context.Context) error {
	z.loginMu.Lock()
	defer z.loginMu.Unlock()
	cur := z.settings.GetZLibrary()
	if !cur.HasCredentials() {
		return fmt.Errorf("Z-library email and password are not set")
	}
	cur.UserID = ""
	cur.UserKey = ""
	return z.loginLocked(ctx, cur)
}

func (z *ZLibrarySource) Quota(ctx context.Context) (today, limit int, err error) {
	err = z.withAuth(ctx, true, func() error {
		res, err := z.api(ctx, http.MethodGet, "/eapi/user/profile", "", z.authHeaders(), true)
		if err != nil {
			return err
		}
		parsed, err := decodeZlibJSON(res.body)
		if err != nil {
			return fmt.Errorf("invalid profile response from Z-library")
		}
		if !parsed.successFlag() || parsed.User == nil {
			if msg := parsed.errorMessage(); msg != "" {
				return fmt.Errorf("%s", msg)
			}
			return fmt.Errorf("could not read download quota")
		}
		today = intFromAny(parsed.User["downloads_today"])
		limit = intFromAny(parsed.User["downloads_limit"])
		return nil
	})
	return today, limit, err
}

func (z *ZLibrarySource) authHeaders() http.Header {
	cfg := z.settings.GetZLibrary()
	h := make(http.Header)
	h.Set("Content-Type", "application/x-www-form-urlencoded")
	if cfg.HasSession() {
		h.Set("Cookie", fmt.Sprintf("remix_userid=%s; remix_userkey=%s", cfg.UserID, cfg.UserKey))
	}
	return h
}

func (z *ZLibrarySource) optionalAuthHeaders() http.Header {
	cfg := z.settings.GetZLibrary()
	if !cfg.HasSession() {
		return nil
	}
	return z.authHeaders()
}

func (z *ZLibrarySource) fileHeaders(href string) map[string]string {
	cfg := z.settings.GetZLibrary()
	h := map[string]string{
		"User-Agent": zlibUserAgent,
	}
	if cfg.HasSession() {
		h["Cookie"] = fmt.Sprintf("remix_userid=%s; remix_userkey=%s", cfg.UserID, cfg.UserKey)
	}
	if href != "" {
		if u := z.bookPageURL(href); u != "" {
			h["Referer"] = u
		}
	}
	return h
}

func (z *ZLibrarySource) bookPageURL(href string) string {
	base := z.base()
	if base == "" || href == "" {
		return ""
	}
	if !strings.HasPrefix(href, "/") {
		href = "/" + href
	}
	return base + href
}

// --- book details / download link ------------------------------------------

func (z *ZLibrarySource) getBookDetails(ctx context.Context, id, hash string) (*Book, error) {
	path := fmt.Sprintf("/eapi/book/%s/%s", url.PathEscape(id), url.PathEscape(hash))
	res, err := z.api(ctx, http.MethodGet, path, "", z.authHeaders(), true)
	if err != nil {
		return nil, err
	}
	parsed, err := decodeZlibJSON(res.body)
	if err != nil {
		return nil, fmt.Errorf("invalid book details response from Z-library")
	}
	if !parsed.successFlag() || parsed.Book == nil {
		if msg := parsed.errorMessage(); msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, fmt.Errorf("could not fetch book details")
	}
	book := z.toBook(*parsed.Book)
	if book == nil {
		return nil, fmt.Errorf("could not process book details")
	}
	return book, nil
}

func (z *ZLibrarySource) getDownloadLink(ctx context.Context, id, hash string) (link, ext string, err error) {
	path := fmt.Sprintf("/eapi/book/%s/%s/file", url.PathEscape(id), url.PathEscape(hash))
	res, err := z.api(ctx, http.MethodGet, path, "", z.authHeaders(), true)
	if err != nil {
		return "", "", err
	}
	parsed, err := decodeZlibJSON(res.body)
	if err != nil {
		return "", "", fmt.Errorf("invalid download-link response from Z-library")
	}
	if !parsed.successFlag() || parsed.File == nil {
		if msg := parsed.errorMessage(); msg != "" {
			return "", "", fmt.Errorf("%s", msg)
		}
		return "", "", fmt.Errorf("could not fetch the download link")
	}
	file := parsed.File
	link = strings.TrimSpace(file.DownloadLink)
	if link == "" {
		if file.AllowDownload != nil && !*file.AllowDownload {
			return "", "", fmt.Errorf("download limit reached. Please try again later or check your account")
		}
		return "", "", fmt.Errorf("no download link provided in API response")
	}
	return link, file.Extension, nil
}

// --- HTTP ------------------------------------------------------------------

type apiResult struct {
	status int
	header http.Header
	body   []byte
}

func (z *ZLibrarySource) base() string {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.realURL != "" {
		return z.realURL
	}
	return strings.TrimRight(z.settings.GetZLibrary().BaseURL, "/")
}

func (z *ZLibrarySource) pin(origin string) {
	origin = strings.TrimRight(origin, "/")
	z.mu.Lock()
	z.realURL = origin
	z.mu.Unlock()
}

func (z *ZLibrarySource) clearPin() {
	z.mu.Lock()
	z.realURL = ""
	z.mu.Unlock()
}

func (z *ZLibrarySource) pinMatches(rawURL string) bool {
	z.mu.Lock()
	pinned := z.realURL
	z.mu.Unlock()
	if pinned == "" {
		return false
	}
	return originString(rawURL) == originString(pinned)
}

func (z *ZLibrarySource) api(ctx context.Context, method, path, body string, hdr http.Header, pin bool) (apiResult, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	currentURL := ""
	currentMethod := method
	currentBody := body
	seen := map[string]int{}
	var zero apiResult

	for hop := 0; hop <= zlibMaxRedirects; hop++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if currentURL == "" {
			base := z.base()
			if base == "" {
				return zero, fmt.Errorf("Z-library server URL is not set")
			}
			currentURL = base + path
		}
		if seen[currentURL] > 1 {
			return zero, fmt.Errorf("too many redirects (%s)", hostOf(currentURL))
		}
		seen[currentURL]++

		res, err := z.roundTrip(ctx, z.http, currentMethod, currentURL, currentBody, hdr)
		if err != nil {
			if pin && z.pinMatches(currentURL) {
				z.clearPin()
			}
			return zero, err
		}

		if looksLikeBotChallenge(res.body) {
			z.markBlocked(currentURL)
			return zero, fmt.Errorf("this Z-library server is refusing automated access (%s). Try a different server via auto-discover", hostOf(currentURL))
		}

		if isHTTPRedirect(res.status) {
			loc := headerValue(res.header, "Location")
			if loc == "" {
				return zero, fmt.Errorf("redirect from %s carried no Location header", currentURL)
			}
			next, err := resolveURL(currentURL, loc)
			if err != nil {
				return zero, err
			}
			from, _ := url.Parse(currentURL)
			if !strings.EqualFold(next.Host, from.Host) {
				if pin {
					z.pin(originOf(next))
				}
				// Rebuild the original API path on the new origin. Following
				// the Location blindly can drop /eapi/... on mirror hops.
				currentURL = ""
				currentMethod = method
				currentBody = body
				if !pin {
					currentURL = strings.TrimRight(originOf(next), "/") + path
				}
				continue
			}
			if res.status == http.StatusMovedPermanently || res.status == http.StatusFound || res.status == http.StatusSeeOther {
				if strings.ToUpper(currentMethod) != http.MethodGet {
					currentMethod = http.MethodGet
					currentBody = ""
				}
			}
			currentURL = next.String()
			continue
		}

		if res.status < 200 || res.status >= 300 {
			if pin && res.status >= 500 && z.pinMatches(currentURL) {
				z.clearPin()
			}
			if msg := jsonErrorMessage(res.body); msg != "" {
				return res, fmt.Errorf("%s", msg)
			}
			return res, fmt.Errorf("Z-library returned HTTP %d", res.status)
		}
		return res, nil
	}
	return zero, fmt.Errorf("too many redirects")
}

func (z *ZLibrarySource) roundTrip(ctx context.Context, client *http.Client, method, rawURL, body string, hdr http.Header) (apiResult, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return apiResult{}, err
	}
	req.Header.Set("User-Agent", zlibUserAgent)
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	if body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}

	resp, err := client.Do(req)
	if err != nil {
		return apiResult{}, fmt.Errorf("connecting to Z-library: %w", err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, zlibMaxBody))
	if err != nil {
		return apiResult{}, fmt.Errorf("reading Z-library response: %w", err)
	}
	return apiResult{status: resp.StatusCode, header: resp.Header, body: payload}, nil
}

func looksLikeBotChallenge(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	head := body
	if len(head) > 4096 {
		head = head[:4096]
	}
	if !strings.Contains(string(head), "<") {
		return false
	}
	s := string(head)
	for _, m := range zlibChallengeMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func isHTTPRedirect(code int) bool {
	switch code {
	case 301, 302, 303, 307, 308:
		return true
	}
	return false
}

func isZlibAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Please login") || strings.Contains(s, "Incorrect email or password")
}

func isZlibCredentialRejection(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Incorrect email or password") ||
		strings.Contains(s, "Please login") ||
		strings.Contains(s, "credentials rejected")
}

// --- discovery -------------------------------------------------------------

func (z *ZLibrarySource) Discover(ctx context.Context) (string, error) {
	seeds := z.seedURLs()
	if len(seeds) == 0 {
		return "", fmt.Errorf("no Z-library mirrors left to try")
	}

	type hit struct {
		url     string
		elapsed time.Duration
	}
	var mu sync.Mutex
	var healthy []hit

	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, seed := range seeds {
		seed := seed
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			start := time.Now()
			if err := z.probeHealth(ctx, seed); err != nil {
				return
			}
			elapsed := time.Since(start)
			mu.Lock()
			healthy = append(healthy, hit{url: seed, elapsed: elapsed})
			mu.Unlock()
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(healthy) == 0 {
		return "", fmt.Errorf("no working Z-library server found")
	}

	sort.Slice(healthy, func(i, j int) bool {
		return healthy[i].elapsed < healthy[j].elapsed
	})

	var verified []hit
	for _, h := range healthy {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := z.probeSearch(ctx, h.url); err != nil {
			continue
		}
		verified = append(verified, h)
		if len(verified) >= 5 {
			break
		}
	}
	if len(verified) == 0 {
		return "", fmt.Errorf("mirrors answered but blocked search — try again, or set a base URL by hand")
	}

	chosen := verified[rand.Intn(len(verified))].url
	if err := z.setBaseURL(chosen); err != nil {
		return "", err
	}
	return chosen, nil
}

func (z *ZLibrarySource) setBaseURL(raw string) error {
	clean, err := validateZLibraryBaseURL(raw)
	if err != nil {
		return err
	}
	if clean == "" {
		return fmt.Errorf("URL cannot be empty")
	}
	cur := z.settings.GetZLibrary()
	if cur.BaseURL != clean {
		cur.UserID = ""
		cur.UserKey = ""
	}
	cur.BaseURL = clean
	if err := z.settings.SetZLibrary(cur); err != nil {
		return err
	}
	z.clearPin()
	return nil
}

func (z *ZLibrarySource) seedURLs() []string {
	if len(z.seeds) > 0 {
		out := make([]string, 0, len(z.seeds))
		for _, s := range z.seeds {
			if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" {
				out = append(out, s)
			}
		}
		return out
	}

	current := strings.TrimRight(z.base(), "/")
	seen := map[string]bool{}
	if current != "" {
		seen[current] = true
	}

	var out, blocked []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if strings.HasSuffix(strings.ToLower(raw), ".onion") {
			return
		}
		if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
			raw = "https://" + raw
		}
		raw = strings.TrimRight(raw, "/")
		if seen[raw] {
			return
		}
		seen[raw] = true
		if z.isBlocked(raw) {
			blocked = append(blocked, raw)
			return
		}
		out = append(out, raw)
	}

	for _, u := range zlibSeedURLs {
		add(u)
	}
	for _, u := range z.dynamicDomains() {
		add(u)
	}

	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	if len(out) == 0 {
		rand.Shuffle(len(blocked), func(i, j int) { blocked[i], blocked[j] = blocked[j], blocked[i] })
		return blocked
	}
	return out
}

func (z *ZLibrarySource) dynamicDomains() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	for _, cdn := range zlibDomainCDNs {
		domains, err := fetchDomainList(ctx, z.probe, cdn)
		if err == nil && len(domains) > 0 {
			return domains
		}
	}
	domains, _ := parseDomainList(embeddedZlibDomains)
	return domains
}

func fetchDomainList(ctx context.Context, _ *http.Client, rawURL string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", zlibUserAgent)
	req.Header.Set("Accept", "application/json")
	// Follow redirects: the domain list lives on CDNs that 30x onto a blob URL.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseDomainList(body)
}

func parseDomainList(body []byte) ([]string, error) {
	var parsed struct {
		Domains []struct {
			Domain string `json:"domain"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Domains))
	for _, d := range parsed.Domains {
		if d.Domain == "" || strings.HasSuffix(d.Domain, ".onion") {
			continue
		}
		out = append(out, d.Domain)
	}
	return out, nil
}

func (z *ZLibrarySource) probeHealth(ctx context.Context, base string) error {
	pctx, cancel := context.WithTimeout(ctx, zlibProbeTimeout)
	defer cancel()
	body, err := z.probeGET(pctx, strings.TrimRight(base, "/")+"/eapi/info/ok")
	if err != nil {
		return err
	}
	parsed, err := decodeZlibJSON(body)
	if err != nil {
		return fmt.Errorf("invalid JSON")
	}
	if !parsed.successFlag() {
		return fmt.Errorf("invalid API response")
	}
	return nil
}

func (z *ZLibrarySource) probeSearch(ctx context.Context, base string) error {
	pctx, cancel := context.WithTimeout(ctx, zlibProbeTimeout)
	defer cancel()
	rawURL := strings.TrimRight(base, "/") + "/eapi/book/search"
	hdr := make(http.Header)
	hdr.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	res, err := z.roundTrip(pctx, z.probe, http.MethodPost, rawURL, "message=test&page=1&limit=1", hdr)
	if err != nil {
		return err
	}
	if looksLikeBotChallenge(res.body) {
		z.markBlocked(rawURL)
		return fmt.Errorf("bot challenge")
	}
	if res.status < 200 || res.status >= 300 {
		if looksLikeBotChallenge(res.body) {
			z.markBlocked(rawURL)
			return fmt.Errorf("bot challenge")
		}
		return fmt.Errorf("HTTP %d", res.status)
	}
	if _, err := decodeZlibJSON(res.body); err != nil {
		if looksLikeBotChallenge(res.body) {
			z.markBlocked(rawURL)
			return fmt.Errorf("bot challenge")
		}
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func (z *ZLibrarySource) probeGET(ctx context.Context, start string) ([]byte, error) {
	current := start
	seen := map[string]int{}
	for hop := 0; hop <= zlibMaxRedirects; hop++ {
		if seen[current] > 1 {
			return nil, fmt.Errorf("too many redirects")
		}
		seen[current]++
		res, err := z.roundTrip(ctx, z.probe, http.MethodGet, current, "", nil)
		if err != nil {
			return nil, err
		}
		if looksLikeBotChallenge(res.body) {
			z.markBlocked(current)
			return nil, fmt.Errorf("bot challenge")
		}
		if isHTTPRedirect(res.status) {
			loc := headerValue(res.header, "Location")
			if loc == "" {
				return nil, fmt.Errorf("empty Location")
			}
			next, err := resolveURL(current, loc)
			if err != nil {
				return nil, err
			}
			current = next.String()
			continue
		}
		if res.status < 200 || res.status >= 300 {
			return nil, fmt.Errorf("HTTP %d", res.status)
		}
		return res.body, nil
	}
	return nil, fmt.Errorf("too many redirects")
}

func (z *ZLibrarySource) markBlocked(rawURL string) {
	key := mirrorKey(rawURL)
	if key == "" {
		return
	}
	cur := z.settings.GetZLibrary()
	now := time.Now().Unix()
	if cur.Blocked == nil {
		cur.Blocked = map[string]int64{}
	}
	for k, ts := range cur.Blocked {
		if now-ts >= int64(blockedMirrorTTL.Seconds()) {
			delete(cur.Blocked, k)
		}
	}
	cur.Blocked[key] = now
	_ = z.settings.SetZLibrary(cur)
}

func (z *ZLibrarySource) isBlocked(rawURL string) bool {
	key := mirrorKey(rawURL)
	if key == "" {
		return false
	}
	ts, ok := z.settings.GetZLibrary().Blocked[key]
	if !ok {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < blockedMirrorTTL
}

func mirrorKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimSpace(raw))
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return strings.ToLower(scheme + "://" + u.Host)
}

func validateZLibraryBaseURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", fmt.Errorf("Z-library URL must include a valid domain name (e.g. https://z-lib.example)")
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" || !strings.Contains(u.Hostname(), ".") {
		return "", fmt.Errorf("Z-library URL must include a valid domain name (e.g. https://z-lib.example)")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("Z-library URL must use http or https")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("Z-library URL must be an origin only, with no path")
	}
	return s, nil
}

// --- JSON shapes -----------------------------------------------------------

type zlibBook struct {
	ID             json.RawMessage `json:"id"`
	Hash           string          `json:"hash"`
	Title          string          `json:"title"`
	Author         string          `json:"author"`
	Year           json.RawMessage `json:"year"`
	Extension      string          `json:"extension"`
	FilesizeString string          `json:"filesizeString"`
	Language       string          `json:"language"`
	Href           string          `json:"href"`
	Cover          string          `json:"cover"`
}

type zlibFile struct {
	DownloadLink  string `json:"downloadLink"`
	Extension     string `json:"extension"`
	AllowDownload *bool  `json:"allowDownload"`
}

type zlibResponse struct {
	Success  any            `json:"success"`
	Error    any            `json:"error"`
	Message  string         `json:"message"`
	Books    []zlibBook     `json:"books"`
	Book     *zlibBook      `json:"book"`
	File     *zlibFile      `json:"file"`
	User     map[string]any `json:"user"`
	Response map[string]any `json:"response"`
	Exact    *struct {
		Books []zlibBook `json:"books"`
	} `json:"exactMatch"`
}

func decodeZlibJSON(body []byte) (*zlibResponse, error) {
	var parsed zlibResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (p *zlibResponse) successFlag() bool {
	return isSuccessFlag(p.Success)
}

func (p *zlibResponse) errorMessage() string {
	if p == nil {
		return ""
	}
	switch t := p.Error.(type) {
	case string:
		if t != "" {
			return t
		}
	case map[string]any:
		if m, ok := t["message"].(string); ok && m != "" {
			return m
		}
	}
	return p.Message
}

func (p *zlibResponse) books() []zlibBook {
	if p == nil {
		return nil
	}
	if len(p.Books) > 0 {
		return p.Books
	}
	if p.Exact != nil && len(p.Exact.Books) > 0 {
		return p.Exact.Books
	}
	return p.Books
}

func isSuccessFlag(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 1
	case bool:
		return t
	case string:
		return t == "1" || strings.EqualFold(t, "true")
	}
	return false
}

func jsonErrorMessage(body []byte) string {
	parsed, err := decodeZlibJSON(body)
	if err != nil {
		return ""
	}
	return parsed.errorMessage()
}

func stringifyJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strings.Trim(string(raw), `"`)
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringifyAny(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func stringifyAny(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}

func intFromAny(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(t)
		return n
	}
	return 0
}

func headerValue(h http.Header, key string) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(key))
}

func resolveURL(current, loc string) (*url.URL, error) {
	base, err := url.Parse(current)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	next := base.ResolveReference(ref)
	if next.Host == "" || next.Scheme == "" {
		return nil, fmt.Errorf("redirect target could not be resolved against %s: %s", current, loc)
	}
	return next, nil
}

func originOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func originString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return originOf(u)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}

// --- HTTP handlers ---------------------------------------------------------

type zlibrarySettingsResponse struct {
	BaseURL        string        `json:"baseUrl"`
	Email          string        `json:"email"`
	HasPassword    bool          `json:"hasPassword"`
	HasSession     bool          `json:"hasSession"`
	HasCredentials bool          `json:"hasCredentials"`
	Languages      []string      `json:"languages"`
	Extensions     []string      `json:"extensions"`
	Order          string        `json:"order"`
	LanguageOpts   []namedOption `json:"languageOpts"`
	ExtensionOpts  []namedOption `json:"extensionOpts"`
	OrderOpts      []namedOption `json:"orderOpts"`
}

func toZLibraryResponse(v ZLibrarySettings) zlibrarySettingsResponse {
	langs := v.Languages
	if langs == nil {
		langs = []string{}
	}
	exts := v.Extensions
	if exts == nil {
		exts = []string{}
	}
	return zlibrarySettingsResponse{
		BaseURL:        v.BaseURL,
		Email:          v.Email,
		HasPassword:    v.Password != "",
		HasSession:     v.HasSession(),
		HasCredentials: v.HasCredentials(),
		Languages:      langs,
		Extensions:     exts,
		Order:          v.Order,
		LanguageOpts:   zlibLanguageOpts,
		ExtensionOpts:  zlibExtensionOpts,
		OrderOpts:      zlibOrderOpts,
	}
}

func (s *Server) zlibrary() *ZLibrarySource {
	src, ok := s.sources["zlibrary"]
	if !ok {
		return nil
	}
	z, _ := src.(*ZLibrarySource)
	return z
}

func (s *Server) handleZLibrary(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, toZLibraryResponse(s.settings.GetZLibrary()))

	case http.MethodPost:
		var req struct {
			BaseURL       string   `json:"baseUrl"`
			Email         string   `json:"email"`
			Password      string   `json:"password"`
			ClearPassword bool     `json:"clearPassword"`
			Languages     []string `json:"languages"`
			Extensions    []string `json:"extensions"`
			Order         string   `json:"order"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "could not parse the request body")
			return
		}

		base, err := validateZLibraryBaseURL(req.BaseURL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		next := s.settings.GetZLibrary()
		if next.BaseURL != base {
			next.UserID = ""
			next.UserKey = ""
			if z := s.zlibrary(); z != nil {
				z.clearPin()
			}
		}
		next.BaseURL = base
		email := strings.TrimSpace(req.Email)
		if email != next.Email {
			next.UserID = ""
			next.UserKey = ""
		}
		next.Email = email
		switch {
		case req.ClearPassword:
			next.Password = ""
			next.UserID = ""
			next.UserKey = ""
		case req.Password != "":
			if req.Password != next.Password {
				next.UserID = ""
				next.UserKey = ""
			}
			next.Password = req.Password
		}
		next.Languages = cleanStringList(req.Languages)
		next.Extensions = cleanStringList(req.Extensions)
		next.Order = strings.TrimSpace(req.Order)

		if err := s.settings.SetZLibrary(next); err != nil {
			writeError(w, http.StatusInternalServerError, "saving settings: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, toZLibraryResponse(next))

	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

func (s *Server) handleZLibraryLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	z := s.zlibrary()
	if z == nil {
		writeError(w, http.StatusBadGateway, "Z-library source is not available")
		return
	}
	if z.base() == "" {
		writeError(w, http.StatusBadRequest, "set a Z-library base URL first")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), apiTimeout)
	defer cancel()

	if err := z.Login(ctx); err != nil {
		status := http.StatusBadGateway
		if isZlibCredentialRejection(err) {
			status = http.StatusUnauthorized
		}
		writeError(w, status, err.Error())
		return
	}

	today, limit, qErr := z.Quota(ctx)
	out := map[string]any{
		"ok":         true,
		"hasSession": true,
	}
	if qErr == nil {
		out["downloadsToday"] = today
		out["downloadsLimit"] = limit
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleZLibraryDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	z := s.zlibrary()
	if z == nil {
		writeError(w, http.StatusBadGateway, "Z-library source is not available")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	chosen, err := z.Discover(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"baseUrl": chosen,
		"message": "Using " + chosen,
	})
}

func cleanStringList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
