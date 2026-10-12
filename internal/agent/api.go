package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// GET /api/ is the agent's summary for a dashboard such as Homepage (its
// customapi widget picks the fields it wants). It is open to a session or to
// the API token, and carries no secrets.

// netView is each box's internet as its own Technitium last answered:
// unknown while the box or its Technitium cannot be asked. One internet: ok
// when any box reaches it.
type netView struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"` // up, warn (a box without it), down, unknown
	Server string `json:"server"` // up, down, unknown
	TNAS   string `json:"tnas"`
}

type dnsMember struct {
	Who    string `json:"who"`
	IP     string `json:"ip"`
	Status string `json:"status"` // up, down, unknown
}

type dnsView struct {
	Up      int         `json:"up"`
	Total   int         `json:"total"`
	Members []dnsMember `json:"members"`
}

func upDown(known, ok bool) string {
	switch {
	case !known:
		return "unknown"
	case ok:
		return "up"
	}
	return "down"
}

// netDNS is the internet and the DNS cluster, as the topology shows them.
func (a *Agent) netDNS() (netView, dnsView) {
	st := &a.st
	serverSeen := st.RouterOK && st.ServerUp
	n := netView{
		Server: upDown(serverSeen && st.ServerDNSOK, st.ServerNetOK),
		TNAS:   upDown(st.RouterOK && st.TNASDNSOK, st.TNASNetOK),
	}
	up, down := n.Server == "up" || n.TNAS == "up", n.Server == "down" || n.TNAS == "down"
	n.OK = up
	switch {
	case up && down:
		n.Status = "warn"
	case up:
		n.Status = "up"
	case down:
		n.Status = "down"
	default:
		n.Status = "unknown"
	}
	d := dnsView{Total: 2, Members: []dnsMember{
		{"servidor", a.cfg.Server.IP, upDown(serverSeen, st.ServerDNSOK)},
		{"TNAS", a.cfg.TNASIP, upDown(true, st.TNASDNSOK)},
	}}
	for _, m := range d.Members {
		if m.Status == "up" {
			d.Up++
		}
	}
	return n, d
}

// apiSummary is the body of GET /api/, made under mu by publish.
func (a *Agent) apiSummary(n netView, d dnsView, notAfter time.Time) []byte {
	type svc struct {
		Name   string    `json:"name"`
		Host   string    `json:"host"`
		State  string    `json:"state"`
		OnTNAS bool      `json:"on_tnas"`
		Since  time.Time `json:"since,omitzero"`
		Msg    string    `json:"msg,omitempty"`
	}
	list := []svc{}
	var home, errs int
	var away []string
	for _, sv := range a.cfg.Services {
		s := a.svc(sv.Name)
		h := a.home(s)
		list = append(list, svc{sv.Name, sv.Host, s.State, !h, s.Since, s.Msg})
		if h {
			home++
		} else {
			away = append(away, sv.Name)
		}
		if s.State == Error {
			errs++
		}
	}
	present, total, size := countImages(a.st.Images)
	var last *Event
	a.evMu.Lock()
	if len(a.events) > 0 {
		e := a.events[len(a.events)-1]
		last = &e
	}
	a.evMu.Unlock()
	v := a.st.Verdict
	b, _ := json.Marshal(struct {
		Version     string    `json:"version"`
		Now         time.Time `json:"now"`
		Mode        string    `json:"mode"`
		Maintenance time.Time `json:"maintenance_until,omitzero"`
		Ready       any       `json:"ready"`
		Server      any       `json:"server"`
		TNAS        any       `json:"tnas"`
		Router      any       `json:"router"`
		Internet    netView   `json:"internet"`
		DNS         dnsView   `json:"dns"`
		Services    any       `json:"services"`
		Images      any       `json:"images"`
		Mirror      any       `json:"mirror"`
		Cert        any       `json:"cert"`
		LastEvent   *Event    `json:"last_event"`
	}{
		Version, a.now, a.cfg.Mode, a.st.MaintUntil,
		struct {
			OK       bool      `json:"ok"`
			Problems []string  `json:"problems"`
			At       time.Time `json:"at,omitzero"`
		}{!v.At.IsZero() && len(v.Problems) == 0, append([]string{}, v.Problems...), v.At},
		struct {
			IP    string `json:"ip"`
			Up    bool   `json:"up"`
			NPMOK bool   `json:"npm_ok"`
		}{a.cfg.Server.IP, a.st.ServerUp, a.st.ServerNPMOK},
		struct {
			IP string `json:"ip"`
			Up bool   `json:"up"`
		}{a.cfg.TNASIP, a.st.TNASUp},
		struct {
			IP string `json:"ip"`
			OK bool   `json:"ok"`
			Ms int    `json:"ms"`
		}{a.cfg.RouterIP, a.st.RouterOK, a.routerMs},
		n, d,
		struct {
			Total         int    `json:"total"`
			Home          int    `json:"home"`
			Failover      int    `json:"failover"`
			Errors        int    `json:"errors"`
			FailoverNames string `json:"failover_names"`
			List          []svc  `json:"list"`
		}{len(list), home, len(away), errs, strings.Join(away, ", "), list},
		struct {
			Present  int     `json:"present"`
			Total    int     `json:"total"`
			GB       float64 `json:"gb"`
			LastPull string  `json:"last_pull,omitempty"`
		}{present, total, float64(size/1e8) / 10, a.st.LastPull},
		struct {
			Changed time.Time `json:"changed,omitzero"`
			Stale   bool      `json:"stale"`
		}{a.st.MirrorChanged, a.st.MirrorStale},
		struct {
			NotAfter time.Time `json:"not_after,omitzero"`
		}{notAfter},
		last,
	})
	return b
}

func tokenHash(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// tokenOK says whether r carries the API token ("Authorization: Bearer …").
func (a *Agent) tokenOK(r *http.Request) bool {
	c := a.creds.Load()
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && c != nil && c.APITokenHash != "" &&
		subtle.ConstantTimeCompare([]byte(tokenHash(strings.TrimSpace(tok))), []byte(c.APITokenHash)) == 1
}

func (a *Agent) getAPI(w http.ResponseWriter, r *http.Request) {
	if !a.sessions.valid(r) && !a.tokenOK(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "falta o token da API (Authorization: Bearer …) ou uma sessão", http.StatusUnauthorized)
		return
	}
	v := a.api.Load()
	writeBody(w, r, "application/json", v.raw, v.gz)
}

// postToken makes a new API token, shown this once; the old one stops working.
func (a *Agent) postToken(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	c := a.creds.Load()
	if c == nil {
		http.Error(w, "sem conta", http.StatusConflict)
		return
	}
	tok := rand.Text()
	n := *c
	n.APITokenHash = tokenHash(tok)
	if err := saveUser(a.userPath, &n); err != nil {
		http.Error(w, "guardar o token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.creds.Store(&n)
	a.mu.Lock()
	a.now = time.Now()
	a.event("", "token da API gerado (interface)")
	a.save()
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
}
