package agent

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// The page, its stylesheet and its JS modules.
//
//go:embed web/index.html web/app.css web/js
var webFS embed.FS

// The app's icon (the header's logo, with room around it for a phone's mask)
// and the manifest that lets a phone install the page on its home screen.
var (
	appIcon  = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect width="24" height="24" fill="#2052c2"/><path fill="#fff" transform="translate(5 5) scale(.583)" d="m21 9l-4-4v3h-7v2h7v3M7 11l-4 4l4 4v-3h7v-2H7z"/></svg>`)
	manifest = []byte(`{"name":"Failover do homelab","short_name":"Failover","start_url":"/","scope":"/","display":"standalone",
"background_color":"#1b2635","theme_color":"#1b2635","icons":[{"src":"icon.svg","sizes":"any","type":"image/svg+xml","purpose":"any maskable"}]}`)
)

// asset is one of webFS's files, gzipped once, with its ETag: a reload that
// finds it unchanged is a 304 of a few bytes instead of 150 KB in all.
type asset struct {
	raw, gz     []byte
	ctype, etag string
}

// assets by the path the page asks for: "index.html", "app.css", "js/main.js".
var assets = func() map[string]asset {
	m := map[string]asset{}
	_ = fs.WalkDir(webFS, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := webFS.ReadFile(p)
		h := sha256.Sum256(raw)
		m[strings.TrimPrefix(p, "web/")] = asset{raw, gzipBytes(raw), mime.TypeByExtension(path.Ext(p)), `"` + hex.EncodeToString(h[:8]) + `"`}
		return nil
	})
	return m
}()

func (s asset) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache") // kept, but asked again each time: an update shows at once
	w.Header().Set("ETag", s.etag)
	if r.Header.Get("If-None-Match") == s.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeBody(w, r, s.ctype, s.raw, s.gz)
}

func gzipBytes(b []byte) []byte { return gzipLevel(b, gzip.BestCompression) }

func gzipLevel(b []byte, level int) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, level)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}

// writeBody sends raw, or gz (the same, gzipped) to a client that takes it.
func writeBody(w http.ResponseWriter, r *http.Request, ctype string, raw, gz []byte) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Add("Vary", "Accept-Encoding")
	if gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
		return
	}
	_, _ = w.Write(raw)
}

// Inter, the LinuxIO typeface (SIL OFL 1.1), embedded so the page needs no internet.
//
//go:embed web/inter.woff2
var interFont []byte

// loginScriptHash lets the login page's one inline script run under the CSP.
var loginScriptHash = func() string {
	i, j := bytes.Index(loginHTML, []byte("<script>"))+len("<script>"), bytes.LastIndex(loginHTML, []byte("</script>"))
	h := sha256.Sum256(loginHTML[i:j])
	return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
}()

// connect-src https: is the settings page's check of the new port's /healthz;
// img-src data: the login's favicon; style-src 'unsafe-inline' the style="--c:…" and the login's <style>.
var csp = "default-src 'self'; script-src 'self' " + loginScriptHash + "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self' https:; manifest-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux() // everything here needs a session
	for p, s := range assets {
		if p == "index.html" {
			p = "{$}"
		}
		mux.Handle("GET /"+p, s)
	}
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		v := a.view.Load()
		writeBody(w, r, "application/json", v.raw, v.gz)
	})
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(a.eventsSnapshot())
		writeBody(w, r, "application/json", b, gzipLevel(b, gzip.DefaultCompression)) // sent on every poll: level 9 costs CPU for a few bytes
	})
	mux.HandleFunc("POST /api/config", a.postConfig)
	mux.HandleFunc("POST /api/config/section", a.postSection)
	mux.HandleFunc("POST /api/config/check", a.postCheck)
	mux.HandleFunc("POST /api/restart", a.postRestart)
	mux.HandleFunc("POST /api/email/test", a.postEmailTest)
	mux.HandleFunc("POST /api/setup/done", a.postSetupDone)
	mux.HandleFunc("GET /api/mirror", a.getMirror)
	mux.HandleFunc("GET /api/discover", a.getDiscover)
	mux.HandleFunc("POST /api/technitium/failover", a.postFailoverApp)
	mux.HandleFunc("GET /icons/{name}", a.getIcon)
	mux.HandleFunc("GET /api/service/override", a.getServiceOverride)
	mux.HandleFunc("POST /api/service", a.postService)
	mux.HandleFunc("POST /api/service/remove", a.postServiceRemove)
	mux.HandleFunc("POST /api/mirror/ignore", a.postMirrorIgnore)
	mux.HandleFunc("POST /api/maintenance", a.postMaintenance)
	mux.HandleFunc("POST /api/action", a.postAction)
	mux.HandleFunc("POST /api/password", a.postPassword)
	mux.HandleFunc("POST /api/dns", a.postDNS)
	mux.HandleFunc("POST /api/images", func(w http.ResponseWriter, _ *http.Request) {
		go a.scanImages()
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/logout", a.postLogout)
	mux.HandleFunc("POST /api/token", a.postToken)
	mux.HandleFunc("POST /api/mirror/seen", func(w http.ResponseWriter, _ *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.st.MirrorNew = nil // the page showed them: the hint goes
		a.save()
		w.WriteHeader(http.StatusNoContent)
	})

	root := http.NewServeMux() // open: the login and what it shows
	root.HandleFunc("GET /login", a.getLogin)
	root.HandleFunc("POST /login", a.postLogin)
	root.HandleFunc("POST /setup", a.postSetup)
	root.HandleFunc("GET /inter.woff2", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "font/woff2")
		w.Header().Set("Cache-Control", "private, max-age=604800")
		_, _ = w.Write(interFont)
	})
	// to install the page on a phone's home screen: open, as the browser asks without the session
	root.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		_, _ = w.Write(manifest)
	})
	root.HandleFunc("GET /icon.svg", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=604800")
		_, _ = w.Write(appIcon)
	})
	root.HandleFunc("GET /api/{$}", a.getAPI) // a session or the API token
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	root.Handle("/", a.auth(mux))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", csp) // /icons/ sets its own, stricter, over this
		w.Header().Set("Referrer-Policy", "no-referrer")
		root.ServeHTTP(w, r)
	})
}

// UIURL is the address to open the UI at: listen (ui.listen) with host in
// place of an unspecified address like 0.0.0.0, https when the UI has TLS.
func UIURL(listen, host string, https bool) (string, error) {
	h, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if ip := net.ParseIP(h); h == "" || ip != nil && ip.IsUnspecified() {
		h = host
	}
	scheme := "http://"
	if https {
		scheme = "https://"
	}
	return scheme + net.JoinHostPort(h, port), nil
}

// Healthcheck asks the UI at listen for /healthz, on loopback when it listens
// everywhere; the image's HEALTHCHECK runs it as "failover-agent healthcheck".
// With certFile the UI is HTTPS and only that certificate is trusted, under
// its own first name, since a loopback address is not in it.
func Healthcheck(listen, certFile string) error {
	u, err := UIURL(listen, "127.0.0.1", certFile != "")
	if err != nil {
		return err
	}
	tr := &http.Transport{}
	if certFile != "" {
		if tr.TLSClientConfig, err = trustOnly(certFile); err != nil {
			return err
		}
	}
	return healthz(u, tr)
}

// SelfCheck is Healthcheck for the agent's own watchdog: it trusts the
// certificate the UI serves, from memory, since one that could not be saved
// is not the one on disk.
func (a *Agent) SelfCheck(listen string) error {
	u, err := UIURL(listen, "127.0.0.1", true)
	if err != nil {
		return err
	}
	s := a.certs.Load()
	if s == nil {
		return errors.New("a interface não tem certificado")
	}
	if _, err = s.get(nil); err != nil { // makes it if no handshake did yet
		return err
	}
	s.mu.Lock()
	leaf := s.leaf
	s.mu.Unlock()
	cfg, err := trustLeaf(leaf)
	if err != nil {
		return err
	}
	return healthz(u, &http.Transport{TLSClientConfig: cfg})
}

func healthz(u string, tr *http.Transport) error {
	c := http.Client{Timeout: 5 * time.Second, Transport: tr}
	resp, err := c.Get(u + "/healthz")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// trustOnly is a TLS config that accepts the certificate in certFile and no
// other, checked against its first name (a wildcard stands for any label).
func trustOnly(certFile string) (*tls.Config, error) {
	b, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("certificado da interface: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("certificado da interface ilegível")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certificado da interface: %w", err)
	}
	return trustLeaf(leaf)
}

func trustLeaf(leaf *x509.Certificate) (*tls.Config, error) {
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = strings.Replace(leaf.DNSNames[0], "*", "healthcheck", 1)
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return nil, errors.New("certificado da interface sem nomes")
	}
	return &tls.Config{RootCAs: pool, ServerName: name, MinVersion: tls.VersionTLS12}, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		http.Error(w, "pedido inválido: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// done persists a UI change and wakes the loop so it applies without waiting.
func (a *Agent) done(w http.ResponseWriter, svc, msg string) {
	a.event(svc, msg+" (interface)")
	a.save()
	a.poke()
	w.WriteHeader(http.StatusNoContent)
}

// postConfig applies what the request names and leaves the rest as it is.
func (a *Agent) postConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode           *string `json:"mode"`
		CheckIntervalS *int    `json:"check_interval_s"`
		Services       []struct {
			Name         string `json:"name"`
			WaitMin      *int   `json:"wait_min"`
			StabilityMin *int   `json:"stability_min"`
		} `json:"services"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.cfg
	next.Services = slices.Clone(a.cfg.Services)
	if req.Mode != nil {
		next.Mode = *req.Mode
	}
	if req.CheckIntervalS != nil {
		next.CheckIntervalS = *req.CheckIntervalS
	}
	for _, rs := range req.Services {
		i := slices.IndexFunc(next.Services, func(s Service) bool { return s.Name == rs.Name })
		if i < 0 {
			http.Error(w, "serviço desconhecido: "+rs.Name, http.StatusBadRequest)
			return
		}
		if rs.WaitMin != nil {
			next.Services[i].WaitMin = *rs.WaitMin
		}
		if rs.StabilityMin != nil {
			next.Services[i].StabilityMin = *rs.StabilityMin
		}
	}
	if err := next.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := saveConfig(a.cfgPath, &next); err != nil {
		http.Error(w, "guardar configuração: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.cfg = next
	a.now = time.Now()
	a.done(w, "", "configuração alterada")
}

func (a *Agent) postMaintenance(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"` // empty = global
		Minutes int    `json:"minutes"` // 0 = off
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Minutes < 0 || req.Minutes > 7*24*60 {
		http.Error(w, "minutos entre 0 e 10080", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	until := &a.st.MaintUntil
	if req.Service != "" {
		if _, ok := a.service(req.Service); !ok {
			http.Error(w, "serviço desconhecido", http.StatusNotFound)
			return
		}
		until = &a.svc(req.Service).MaintUntil
	}
	*until = time.Time{}
	msg := "manutenção desligada"
	if req.Minutes > 0 {
		*until = a.now.Add(minutes(req.Minutes))
		msg = "manutenção ligada até " + until.Local().Format("02/01 15:04")
	}
	a.done(w, req.Service, msg)
}

func (a *Agent) postAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
		Action  string `json:"action"` // failover | return
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.service(req.Service); !ok {
		http.Error(w, "serviço desconhecido", http.StatusNotFound)
		return
	}
	a.now = time.Now()
	s := a.svc(req.Service)
	switch {
	// From ERROR only once the cleanup finished, otherwise return first.
	case req.Action == "failover" && (s.State == Normal || s.State == Error && s.Snapshot == "" && !s.DNS):
		a.set(s, FailingOver)
		s.Forced = true
		a.done(w, req.Service, "failover forçado: fica no TNAS até um regresso forçado")
	case req.Action == "return" && (s.State == FailingOver || s.State == Active || s.State == Error):
		a.set(s, Returning)
		a.done(w, req.Service, "regresso forçado")
	default:
		http.Error(w, "ação impossível no estado "+s.State, http.StatusConflict)
	}
}

func (a *Agent) postPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	c := a.creds.Load()
	if bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(req.Current)) != nil {
		http.Error(w, "a password atual está errada", http.StatusForbidden)
		return
	}
	if len(req.New) < 8 || len(req.New) > 72 { // bcrypt ignores anything past 72 bytes
		http.Error(w, "a nova password tem de ter entre 8 e 72 caracteres (os acentos contam a dobrar)", http.StatusBadRequest)
		return
	}
	n, err := newCreds(c.User, req.New)
	if err == nil {
		n.APITokenHash = c.APITokenHash
		err = saveUser(a.userPath, n)
	}
	if err != nil {
		http.Error(w, "guardar a password: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.creds.Store(n)
	a.sessions.keepOnly(token(r)) // whoever knew the old password is logged out
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	a.done(w, "", "password da interface alterada")
}

// postDNS replaces the Technitium token. The token is checked with a call that
// needs no zone (the zone may not be known yet); one that cannot touch the zone
// is kept, with a warning.
func (a *Agent) postDNS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" {
		http.Error(w, "falta o token do Technitium", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	d := a.cfg.DNS
	a.mu.Unlock()
	// outside the lock: the Technitium may take seconds to answer. The zone may
	// not be known yet (a new install): a token that is valid but cannot read it
	// is kept, with a warning.
	if err := technitiumJSON(a.sys, d.APIURL, "zones/list", nil, tok, nil); err != nil {
		http.Error(w, "o Technitium recusou o token: "+err.Error(), http.StatusBadRequest)
		return
	}
	warning := ""
	if err := testToken(a.sys, d.APIURL, d.Zone, tok); err != nil {
		warning = "o token foi guardado, mas não mexe na zona " + d.Zone + ": " + err.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := writeAtomic(d.TokenFile, []byte(tok+"\n")); err != nil {
		http.Error(w, "guardar o token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.now = time.Now()
	a.event("", "token do Technitium alterado (interface)")
	a.save()
	a.poke()
	writeJSON(w, http.StatusOK, map[string]string{"warning": warning})
}

// The address the UI is on, kept next to the state: after a new ui.listen is
// saved, the healthcheck still asks the running one until the restart.
func runningFile(statePath string) string { return filepath.Join(filepath.Dir(statePath), "ui-listen") }

func SetRunningListen(statePath, addr string) error {
	return writeAtomic(runningFile(statePath), []byte(addr))
}

func RunningListen(statePath, fallback string) string {
	if b, err := os.ReadFile(runningFile(statePath)); err == nil && isListen(strings.TrimSpace(string(b))) {
		return strings.TrimSpace(string(b))
	}
	return fallback
}
