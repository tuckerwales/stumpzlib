package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const userAgent = "stumpzlib/1.0 (+https://github.com/stumpapp/stump)"

// isAllowedDownloadHost gates every file fetch against the declaring source's
// host list. Sources resolve URLs server-side, but this is the backstop: if a
// catalog API is ever compromised or spoofed, it still cannot make this
// process fetch from an arbitrary host.
//
// Plain HTTP is permitted only for loopback, so a local mirror works in
// development without opening the door to cleartext fetches from the network.
func isAllowedDownloadHost(rawURL string, allowed []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())

	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(host) {
			return false
		}
	default:
		return false
	}

	for _, a := range allowed {
		if host == strings.ToLower(a) {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Downloader writes books into the directory Stump scans.
type Downloader struct {
	libraryPath string
	maxBytes    int64
	client      *http.Client
}

// ErrAlreadyExists means the book is already sitting in the library.
var ErrAlreadyExists = errors.New("already in library")

// Save streams a book into the library directory. It downloads to a .part file
// and renames on success, so Stump's scanner never sees a half-written book.
func (d *Downloader) Save(ctx context.Context, b *Book, allowedHosts []string) (string, error) {
	if !isAllowedDownloadHost(b.downloadURL, allowedHosts) {
		return "", fmt.Errorf("refusing to download from disallowed host: %s", b.downloadURL)
	}

	name := buildFilename(b)
	dest, err := d.safeJoin(name)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(dest); err == nil {
		return filepath.Base(dest), ErrAlreadyExists
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range b.downloadHeaders {
		if k == "" || v == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %s: %w", b.downloadURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("source returned HTTP %d for %s", resp.StatusCode, b.downloadURL)
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return "", fmt.Errorf("source returned a web page instead of a file — the download limit may have been reached, or the server blocked the request")
	}
	if resp.ContentLength > d.maxBytes {
		return "", fmt.Errorf("file is %d bytes, over the %d byte limit", resp.ContentLength, d.maxBytes)
	}

	tmp, err := os.CreateTemp(d.libraryPath, ".stumpzlib-*.part")
	if err != nil {
		return "", fmt.Errorf("creating temp file in library: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once the rename below has succeeded
	}()

	// Peek at the first bytes so an HTML quota/interstitial page is rejected
	// even when the server lies about Content-Type.
	prefix := make([]byte, 512)
	n, readErr := resp.Body.Read(prefix)
	if n == 0 && readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", fmt.Errorf("reading download: %w", readErr)
	}
	prefix = prefix[:n]
	if looksLikeHTML(prefix) {
		return "", fmt.Errorf("source returned a web page instead of a file — the download limit may have been reached, or the server blocked the request")
	}
	body := io.MultiReader(bytes.NewReader(prefix), resp.Body)
	if errors.Is(readErr, io.EOF) {
		body = bytes.NewReader(prefix)
	}

	// +1 so we can tell "exactly at the limit" from "truncated at the limit".
	written, err := io.Copy(tmp, io.LimitReader(body, d.maxBytes+1))
	if err != nil {
		return "", fmt.Errorf("writing download: %w", err)
	}
	if written > d.maxBytes {
		return "", fmt.Errorf("file exceeds the %d byte download limit", d.maxBytes)
	}
	if written == 0 {
		return "", errors.New("source returned an empty file")
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing download: %w", err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("moving download into library: %w", err)
	}
	// Temp files are 0600; make it readable by the Stump process.
	_ = os.Chmod(dest, 0o644)

	return filepath.Base(dest), nil
}

// safeJoin places name directly inside libraryPath, rejecting anything that
// would escape it. Titles come from a remote API, so this is not optional.
func (d *Downloader) safeJoin(name string) (string, error) {
	dest := filepath.Join(d.libraryPath, name)
	if filepath.Dir(dest) != d.libraryPath {
		return "", fmt.Errorf("refusing to write outside the library: %q", name)
	}
	return dest, nil
}

// buildFilename produces "Author - Title.epub", stripped of anything that
// would be awkward or unsafe on disk.
func buildFilename(b *Book) string {
	title := sanitize(b.Title)
	if title == "" {
		title = "untitled"
	}

	name := title
	if len(b.Authors) > 0 {
		if author := sanitize(b.Authors[0]); author != "" {
			name = author + " - " + title
		}
	}

	// Truncate by runes, not bytes, so a long non-ASCII title can't be cut
	// mid-character.
	if r := []rune(name); len(r) > 150 {
		name = strings.TrimRight(string(r[:150]), " .-")
	}

	ext := b.ext
	if ext == "" {
		ext = ".epub"
	}
	return name + ext
}

// sanitize keeps letters, digits, spaces and a small set of safe punctuation,
// which rules out path separators, NUL, and control characters by construction.
func looksLikeHTML(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return false
	}
	lower := bytes.ToLower(trimmed)
	return bytes.HasPrefix(lower, []byte("<!doctype html")) ||
		bytes.HasPrefix(lower, []byte("<html"))
}

func sanitize(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			out.WriteRune(r)
		case r == ' ', r == '-', r == '_', r == ',', r == '\'', r == '(', r == ')':
			out.WriteRune(r)
		case r == '\n', r == '\r', r == '\t':
			out.WriteRune(' ')
		}
	}
	// Collapse runs of whitespace and trim leading dots/dashes so we never
	// produce a hidden file or something that looks like a flag.
	cleaned := strings.Join(strings.Fields(out.String()), " ")
	return strings.Trim(cleaned, " .-")
}
