package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The zone's * is a record of the Technitium's Failover app: the server's
// address while its NPM answers on 443, the TNAS's otherwise (R8). The
// readiness checks it on every Technitium of the cluster, and Definições → DNS
// writes it.

const (
	failoverApp   = "Failover"
	failoverClass = "Failover.Address"
	// the server's NPM, not just the host: a server up without its containers
	// sends the names to the TNAS too
	failoverCheck = "tcp443"
)

// failoverData is the APP record's data, as the app reads it.
type failoverData struct {
	Primary        []string `json:"primary"`
	Secondary      []string `json:"secondary"`
	ServerDown     []string `json:"serverDown"`
	HealthCheck    string   `json:"healthCheck"`
	HealthCheckURL *string  `json:"healthCheckUrl"`
	AllowTxtStatus bool     `json:"allowTxtStatus"`
}

func wantFailover(c *Config) failoverData {
	return failoverData{Primary: []string{c.Server.IP}, Secondary: []string{c.TNASIP}, ServerDown: []string{c.TNASIP},
		HealthCheck: failoverCheck, AllowTxtStatus: true}
}

func (d failoverData) same(o failoverData) bool {
	return slices.Equal(d.Primary, o.Primary) && slices.Equal(d.Secondary, o.Secondary) &&
		slices.Equal(d.ServerDown, o.ServerDown) && d.HealthCheck == o.HealthCheck
}

func (d failoverData) String() string {
	return "primário " + strings.Join(d.Primary, " ") + ", secundário " + strings.Join(d.Secondary, " ") + ", teste " + d.HealthCheck
}

// techNode is one Technitium of the cluster; with no cluster, the one of
// dns.api_url alone.
type techNode struct {
	Name, IP string
	Self, Up bool
}

func (n techNode) String() string {
	if n.Name == "" {
		return "Technitium"
	}
	return "Technitium " + n.Name + " (" + n.IP + ")"
}

// on adds the node to q, for a call meant for another Technitium of the cluster.
func (n techNode) on(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	if !n.Self {
		q.Set("node", n.Name)
	}
	return q
}

func techNodes(sys System, c Config, tok string) []techNode {
	var cs struct {
		Response struct {
			ClusterInitialized bool `json:"clusterInitialized"`
			Nodes              []struct {
				Name      string `json:"name"`
				IPAddress string `json:"ipAddress"`
				State     string `json:"state"`
			} `json:"nodes"`
		} `json:"response"`
	}
	if technitiumJSON(sys, c.DNS.APIURL, "admin/cluster/state", nil, tok, &cs) != nil || !cs.Response.ClusterInitialized {
		return []techNode{{Self: true, Up: true}}
	}
	var nodes []techNode
	for _, n := range cs.Response.Nodes {
		nodes = append(nodes, techNode{n.Name, n.IPAddress, n.State == "Self", n.State == "Self" || n.State == "Connected"})
	}
	return nodes
}

type techApp struct {
	Name string `json:"name"`
	URL  string `json:"url"` // in the store's list
}

func hasFailoverApp(sys System, c Config, tok string, n techNode) (bool, error) {
	var l struct {
		Response struct {
			Apps []techApp `json:"apps"`
		} `json:"response"`
	}
	if err := technitiumJSON(sys, c.DNS.APIURL, "apps/list", n.on(nil), tok, &l); err != nil {
		return false, err
	}
	return slices.ContainsFunc(l.Response.Apps, func(a techApp) bool { return a.Name == failoverApp }), nil
}

type techRecord struct {
	Type  string `json:"type"`
	RData struct {
		IPAddress string `json:"ipAddress"`
		AppName   string `json:"appName"`
		ClassPath string `json:"classPath"`
		Data      string `json:"data"`
	} `json:"rData"`
}

func wildcardRecords(sys System, c Config, tok string) ([]techRecord, error) {
	var rec struct {
		Response struct {
			Records []techRecord `json:"records"`
		} `json:"response"`
	}
	err := technitiumJSON(sys, c.DNS.APIURL, "zones/records/get", url.Values{"domain": {"*." + c.DNS.Zone}, "zone": {c.DNS.Zone}}, tok, &rec)
	return rec.Response.Records, err
}

// failoverProblems is what keeps the zone's * from following the server: the
// app missing on a Technitium that answers, a native A or AAAA on * (it wins
// over the app), or an APP record other than wantFailover.
func failoverProblems(sys System, c Config, tok string) []string {
	const fix = "; corrige em Definições → DNS"
	var p []string
	for _, n := range techNodes(sys, c, tok) {
		if !n.Up {
			continue // down, as the server's is with the server: checked when it is back
		}
		switch ok, err := hasFailoverApp(sys, c, tok, n); {
		case err != nil:
			p = append(p, "o "+n.String()+" não disse que apps tem: "+err.Error())
		case !ok:
			p = append(p, "a app Failover não está instalada no "+n.String()+fix)
		}
	}
	zone := "*." + c.DNS.Zone
	recs, err := wildcardRecords(sys, c, tok)
	if err != nil {
		return append(p, "não consegui ler o "+zone+": "+err.Error())
	}
	want, app := wantFailover(&c), false
	for _, r := range recs {
		switch r.Type {
		case "A", "AAAA":
			p = append(p, "o "+zone+" tem um registo "+r.Type+" "+r.RData.IPAddress+", que ganha à app Failover"+fix)
		case "APP":
			app = true
			var d failoverData
			if r.RData.AppName != failoverApp || r.RData.ClassPath != failoverClass || json.Unmarshal([]byte(r.RData.Data), &d) != nil || !d.same(want) {
				p = append(p, "o registo APP do "+zone+" não é o do failover ("+want.String()+")"+fix)
			}
		}
	}
	if !app {
		p = append(p, "o "+zone+" não é um registo da app Failover, por isso com o servidor em baixo os outros nomes não passam para o TNAS"+fix)
	}
	return p
}

// installFailoverApp installs the app from the Technitium's store on n,
// unless it has it; installed says it had to.
func installFailoverApp(sys System, c Config, tok string, n techNode) (installed bool, err error) {
	if ok, err := hasFailoverApp(sys, c, tok, n); err != nil || ok {
		if err != nil {
			err = fmt.Errorf("o %s não disse que apps tem: %w", n, err)
		}
		return false, err
	}
	var store struct {
		Response struct {
			StoreApps []techApp `json:"storeApps"`
		} `json:"response"`
	}
	if err := technitiumJSON(sys, c.DNS.APIURL, "apps/listStoreApps", n.on(nil), tok, &store); err != nil {
		return false, fmt.Errorf("o %s não chegou à loja de apps: %w", n, err)
	}
	i := slices.IndexFunc(store.Response.StoreApps, func(a techApp) bool { return a.Name == failoverApp })
	if i < 0 {
		return false, fmt.Errorf("a loja de apps do %s não tem a Failover", n)
	}
	q := n.on(url.Values{"name": {failoverApp}, "url": {store.Response.StoreApps[i].URL}})
	if err := technitiumJSON(sys, c.DNS.APIURL, "apps/downloadAndInstall", q, tok, nil); err != nil {
		return false, fmt.Errorf("instalar a app Failover no %s: %w", n, err)
	}
	return true, nil
}

// applyFailover makes it so: the app on every Technitium that answers, then
// the * as wantFailover, then the native A and AAAA on * removed. The APP
// record goes in first, so * always answers.
func applyFailover(sys System, c Config, tok string) ([]string, error) {
	var done []string
	for _, n := range techNodes(sys, c, tok) {
		if !n.Up {
			continue
		}
		installed, err := installFailoverApp(sys, c, tok, n)
		if err != nil {
			return done, err
		}
		if installed {
			done = append(done, "app Failover instalada no "+n.String())
		}
	}
	zone := "*." + c.DNS.Zone
	recs, err := wildcardRecords(sys, c, tok)
	if err != nil {
		return done, fmt.Errorf("ler o %s: %w", zone, err)
	}
	want := wantFailover(&c)
	data, _ := json.MarshalIndent(want, "", "  ")
	q := url.Values{"domain": {zone}, "zone": {c.DNS.Zone}, "type": {"APP"}, "ttl": {strconv.Itoa(c.DNS.TTL)}, "overwrite": {"true"},
		"appName": {failoverApp}, "classPath": {failoverClass}, "recordData": {string(data)}}
	if err := technitiumJSON(sys, c.DNS.APIURL, "zones/records/add", q, tok, nil); err != nil {
		return done, fmt.Errorf("guardar o %s: %w", zone, err)
	}
	done = append(done, zone+" com a app Failover ("+want.String()+")")
	for _, r := range recs {
		if r.Type != "A" && r.Type != "AAAA" {
			continue
		}
		q := url.Values{"domain": {zone}, "zone": {c.DNS.Zone}, "type": {r.Type}, "ipAddress": {r.RData.IPAddress}}
		if err := technitiumJSON(sys, c.DNS.APIURL, "zones/records/delete", q, tok, nil); err != nil {
			return done, fmt.Errorf("apagar o %s %s do %s: %w", r.Type, r.RData.IPAddress, zone, err)
		}
		done = append(done, "registo "+r.Type+" "+r.RData.IPAddress+" do "+zone+" apagado")
	}
	return done, nil
}

// postFailoverApp is Definições → DNS → Configurar. It runs outside the lock
// (the Technitium may take seconds, an install longer) and then makes the
// verdict again.
func (a *Agent) postFailoverApp(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	c := a.cfg
	a.mu.Unlock()
	b, _ := os.ReadFile(c.DNS.TokenFile)
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		http.Error(w, "falta o token do Technitium", http.StatusBadRequest)
		return
	}
	done, err := applyFailover(a.sys, c, tok)
	a.mu.Lock()
	a.now = time.Now()
	for _, d := range done {
		a.event("", d+" (interface)")
	}
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	a.checkReady()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done(w, "", "*."+c.DNS.Zone+" configurado no Technitium")
}
