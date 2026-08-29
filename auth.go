package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "stumpzlib_session"
	sessionTTL        = 30 * 24 * time.Hour
)

// SessionStore holds server-side session tokens in memory. Sessions don't
// survive a restart, which is fine for a single-user tool: worst case
// everyone logs in again.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: make(map[string]time.Time)}
}

func (s *SessionStore) New() string {
	token := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(sessionTTL)
	return token
}

func (s *SessionStore) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read only fails if the OS RNG is broken, which makes
		// the process untrustworthy for anything security-sensitive anyway.
		panic("stumpzlib: reading random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// checkCredentials compares against the configured username/password in
// constant time, so a wrong-length guess can't be timed apart from a
// right-length one.
func (c *Config) checkCredentials(user, pass string) bool {
	userOK := constantTimeEqual(user, c.AuthUsername)
	passOK := constantTimeEqual(pass, c.AuthPassword)
	return userOK && passOK
}

func constantTimeEqual(a, b string) bool {
	ah := sha256.Sum256([]byte(a))
	bh := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// requireAuth gates access behind a valid session cookie. It's a no-op when
// no AUTH_USERNAME/AUTH_PASSWORD is configured, matching this app's existing
// pattern of treating unconfigured features as disabled rather than broken.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.AuthConfigured() {
			next.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.Valid(cookie.Value) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AuthConfigured() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(loginPage(r.URL.Query().Get("err") == "1")))

	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
			return
		}
		if !s.cfg.checkCredentials(r.FormValue("username"), r.FormValue("password")) {
			http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    s.sessions.New(),
			Path:     "/",
			Expires:  time.Now().Add(sessionTTL),
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusSeeOther)

	default:
		writeError(w, http.StatusMethodNotAllowed, "use GET or POST")
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func loginPage(showError bool) string {
	errBlock := ""
	if showError {
		errBlock = `<p class="err">Invalid username or password.</p>`
	}
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>stumpzlib · log in</title>
<style>
  :root {
    --bg: #f4f1ea; --bg-accent: #efeae1; --panel: #fffcf7; --border: #e0d8cc;
    --border-strong: #cfc4b3; --text: #241f1a; --muted: #6d665c; --accent: #8a5a2b;
    --accent-hover: #734a22; --accent-text: #fffaf4; --err: #a33a2a; --err-bg: #f8ebe8;
    --shadow: 0 1px 2px rgba(36,31,26,.05), 0 10px 28px rgba(36,31,26,.06);
    --h: 40px; --r: 10px; --r-lg: 14px;
    color-scheme: light;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #16140f; --bg-accent: #1c1913; --panel: #221f18; --border: #3b352a;
      --border-strong: #4d4638; --text: #ece7dd; --muted: #9d968a; --accent: #c98a4b;
      --accent-hover: #d49a5c; --accent-text: #17150f; --err: #e08a78; --err-bg: #3a2420;
      --shadow: 0 1px 0 rgba(255,255,255,.03);
      color-scheme: dark;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
    background: radial-gradient(900px 420px at 50% -80px, var(--bg-accent), transparent 70%), var(--bg);
    color: var(--text);
    font: 15px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    -webkit-font-smoothing: antialiased;
  }
  .card {
    width: 100%; max-width: 340px; margin: 20px; padding: 28px;
    background: var(--panel); border: 1px solid var(--border); border-radius: var(--r-lg);
    box-shadow: var(--shadow);
  }
  .brand { display: flex; align-items: center; gap: 10px; margin-bottom: 6px; }
  .mark { width: 26px; height: 26px; flex: none; display: block; color: var(--accent); }
  h1 { margin: 0; font: 600 22px/1.2 Georgia, "Palatino Linotype", Palatino, serif; letter-spacing: -.02em; }
  .lede { color: var(--muted); font-size: 14px; margin: 8px 0 20px; }
  label { display: block; font-size: 13px; color: var(--muted); margin: 16px 0 6px; }
  input {
    width: 100%; height: var(--h); padding: 0 12px; font: inherit;
    color: var(--text); background: var(--bg);
    border: 1px solid var(--border); border-radius: var(--r);
  }
  input:hover:not(:focus) { border-color: var(--border-strong); }
  input:focus-visible, button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  button {
    display: inline-flex; align-items: center; justify-content: center;
    width: 100%; height: var(--h); margin-top: 22px; padding: 0 16px;
    font: inherit; font-weight: 600; line-height: 1; cursor: pointer;
    border: 1px solid transparent; border-radius: var(--r);
    background: var(--accent); color: var(--accent-text);
  }
  button:hover { background: var(--accent-hover); }
  .err {
    color: var(--err); background: var(--err-bg); font-size: 13px;
    padding: 9px 11px; border-radius: var(--r); margin: 0 0 18px;
  }
</style>
</head>
<body>
  <form class="card" method="post" action="/login">
    <div class="brand"><svg class="mark" viewBox="0 0 24 24" aria-hidden="true"><rect x="3.5" y="3.5" width="17" height="17" rx="4" fill="currentColor"/><path d="M8 7.5h9.2v11H9.1A1.1 1.1 0 0 1 8 17.4V7.5z" fill="var(--accent-text)"/><rect x="6" y="3.5" width="2.2" height="17" rx=".5" fill="#000" opacity=".22"/></svg><h1>stumpzlib</h1></div>
    <p class="lede">Sign in to search catalogs and add books to Stump.</p>
    ` + errBlock + `
    <label for="username">Username</label>
    <input type="text" id="username" name="username" autocomplete="username" autofocus required>
    <label for="password">Password</label>
    <input type="password" id="password" name="password" autocomplete="current-password" required>
    <button type="submit">Log in</button>
  </form>
</body>
</html>`
}
