package agent

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed web/login.html
var loginHTML []byte

const (
	sessionCookie = "failover_session"
	sessionTTL    = 30 * time.Minute // renewed on every request; the page logs out by itself after 30 min without input
)

// loginSlots caps the bcrypt checks of the open endpoints running at once:
// without it a flood of logins saturates the CPU and the watchdog restarts the agent.
var loginSlots = make(chan struct{}, 2)

var loginDelay = time.Second // after a wrong login, to slow down guessing; tests set it to 0

// sessions are kept in memory: a restart of the agent asks for the login again.
type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time // token → expiry

	fails       int       // wrong logins in a row
	lockedUntil time.Time // no login before this, after 5 wrong ones
}

func (s *sessions) create() string {
	tok := rand.Text() // 128 random bits
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	now := time.Now()
	for t, exp := range s.m { // expired ones are otherwise only dropped if their cookie comes back
		if now.After(exp) {
			delete(s.m, t)
		}
	}
	s.m[tok] = now.Add(sessionTTL)
	return tok
}

// locked says whether logins are refused: after 5 wrong ones in a row, for a minute.
func (s *sessions) locked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.lockedUntil)
}

// attempt records how a login ended: a right one starts the count again.
func (s *sessions) attempt(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		s.fails = 0
	} else if s.fails++; s.fails >= 5 {
		s.lockedUntil, s.fails = time.Now().Add(time.Minute), 0
	}
}

// crossSite refuses a form posted from another site's page (a login or an account made from there).
func crossSite(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "pedido de outro site", http.StatusForbidden)
		return true
	}
	return false
}

func token(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

func (s *sessions) valid(r *http.Request) bool {
	tok := token(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok || time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	s.m[tok] = time.Now().Add(sessionTTL)
	return true
}

func (s *sessions) drop(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tok)
}

// keepOnly ends every other session, after a password change.
func (s *sessions) keepOnly(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.m {
		if t != tok {
			delete(s.m, t)
		}
	}
}

func setSession(w http.ResponseWriter, r *http.Request, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", MaxAge: maxAge, HttpOnly: true,
		// Lax, not Strict: a link from another page (Homepage) must still open
		// the UI logged in. The API only takes JSON posts, which Lax does not let through.
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

// auth lets a request through only with a live session: the page goes to the
// login form, the API answers 401 so the page can do the same.
func (a *Agent) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.sessions.valid(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "sessão terminada: entra outra vez", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// JSON only: a cross-site form cannot send it without a CORS preflight (CSRF).
		if r.Method == http.MethodPost && r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "Content-Type tem de ser application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setupWindow is how long after the agent starts the account can be made:
// until then, whoever opens the page first gets it (as in Portainer).
const setupWindow = 30 * time.Minute

// getLogin serves the login form, or, with no account yet, the form that
// makes it (data-setup open), or says to restart the agent (closed).
func (a *Agent) getLogin(w http.ResponseWriter, r *http.Request) {
	if a.creds.Load() != nil && a.sessions.valid(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	page := bytes.Replace(loginHTML, []byte("{{version}}"), []byte(html.EscapeString(Version)), 1)
	if a.creds.Load() == nil {
		mode := map[bool]string{true: "open", false: "closed"}[time.Since(a.started) < setupWindow]
		page = bytes.Replace(page, []byte("<body>"), []byte(`<body data-setup="`+mode+`">`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// postSetup makes the admin account on the first visit: a plain form post,
// like the login, only while there is none and within setupWindow.
func (a *Agent) postSetup(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	user, pw := strings.TrimSpace(r.PostFormValue("username")), r.PostFormValue("password")
	fail := func(code string) { http.Redirect(w, r, "/login?erro="+code, http.StatusSeeOther) }
	switch {
	case a.creds.Load() != nil:
		fail("existe")
		return
	case time.Since(a.started) >= setupWindow:
		fail("fora")
		return
	case a.setupCode == "" || subtle.ConstantTimeCompare([]byte(strings.ToUpper(strings.TrimSpace(r.PostFormValue("code")))), []byte(a.setupCode)) != 1:
		fail("codigo")
		return
	case user == "" || strings.ContainsAny(user, " \t:"):
		fail("utilizador")
		return
	case len(pw) < 8:
		fail("curta")
		return
	case len(pw) > 72: // bcrypt's limit
		fail("longa")
		return
	case pw != r.PostFormValue("password2"):
		fail("diferentes")
		return
	}
	select {
	case loginSlots <- struct{}{}:
	default:
		fail("ocupado")
		return
	}
	c, err := newCreds(user, pw)
	<-loginSlots
	if err != nil {
		fail("erro")
		return
	}
	if !a.creds.CompareAndSwap(nil, c) { // someone else made it meanwhile
		fail("existe")
		return
	}
	if err := saveUser(a.userPath, c); err != nil {
		a.creds.Store(nil)
		slog.Error("guardar a conta", "error", err)
		fail("erro")
		return
	}
	a.mu.Lock()
	a.now = time.Now()
	a.event("", "conta de administrador criada: "+user)
	a.mu.Unlock()
	setSession(w, r, a.sessions.create(), 0)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// postLogin takes a plain form post, the kind password managers recognise
// both to fill in and to offer saving.
func (a *Agent) postLogin(w http.ResponseWriter, r *http.Request) {
	if crossSite(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	user, pw := r.PostFormValue("username"), r.PostFormValue("password")
	c := a.creds.Load()
	if c == nil { // no account yet: the page offers to make it
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if a.sessions.locked() {
		http.Redirect(w, r, "/login?erro=bloqueado", http.StatusSeeOther)
		return
	}
	select {
	case loginSlots <- struct{}{}:
	default:
		http.Redirect(w, r, "/login?erro=ocupado", http.StatusSeeOther)
		return
	}
	// both are always checked, so the time taken does not tell which one was wrong
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.User)) == 1
	pwOK := bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(pw)) == nil
	<-loginSlots
	a.sessions.attempt(userOK && pwOK)
	if !userOK || !pwOK {
		time.Sleep(loginDelay)
		http.Redirect(w, r, "/login?erro=1", http.StatusSeeOther)
		return
	}
	// a browser-session cookie: the agent renews the session, a Max-Age would not be
	setSession(w, r, a.sessions.create(), 0)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *Agent) postLogout(w http.ResponseWriter, r *http.Request) {
	a.sessions.drop(token(r))
	setSession(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}
