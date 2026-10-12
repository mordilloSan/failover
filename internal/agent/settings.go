package agent

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// setting ties a key of failover.yml to its section in the settings page.
// locked: only while every service is on the server (Rede, Caminhos, the DNS zone and API);
// secret: never sent back, and an empty value keeps the current one.
type setting struct {
	key, section   string
	locked, secret bool
	str            func(*Config) *string
	num            func(*Config) *int
}

func str(key, section string, f func(*Config) *string) setting {
	return setting{key: key, section: section, str: f}
}

func num(key, section string, f func(*Config) *int) setting {
	return setting{key: key, section: section, num: f}
}

func locked(s setting) setting { s.locked = true; return s }
func secret(s setting) setting { s.secret = true; return s }

// Services are set with the services' own form.
var settings = []setting{
	locked(str("server.ip", "rede", func(c *Config) *string { return &c.Server.IP })),
	locked(str("server.npm_check_host", "rede", func(c *Config) *string { return &c.Server.NPMCheckHost })),
	locked(str("tnas_ip", "rede", func(c *Config) *string { return &c.TNASIP })),
	locked(str("router_ip", "rede", func(c *Config) *string { return &c.RouterIP })),
	locked(str("lan_iface", "rede", func(c *Config) *string { return &c.LANIface })),
	num("start_timeout_min", "verificacao", func(c *Config) *int { return &c.StartTimeoutMin }),
	num("npm.alert_after_min", "verificacao", func(c *Config) *int { return &c.NPM.AlertAfterMin }),
	num("maintenance.default_expiry_min", "verificacao", func(c *Config) *int { return &c.Maintenance.DefaultExpiryMin }),
	locked(str("dns.api_url", "dns", func(c *Config) *string { return &c.DNS.APIURL })),
	locked(str("dns.zone", "dns", func(c *Config) *string { return &c.DNS.Zone })),
	num("dns.ttl", "dns", func(c *Config) *int { return &c.DNS.TTL }),
	locked(str("paths.mirror_subvol", "caminhos", func(c *Config) *string { return &c.Paths.MirrorSubvol })),
	locked(str("paths.mirror_root", "caminhos", func(c *Config) *string { return &c.Paths.MirrorRoot })),
	locked(str("paths.snapshots_dir", "caminhos", func(c *Config) *string { return &c.Paths.SnapshotsDir })),
	locked(str("paths.overrides_dir", "caminhos", func(c *Config) *string { return &c.Paths.OverridesDir })),
	locked(str("npm.dir", "caminhos", func(c *Config) *string { return &c.NPM.Dir })),
	num("paths.mirror_stale_days", "caminhos", func(c *Config) *int { return &c.Paths.MirrorStaleDays }),
	str("email.host", "avisos", func(c *Config) *string { return &c.Email.Host }),
	num("email.port", "avisos", func(c *Config) *int { return &c.Email.Port }),
	str("email.security", "avisos", func(c *Config) *string { return &c.Email.Security }),
	str("email.user", "avisos", func(c *Config) *string { return &c.Email.User }),
	secret(str("email.password", "avisos", func(c *Config) *string { return &c.Email.Password })),
	str("email.from", "avisos", func(c *Config) *string { return &c.Email.From }),
	str("email.to", "avisos", func(c *Config) *string { return &c.Email.To }),
	str("nightly.prepull_at", "noturna", func(c *Config) *string { return &c.Nightly.PrepullAt }),
	str("ui.listen", "interface", func(c *Config) *string { return &c.UI.Listen }),
}

// settingsView is every setting for the page; a secret only says whether it is set.
func settingsView(c *Config) map[string]any {
	m := make(map[string]any, len(settings))
	for _, s := range settings {
		switch {
		case s.num != nil:
			m[s.key] = *s.num(c)
		case s.secret:
			m[s.key] = *s.str(c) != ""
		default:
			m[s.key] = *s.str(c)
		}
	}
	return m
}

// apply sets the value in raw on c; msg says why it could not.
func (s setting) apply(c *Config, raw json.RawMessage) (changed bool, msg string) {
	if s.num != nil {
		var n int
		if json.Unmarshal(raw, &n) != nil {
			return false, "tem de ser um número inteiro"
		}
		changed = *s.num(c) != n
		*s.num(c) = n
		return changed, ""
	}
	var v string
	if json.Unmarshal(raw, &v) != nil {
		return false, "tem de ser texto"
	}
	if v = strings.TrimSpace(v); s.secret && v == "" {
		return false, "" // keep the current one
	}
	changed = *s.str(c) != v
	*s.str(c) = v
	return changed, ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fieldErr(w http.ResponseWriter, field, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"field": field, "error": msg})
}

// allHome: every service on the server and the TNAS NPM stopped, so paths
// and addresses can change under nothing that is running.
func (a *Agent) allHome() bool {
	for _, sv := range a.cfg.Services {
		if !a.home(a.svc(sv.Name)) {
			return false
		}
	}
	return a.st.TNASNPM.Snapshot == ""
}

// home: nothing of the service runs on the TNAS. A failed failover (ERROR)
// counts once its copy, snapshot and DNS record are gone, so its cause can be
// fixed while the server is still down.
func (a *Agent) home(s *SvcState) bool {
	return s.State == Normal || s.State == Error && s.Snapshot == "" && !s.DNS
}

// postSection saves one section of the settings page: only its keys, all
// validated together, and nothing when any is refused.
func (a *Agent) postSection(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Section string                     `json:"section"`
		Values  map[string]json.RawMessage `json:"values"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	next := a.cfg
	for key, raw := range req.Values {
		i := slices.IndexFunc(settings, func(s setting) bool { return s.key == key })
		if i < 0 || settings[i].section != req.Section {
			fieldErr(w, key, "não é um campo desta secção")
			return
		}
		changed, msg := settings[i].apply(&next, raw)
		if msg != "" {
			fieldErr(w, key, msg)
			return
		}
		if changed && settings[i].locked && !a.allHome() {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "só com todos os serviços no servidor e o NPM do TNAS parado"})
			return
		}
	}
	if err := next.validate(); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) && slices.ContainsFunc(settings, func(s setting) bool { return s.key == fe.Field && s.section == req.Section }) {
			fieldErr(w, fe.Field, fe.Msg)
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	if err := checkListen(next.UI.Listen, a.cfg.UI.Listen, a.listening); err != nil {
		fieldErr(w, "ui.listen", err.Error())
		return
	}
	if err := saveConfig(a.cfgPath, &next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "guardar configuração: " + err.Error()})
		return
	}
	a.cfg = next
	ip := next.TNASIP
	a.tnasIP.Store(&ip)
	a.now = time.Now()
	a.done(w, "", "definições alteradas: "+req.Section)
}

// checkListen opens a new UI address once, so a port in use is refused here
// and not on the restart, which would leave no UI. The port the UI already
// holds cannot be opened again, so it is not tried.
func checkListen(next, saved, running string) error {
	if next == saved || next == running {
		return nil
	}
	if _, np, _ := net.SplitHostPort(next); running != "" {
		if _, rp, _ := net.SplitHostPort(running); np == rp {
			return nil
		}
	}
	ln, err := net.Listen("tcp", next)
	if err != nil {
		return errors.New("não consigo abrir este endereço: " + err.Error())
	}
	return ln.Close()
}

type checkResult struct {
	Field string `json:"field"`
	OK    bool   `json:"ok"`
	Msg   string `json:"msg"`
}

// postCheck runs the checks of a section after it is saved. They only warn:
// a ping fails while the server is down without its address being wrong.
func (a *Agent) postCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Section string `json:"section"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	c := a.cfg
	a.mu.Unlock()
	out := []checkResult{}
	add := func(field string, err error, ok string) {
		res := checkResult{Field: field, OK: err == nil, Msg: ok}
		if err != nil {
			res.Msg = err.Error()
		}
		out = append(out, res)
	}
	dir := func(field, p string) {
		_, err := os.Stat(p)
		if err != nil {
			err = errors.New(p + " não existe ou não se lê")
		}
		add(field, err, p+" existe")
	}
	switch req.Section {
	case "rede":
		add("router_ip", a.pingErr(c.RouterIP), "o router responde a ping")
		add("server.ip", a.pingErr(c.Server.IP), "o servidor responde a ping")
		add("tnas_ip", a.pingErr(c.TNASIP), "o TNAS responde a ping")
		add("server.npm_check_host", a.sys.Check(c.Server.NPMCheckHost, c.Server.IP), "o NPM do servidor responde")
	case "dns":
		b, _ := os.ReadFile(c.DNS.TokenFile)
		add("dns.zone", testToken(a.sys, c.DNS.APIURL, c.DNS.Zone, strings.TrimSpace(string(b))), "o Technitium responde para a zona "+c.DNS.Zone)
	case "caminhos":
		root := filepath.Join(c.Paths.MirrorSubvol, c.Paths.MirrorRoot)
		dir("paths.mirror_subvol", c.Paths.MirrorSubvol)
		dir("paths.mirror_root", root)
		dir("npm.dir", filepath.Join(root, c.NPM.Dir))
		// the agent makes these two on first use: a missing one is fine under an existing parent
		made := func(field, p string) {
			if _, err := os.Stat(p); err == nil {
				add(field, nil, p+" existe")
				return
			}
			_, err := os.Stat(filepath.Dir(p))
			if err != nil {
				err = errors.New(filepath.Dir(p) + " não existe: " + p + " não se pode criar")
			}
			add(field, err, p+" ainda não existe; o agente cria-a no primeiro uso")
		}
		made("paths.snapshots_dir", c.Paths.SnapshotsDir)
		if c.Paths.OverridesDir != "" {
			made("paths.overrides_dir", c.Paths.OverridesDir)
		}
		// a failover's first step, tried now: the mirror is a subvolume and the
		// snapshots are on its volume (reflinks never cross volumes)
		add("paths.snapshots_dir", a.snapshotTest(c), "snapshot de teste do espelho feito e apagado")
	}
	writeJSON(w, http.StatusOK, out)
}

// postSetupDone records that the first-start guide was finished.
func (a *Agent) postSetupDone(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.st.SetupPending = false
	a.now = time.Now()
	a.done(w, "", "configuração inicial concluída")
}

// SetRestart is how the UI ends the agent so Docker starts it again.
func (a *Agent) SetRestart(f func()) { a.mu.Lock(); a.restart = f; a.mu.Unlock() }

// SetListening records the address the UI is actually on.
func (a *Agent) SetListening(addr string) { a.mu.Lock(); a.listening = addr; a.mu.Unlock() }

func (a *Agent) postRestart(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	f := a.restart
	if f != nil {
		a.now = time.Now()
		a.event("", "reinício pedido (interface)")
		a.save()
	}
	a.mu.Unlock()
	if f == nil {
		http.Error(w, "reinício indisponível", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	go f()
}
