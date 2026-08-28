package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
)

// Stump 0.1.x exposes its data through GraphQL at /api/graphql. There is no
// REST /api/v1; the only REST left under /api/v2 is auth, thumbnails and a few
// per-file endpoints.
const (
	graphQLPath = "/api/graphql"
	loginPath   = "/api/v2/auth/login"
)

const librariesQuery = `query Libraries {
	libraries {
		nodes { id name path }
	}
}`

const scanLibraryMutation = `mutation ScanLibrary($id: ID!) {
	scanLibrary(id: $id)
}`

// StumpClient talks to a Stump server.
//
// Auth is either an API key (sent as a bearer token) or a username/password,
// which is exchanged for a session cookie on first use and reused after that.
//
// It reads settings live from a SettingsStore on every call rather than
// caching them, so a change saved on the Settings page takes effect on the
// next request without restarting the process.
type StumpClient struct {
	settings *SettingsStore
	client   *http.Client

	mu         sync.Mutex
	loggedIn   bool
	loggedInAs string // fingerprint of the credentials the current session is for
}

func NewStumpClient(settings *SettingsStore) *StumpClient {
	jar, _ := cookiejar.New(nil)
	return &StumpClient{
		settings: settings,
		client:   &http.Client{Jar: jar, Timeout: apiTimeout},
	}
}

// fingerprint identifies which credentials a login session belongs to, so a
// credentials change on the Settings page forces a fresh login instead of
// reusing a session established under the old ones.
func fingerprint(s StumpSettings) string {
	return strings.Join([]string{s.URL, s.APIKey, s.Username, s.Password}, "\x00")
}

type StumpLibrary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// Libraries lists the libraries on the server. Used by /api/status to confirm
// the connection works and to show the user their library ids.
func (s *StumpClient) Libraries(ctx context.Context) ([]StumpLibrary, error) {
	var out struct {
		Libraries struct {
			Nodes []StumpLibrary `json:"nodes"`
		} `json:"libraries"`
	}
	if err := s.execute(ctx, librariesQuery, nil, &out); err != nil {
		return nil, err
	}
	return out.Libraries.Nodes, nil
}

// Scan asks Stump to rescan a library so a freshly downloaded file shows up
// without waiting for the next scheduled scan. Stump enqueues a background job
// and returns immediately, so this returning nil means "accepted", not
// "finished".
func (s *StumpClient) Scan(ctx context.Context, libraryID string) error {
	if libraryID == "" {
		return fmt.Errorf("no Stump library id configured (set it on the Settings page, or via STUMP_LIBRARY_ID)")
	}

	var out struct {
		ScanLibrary bool `json:"scanLibrary"`
	}
	if err := s.execute(ctx, scanLibraryMutation, map[string]any{"id": libraryID}, &out); err != nil {
		return err
	}
	if !out.ScanLibrary {
		return fmt.Errorf("Stump declined to scan library %q", libraryID)
	}
	return nil
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// execute runs one GraphQL operation and unmarshals `data` into out.
func (s *StumpClient) execute(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(graphQLRequest{Query: query, Variables: vars})
	if err != nil {
		return err
	}

	payload, err := s.post(ctx, graphQLPath, body)
	if err != nil {
		return err
	}

	var resp graphQLResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return fmt.Errorf("could not parse the GraphQL response from Stump: %w", err)
	}

	// GraphQL reports failures in `errors` with HTTP 200, so this check is not
	// redundant with the status check in post.
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("Stump returned: %s", strings.Join(msgs, "; "))
	}
	if len(resp.Data) == 0 {
		return fmt.Errorf("Stump returned no data")
	}

	return json.Unmarshal(resp.Data, out)
}

func (s *StumpClient) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	cur := s.settings.Get()

	resp, err := s.request(ctx, cur, path, body)
	if err != nil {
		return nil, err
	}

	// A session can expire; log in once and retry before giving up.
	if resp.StatusCode == http.StatusUnauthorized && cur.APIKey == "" && cur.Username != "" {
		resp.Body.Close()
		s.mu.Lock()
		s.loggedIn = false
		s.mu.Unlock()

		if err := s.ensureLogin(ctx, cur); err != nil {
			return nil, err
		}
		if resp, err = s.request(ctx, cur, path, body); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("reading Stump response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(payload))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		if msg == "" {
			msg = "(empty response body)"
		}
		if resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("Stump rejected the credentials (HTTP 401): %s", msg)
		}
		return nil, fmt.Errorf("Stump returned HTTP %d for %s: %s", resp.StatusCode, path, msg)
	}

	// Hitting a path Stump doesn't route falls through to its web UI, which is
	// a much clearer thing to report than a JSON parse error.
	if trimmed := strings.TrimLeft(string(payload), " \t\r\n"); strings.HasPrefix(strings.ToLower(trimmed), "<!doctype") {
		return nil, fmt.Errorf("%s returned Stump's web UI instead of an API response — check the Stump URL in Settings (or STUMP_URL) points at a Stump server", path)
	}

	return payload, nil
}

func (s *StumpClient) request(ctx context.Context, cur StumpSettings, path string, body []byte) (*http.Response, error) {
	if cur.APIKey == "" {
		if err := s.ensureLogin(ctx, cur); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cur.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if cur.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cur.APIKey)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to Stump at %s: %w", cur.URL, err)
	}
	return resp, nil
}

func (s *StumpClient) ensureLogin(ctx context.Context, cur StumpSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fp := fingerprint(cur)
	if s.loggedIn && s.loggedInAs == fp {
		return nil
	}
	if cur.Username == "" || cur.Password == "" {
		return fmt.Errorf("no Stump credentials configured (set them on the Settings page, or via STUMP_API_KEY / STUMP_USERNAME+STUMP_PASSWORD)")
	}

	payload, err := json.Marshal(map[string]string{
		"username": cur.Username,
		"password": cur.Password,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cur.URL+loginPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("logging in to Stump at %s: %w", cur.URL, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Stump rejected the login for %q (HTTP %d)", cur.Username, resp.StatusCode)
	}

	s.loggedIn = true
	s.loggedInAs = fp
	return nil
}
