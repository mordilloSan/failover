package agent

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T-18: the account made on the first visit, the login form, sessions, JSON only, edits
// applied and persisted without a restart, and the password change.
//
//nolint:gocognit,cyclop // one request table walked in order, like a session in the browser
func TestUI(t *testing.T) {
	a, _ := setup(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if v := string(a.view.Load().raw); strings.Contains(v, "0001-01-01") {
		t.Fatalf("estado da interface: hora zero: %s", v)
	}

	// a browser: keeps the cookie, does not follow redirects so they can be checked
	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	login := func(c *http.Client, user, pw string) string {
		t.Helper()
		resp, err := c.PostForm(srv.URL+"/login", url.Values{"username": {user}, "password": {pw}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Location")
	}
	do := func(c *http.Client, ctype, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if body == "" {
			req.Method = http.MethodGet
		}
		req.Header.Set("Content-Type", ctype)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	anon, me, other := browser(), browser(), browser()
	if do(anon, "", "/", "") != http.StatusSeeOther || do(anon, "", "/api/status", "") != 401 || do(anon, "", "/api/events", "") != 401 || do(anon, "", "/login", "") != 200 {
		t.Fatal("sem sessão: a página tem de ir para o login e a API responder 401")
	}
	setupAccount := func(c *http.Client, user, pw, again string) string {
		t.Helper()
		resp, err := c.PostForm(srv.URL+"/setup", url.Values{"username": {user}, "password": {pw}, "password2": {again}, "code": {a.setupCode}})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if setupAccount(me, "admin", "uma-password-velha", "uma-password-velha") != "/" {
		t.Fatal("a conta do primeiro acesso não foi criada")
	}
	for _, bad := range [][2]string{{"admin", "errada"}, {"outro", "uma-password-velha"}} {
		if got := login(anon, bad[0], bad[1]); got != "/login?erro=1" {
			t.Fatalf("login %v: foi para %q", bad, got)
		}
	}
	if login(other, "admin", "uma-password-velha") != "/" {
		t.Fatal("a conta criada não entra")
	}
	if do(me, "", "/login", "") != http.StatusSeeOther {
		t.Fatal("com sessão, o login devia mandar para a página")
	}

	const js = "application/json"
	cfg := `{"mode":"auto","check_interval_s":30,"services":[{"name":"vaultwarden","wait_min":2,"stability_min":4}]}`
	for _, c := range []struct {
		ctype, path, body string
		want              int
	}{
		{"", "/api/status", "", 200},
		{"", "/api/events", "", 200},
		{"text/plain", "/api/config", cfg, 415},
		{js, "/api/config", `{"mode":"auto","check_interval_s":5}`, 400},
		{js, "/api/config", cfg, 204},
		{js, "/api/maintenance", `{"service":"immich","minutes":30}`, 204},
		{js, "/api/action", `{"service":"homepage","action":"return"}`, 409},
		{js, "/api/action", `{"service":"homepage","action":"failover"}`, 204},
		{js, "/api/password", `{"current":"errada","new":"uma-password-nova"}`, 403},
		{js, "/api/password", `{"current":"uma-password-velha","new":"curta"}`, 400},
		{js, "/api/password", `{"current":"uma-password-velha","new":"uma-password-nova"}`, 204},
		{"", "/api/status", "", 200}, // the session that changed it goes on
	} {
		if got := do(me, c.ctype, c.path, c.body); got != c.want {
			t.Errorf("%s %s: HTTP %d, esperado %d", c.path, c.body, got, c.want)
		}
	}
	if do(other, "", "/api/status", "") != 401 {
		t.Error("a outra sessão continua aberta depois de mudar a password")
	}
	if login(browser(), "admin", "uma-password-velha") != "/login?erro=1" || login(browser(), "admin", "uma-password-nova") != "/" {
		t.Error("depois de mudar, a password antiga ainda entra ou a nova não")
	}
	if do(me, js, "/api/logout", "{}") != 204 || do(me, "", "/api/status", "") != 401 {
		t.Error("sair não terminou a sessão")
	}

	if sv, _ := a.service("vaultwarden"); sv.WaitMin != 2 || sv.StabilityMin != 4 || a.cfg.CheckIntervalS != 30 {
		t.Errorf("config não aplicada: %+v", sv)
	}
	if saved, err := LoadConfig(a.cfgPath); err != nil || saved.Services[0].WaitMin != 2 {
		t.Errorf("config não guardada: %v", err)
	}
	if a.st.Services["immich"].MaintUntil.IsZero() || state(a, "homepage") != FailingOver {
		t.Error("manutenção ou ação não aplicada")
	}
	// the new password is in no file: user.yml has only its hash, and it survives a restart
	b, _ := os.ReadFile(a.userPath)
	if strings.Contains(string(b), "uma-password-nova") || !strings.Contains(string(b), "$2a$") {
		t.Fatalf("user.yml: %s", b)
	}
	if c, err := loadUser(a.userPath); err != nil || c == nil || c.User != "admin" {
		t.Fatalf("user.yml depois de mudar a password: %v %+v", err, c)
	}
}

// The Technitium token in the UI is tested before it is saved; one that cannot
// touch the zone is saved too, with a warning.
func TestDNSToken(t *testing.T) {
	a, f := setup(t)
	post := func(body string) (int, string) {
		t.Helper()
		w := httptest.NewRecorder()
		a.postDNS(w, httptest.NewRequest(http.MethodPost, "/api/dns", strings.NewReader(body)))
		return w.Code, w.Body.String()
	}
	tokFile := a.cfg.DNS.TokenFile
	if err := os.Remove(tokFile); err != nil {
		t.Fatal(err)
	}
	if c, _ := post(`{"token":" "}`); c != 400 {
		t.Fatal("aceitou um token vazio")
	}
	f.badToken = "errado"
	if c, _ := post(`{"token":"errado"}`); c != 400 || fileExists(tokFile) {
		t.Fatal("token recusado pelo Technitium mas gravado")
	}
	api := strings.TrimRight(a.cfg.DNS.APIURL, "/")
	f.bodies = map[string][]byte{api + "/api/zones/records/get?domain=engmariz.com&zone=engmariz.com": []byte(`{"status":"error","errorMessage":"sem direitos"}`)}
	if c, body := post(`{"token":"fraco"}`); c != 200 || !strings.Contains(body, "não mexe na zona") || !fileExists(tokFile) {
		t.Fatalf("token sem direitos na zona: HTTP %d %s", c, body)
	}
	f.bodies = nil
	if c, body := post(`{"token":" novo "}`); c != 200 || !strings.Contains(body, `"warning":""`) || !hasGet(f, "zones/list?") {
		t.Fatalf("token bom: HTTP %d %s %v", c, body, f.gets)
	}
	if b, _ := os.ReadFile(tokFile); string(b) != "novo\n" {
		t.Fatalf("token gravado: %q", b)
	}
	if fi, _ := os.Stat(tokFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("permissões do token: %v", fi.Mode().Perm())
	}
	if v := string(a.view.Load().raw); !strings.Contains(v, `"dns_token":true`) || strings.Contains(v, "novo") {
		t.Fatalf("o estado tem de dizer que há token, sem o mostrar: %s", v)
	}
}

// Expired sessions are dropped when a new one is made, so the map does not grow forever.
func TestSessionsPruned(t *testing.T) {
	var s sessions
	old := s.create()
	s.m[old] = time.Now().Add(-time.Minute)
	s.create()
	if _, ok := s.m[old]; ok || len(s.m) != 1 {
		t.Fatalf("sessões: %v", s.m)
	}
}

// First start on an empty TNAS: the shipped config is written, and never over an existing one.
func TestFirstRun(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config", "failover.yml")
	if err := FirstRun(p); err != nil {
		t.Fatal(err)
	}
	if cfg, err := LoadConfig(p); err != nil || cfg.Mode != "observe" {
		t.Fatalf("config criada errada: %v", err)
	}
	if err := os.WriteFile(p, []byte("editado"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := FirstRun(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "editado" {
		t.Fatal("o primeiro arranque escreveu por cima de uma config existente")
	}
}

// The image's HEALTHCHECK: open (no session), and 0.0.0.0 is asked on loopback.
func TestHealthcheck(t *testing.T) {
	a, _ := setup(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	if err := Healthcheck("0.0.0.0:"+port, ""); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if Healthcheck("0.0.0.0:"+port, "") == nil {
		t.Fatal("healthcheck ok com a interface em baixo")
	}

	// HTTPS: it trusts only the UI's own certificate, never any certificate.
	dir := t.TempDir()
	for _, c := range []struct {
		name  string
		names []string
	}{{"nome", []string{"failover.lan"}}, {"wildcard", []string{"*.engmariz.com"}}} {
		cert, certFile := testCert(t, dir, c.name, c.names)
		tsrv := httptest.NewUnstartedServer(a.Handler())
		tsrv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		tsrv.StartTLS()
		_, port, _ = net.SplitHostPort(tsrv.Listener.Addr().String())
		if err := Healthcheck("0.0.0.0:"+port, certFile); err != nil {
			t.Errorf("%s: healthcheck em HTTPS: %v", c.name, err)
		}
		_, other := testCert(t, dir, c.name+"-outro", c.names)
		if Healthcheck("0.0.0.0:"+port, other) == nil {
			t.Errorf("%s: healthcheck ok com um certificado que não é o da interface", c.name)
		}
		tsrv.Close()
	}
}

// The watchdog's check trusts the certificate the UI serves, even one that
// was never saved, and no other.
func TestSelfCheck(t *testing.T) {
	a, _ := setup(t)
	cfg, err := a.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	tsrv := httptest.NewUnstartedServer(a.Handler())
	tsrv.TLS = cfg
	tsrv.StartTLS()
	defer tsrv.Close()
	_, port, _ := net.SplitHostPort(tsrv.Listener.Addr().String())
	if err := a.SelfCheck("0.0.0.0:" + port); err != nil {
		t.Fatal(err)
	}
	other := httptest.NewTLSServer(a.Handler()) // another certificate
	defer other.Close()
	_, port, _ = net.SplitHostPort(other.Listener.Addr().String())
	if a.SelfCheck("0.0.0.0:"+port) == nil {
		t.Fatal("confiou num certificado que não é o da interface")
	}
}

// testCert makes a self-signed certificate for names and writes its PEM to dir.
func testCert(t *testing.T, dir, file string, names []string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file+".pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, path
}

// The address in the startup log: the TNAS IP instead of 0.0.0.0.
func TestUIURL(t *testing.T) {
	for listen, want := range map[string]string{
		"0.0.0.0:8099":      "http://192.168.1.249:8099",
		":8099":             "http://192.168.1.249:8099",
		"[::]:8099":         "http://192.168.1.249:8099",
		"192.168.1.10:8099": "http://192.168.1.10:8099",
	} {
		if got, _ := UIURL(listen, "192.168.1.249", false); got != want {
			t.Errorf("%s: %s, queria %s", listen, got, want)
		}
	}
	if got, _ := UIURL(":8099", "192.168.1.249", true); got != "https://192.168.1.249:8099" {
		t.Errorf("com TLS: %s", got)
	}
}

// A partial config changes only what it names.
func TestConfigPartial(t *testing.T) {
	a, _ := setup(t)
	post := func(body string) int {
		t.Helper()
		w := httptest.NewRecorder()
		a.postConfig(w, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
		return w.Code
	}
	iv, hp := a.cfg.CheckIntervalS, a.cfg.Services[1]
	if post(`{"mode":"observe"}`) != 204 || a.cfg.Mode != "observe" || a.cfg.CheckIntervalS != iv {
		t.Fatalf("só o modo: %s %d", a.cfg.Mode, a.cfg.CheckIntervalS)
	}
	if post(`{"services":[{"name":"vaultwarden","stability_min":7}]}`) != 204 {
		t.Fatal("só a estabilidade de um serviço foi recusada")
	}
	vw, _ := a.service("vaultwarden")
	if vw.StabilityMin != 7 || vw.WaitMin == 0 || a.cfg.Services[1] != hp || a.cfg.Mode != "observe" {
		t.Fatalf("mexeu no que não devia: %+v %+v", vw, a.cfg.Services[1])
	}
	if post(`{"services":[{"name":"vaultwarden","wait_min":0}]}`) != 400 {
		t.Fatal("aceitou espera 0")
	}
	if post(`{"services":[{"name":"nao-existe","wait_min":3}]}`) != 400 {
		t.Fatal("aceitou um serviço desconhecido")
	}
	if c, _ := LoadConfig(a.cfgPath); c.Mode != "observe" {
		t.Fatal("não gravou")
	}
}

// /api/events has every event, newest first.
func TestEventsAPI(t *testing.T) {
	a, _ := setup(t)
	a.event("", "primeiro")
	a.event("", "segundo")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, withSession(a, httptest.NewRequest(http.MethodGet, "/api/events", nil)))
	var evs []Event
	if err := json.Unmarshal(w.Body.Bytes(), &evs); err != nil || len(evs) < 2 || evs[0].Msg != "segundo" {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body)
	}
}

// The page's stylesheet and modules are served with their types, gzipped,
// and an unchanged one is a 304.
func TestAssets(t *testing.T) {
	a, _ := setup(t)
	for path, ctype := range map[string]string{"/app.css": "text/css", "/js/main.js": "text/javascript"} {
		r := withSession(a, httptest.NewRequest(http.MethodGet, path, nil))
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) || w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s: HTTP %d %v", path, w.Code, w.Header())
		}
		r.Header.Set("If-None-Match", w.Header().Get("ETag"))
		w = httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotModified {
			t.Fatalf("%s sem mudanças: HTTP %d", path, w.Code)
		}
	}
}

// withSession adds a live session cookie to r.
func withSession(a *Agent, r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: a.sessions.create()})
	return r
}

// With no account, the first visit makes one: only within 30 minutes of the
// agent's start, and never over an account that exists.
//
//nolint:gocognit // one request table walked in order, like a session in the browser
func TestFirstAccount(t *testing.T) {
	a, _ := setup(t)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	page := func() string {
		t.Helper()
		resp, err := c.Get(srv.URL + "/login")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	post := func(path string, v url.Values) string {
		t.Helper()
		resp, err := c.PostForm(srv.URL+path, v)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get("Location")
	}
	if !strings.Contains(page(), `data-setup="open"`) {
		t.Fatal("sem conta, o login não pede para a criar")
	}
	if got := post("/login", url.Values{"username": {"admin"}, "password": {"admin"}}); got != "/login" {
		t.Fatalf("admin/admin ainda entra: %q", got)
	}
	if len(a.setupCode) != 12 {
		t.Fatalf("código de arranque: %q", a.setupCode)
	}
	code := a.setupCode
	for erro, v := range map[string]url.Values{
		"curta":      {"username": {"miguel"}, "password": {"curta"}, "password2": {"curta"}, "code": {code}},
		"diferentes": {"username": {"miguel"}, "password": {"uma-password"}, "password2": {"outra-password"}, "code": {code}},
		"utilizador": {"username": {" "}, "password": {"uma-password"}, "password2": {"uma-password"}, "code": {code}},
		"codigo":     {"username": {"miguel"}, "password": {"uma-password"}, "password2": {"uma-password"}},
	} {
		if got := post("/setup", v); got != "/login?erro="+erro {
			t.Errorf("%s: %q", erro, got)
		}
	}
	ok := url.Values{"username": {"miguel"}, "password": {"uma-password"}, "password2": {"uma-password"}, "code": {"ERRADO"}}
	if got := post("/setup", ok); got != "/login?erro=codigo" || fileExists(a.userPath) {
		t.Fatalf("com um código errado: %q", got)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/setup", strings.NewReader(url.Values{"username": {"miguel"}, "password": {"uma-password"}, "password2": {"uma-password"}, "code": {code}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || fileExists(a.userPath) {
		t.Fatalf("pedido de outro site: HTTP %d", resp.StatusCode)
	}
	ok.Set("code", " "+strings.ToLower(code)+" ") // typed by hand, in lowercase
	if got := post("/setup", ok); got != "/" {
		t.Fatalf("criar: %q", got)
	}
	if b, _ := os.ReadFile(a.userPath); !strings.Contains(string(b), "miguel") || strings.Contains(string(b), "uma-password") {
		t.Fatalf("user.yml: %s", b)
	}
	if got := post("/setup", url.Values{"username": {"intruso"}, "password": {"outra-password"}, "password2": {"outra-password"}, "code": {code}}); got != "/login?erro=existe" {
		t.Fatalf("criou por cima de uma conta: %q", got)
	}

	late, _ := setup(t)
	late.started = time.Now().Add(-31 * time.Minute)
	srv2 := httptest.NewServer(late.Handler())
	defer srv2.Close()
	resp, err = http.Get(srv2.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(b), `data-setup="closed"`) {
		t.Fatal("passados 30 minutos ainda se podia criar a conta")
	}
	resp, err = c.PostForm(srv2.URL+"/setup", ok)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/login?erro=fora" || fileExists(late.userPath) {
		t.Fatalf("fora da janela: %q", got)
	}
}

// The page goes gzipped to a browser that takes it, and a reload of an
// unchanged page is a 304; the status goes gzipped too.
func TestPageGzipAndETag(t *testing.T) {
	a, _ := setup(t)
	indexHTML := assets["index.html"].raw
	get := func(path string, h map[string]string) *httptest.ResponseRecorder {
		r := withSession(a, httptest.NewRequest(http.MethodGet, path, nil))
		for k, v := range h {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	w := get("/", map[string]string{"Accept-Encoding": "gzip, br"})
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Body.Len() >= len(indexHTML)/2 || w.Header().Get("ETag") == "" {
		t.Fatalf("página: %d %v, %d bytes", w.Code, w.Header(), w.Body.Len())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); !bytes.Equal(b, indexHTML) {
		t.Fatal("o gzip não é a página")
	}
	if w := get("/", map[string]string{"If-None-Match": assets["index.html"].etag}); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("recarregar sem mudanças: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := get("/", nil); w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), indexHTML) {
		t.Fatal("sem gzip pedido, a página vai como está")
	}
	if w := get("/api/status", map[string]string{"Accept-Encoding": "gzip"}); w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("status sem gzip: %v", w.Header())
	}
}

// The login page shows the agent's version, and nothing of the network.
func TestLoginVersion(t *testing.T) {
	a, _ := setup(t)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	if b := w.Body.String(); !strings.Contains(b, "failover-agent "+Version) || strings.Contains(b, a.cfg.TNASIP) {
		t.Fatalf("página de login: %s", b[len(b)-400:])
	}
}

// Five wrong logins in a row lock the form for a minute, even for the right
// password, and the bcrypt checks run at most loginSlots at a time.
func TestLoginLockoutAndSlots(t *testing.T) {
	a, _ := setup(t)
	c, err := newCreds("admin", "uma-password")
	if err != nil {
		t.Fatal(err)
	}
	a.creds.Store(c)
	login := func(pw string) string {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(url.Values{"username": {"admin"}, "password": {pw}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		a.Handler().ServeHTTP(w, r)
		return w.Header().Get("Location")
	}
	for range 4 { // a right one in between starts the count again
		login("errada")
	}
	if got := login("uma-password"); got != "/" {
		t.Fatalf("login certo: %q", got)
	}
	for i := range 5 { // the 5th wrong one is answered as wrong, and is the one that locks
		if got := login("errada"); got != "/login?erro=1" {
			t.Fatalf("errado %d depois de um login certo: %q", i+1, got)
		}
	}
	if got := login("uma-password"); got != "/login?erro=bloqueado" {
		t.Fatalf("depois de 5 falhas: %q", got)
	}
	a.sessions.mu.Lock()
	a.sessions.lockedUntil = time.Time{}
	a.sessions.mu.Unlock()
	if got := login("uma-password"); got != "/" {
		t.Fatalf("depois do bloqueio: %q", got)
	}

	for range cap(loginSlots) {
		loginSlots <- struct{}{}
	}
	got := login("uma-password")
	for range cap(loginSlots) {
		<-loginSlots
	}
	if got != "/login?erro=ocupado" {
		t.Fatalf("com os lugares cheios: %q", got)
	}
}

// Every response carries the CSP and nosniff; the login page's one inline
// script is allowed by the hash of exactly what is served, and the icons keep
// their own stricter policy.
func TestSecurityHeaders(t *testing.T) {
	a, _ := setup(t)
	if err := os.MkdirAll(a.iconDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.iconDir(), "nextcloud"), pngIcon, 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(path string, session bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if session {
			r = withSession(a, r)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	for _, p := range []struct {
		path    string
		session bool
	}{{"/", true}, {"/login", false}} {
		w := get(p.path, p.session)
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("%s: %v", p.path, w.Header())
		}
	}
	w := get("/icons/nextcloud", true)
	if got := w.Header().Get("Content-Security-Policy"); w.Code != 200 || !strings.HasSuffix(got, "sandbox") || strings.Contains(got, "'self'") {
		t.Errorf("ícone: HTTP %d, CSP %q", w.Code, got)
	}

	// no account (the setup form) and with one: the substitutions stay outside the script
	c, _ := newCreds("admin", "uma-password")
	for _, creds := range []*creds{nil, c} {
		a.creds.Store(creds)
		w := get("/login", false)
		body := w.Body.String()
		i := strings.Index(body, "<script>") + len("<script>")
		j := strings.LastIndex(body, "</script>")
		sum := sha256.Sum256([]byte(body[i:j]))
		if want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self' "+want+";") {
			t.Errorf("o hash do script servido (%s) não está na CSP: %s", want, w.Header().Get("Content-Security-Policy"))
		}
	}
}
