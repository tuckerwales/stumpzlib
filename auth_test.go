package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConstantTimeEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"secret", "secret", true},
		{"secret", "wrong", false},
		{"secret", "secret ", false}, // different length
		{"", "", true},
	}
	for _, c := range cases {
		if got := constantTimeEqual(c.a, c.b); got != c.want {
			t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCheckCredentials(t *testing.T) {
	cfg := &Config{AuthUsername: "alice", AuthPassword: "hunter2"}

	if !cfg.checkCredentials("alice", "hunter2") {
		t.Error("correct credentials were rejected")
	}
	if cfg.checkCredentials("alice", "wrong") {
		t.Error("wrong password was accepted")
	}
	if cfg.checkCredentials("bob", "hunter2") {
		t.Error("wrong username was accepted")
	}
}

func TestSessionStoreLifecycle(t *testing.T) {
	s := NewSessionStore()

	if s.Valid("nonexistent") {
		t.Error("an unknown token should not be valid")
	}
	if s.Valid("") {
		t.Error("an empty token should not be valid")
	}

	token := s.New()
	if !s.Valid(token) {
		t.Fatal("a freshly issued token should be valid")
	}

	s.Delete(token)
	if s.Valid(token) {
		t.Error("a deleted token should no longer be valid")
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	s := NewSessionStore()
	token := s.New()

	// Backdate it past expiry directly, rather than waiting out sessionTTL.
	s.mu.Lock()
	s.sessions[token] = time.Now().Add(-time.Second)
	s.mu.Unlock()

	if s.Valid(token) {
		t.Error("an expired token should not be valid")
	}
}

func authTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newTestServer(t, fakeGutendex(t), "http://127.0.0.1:1", "")
	srv.cfg.AuthUsername = "alice"
	srv.cfg.AuthPassword = "hunter2"
	return srv
}

func TestRequireAuthIsNoopWhenNotConfigured(t *testing.T) {
	srv, _ := newTestServer(t, fakeGutendex(t), "http://127.0.0.1:1", "")

	called := false
	h := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if !called {
		t.Error("requireAuth should pass requests through when auth is unconfigured")
	}
}

func TestRequireAuthBlocksApiWithoutSession(t *testing.T) {
	srv := authTestServer(t)
	h := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not run without a valid session")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rec.Code)
	}
}

func TestRequireAuthRedirectsPageWithoutSession(t *testing.T) {
	srv := authTestServer(t)
	h := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not run without a valid session")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("got status %d, location %q; want redirect to /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	srv := authTestServer(t)

	form := strings.NewReader("username=alice&password=wrong")
	req := httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	srv.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "err=1") {
		t.Errorf("got status %d, location %q; want redirect back to /login?err=1", rec.Code, rec.Header().Get("Location"))
	}
	if rec.Result().Cookies() != nil && len(rec.Result().Cookies()) != 0 {
		t.Error("a failed login must not set a session cookie")
	}
}

func TestLoginGrantsAccessAndLogoutRevokesIt(t *testing.T) {
	srv := authTestServer(t)

	form := strings.NewReader("username=alice&password=hunter2")
	req := httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	srv.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("got status %d, location %q; want redirect to /", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName || cookies[0].Value == "" {
		t.Fatalf("expected a session cookie to be set, got %v", cookies)
	}
	cookie := cookies[0]

	protected := srv.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	authed := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	authed.AddCookie(cookie)
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, authed)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session was rejected: status %d", rec.Code)
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutReq.AddCookie(cookie)
	rec = httptest.NewRecorder()
	srv.handleLogout(rec, logoutReq)

	afterLogout := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	afterLogout.AddCookie(cookie)
	rec = httptest.NewRecorder()
	protected.ServeHTTP(rec, afterLogout)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("session should be revoked after logout, got status %d", rec.Code)
	}
}
