package agent

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const procRoute = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
ovs_eth0	00000000	0101A8C0	0003	0	0	10	00000000	0	0	0
ovs_eth0	0001A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`

func TestDefaultRoute(t *testing.T) {
	if got, iface := defaultRoute([]byte(procRoute)); got != "192.168.1.1" || iface != "ovs_eth0" {
		t.Fatal(got, iface)
	}
	if got, _ := defaultRoute([]byte("Iface\tDestination\n")); got != "" {
		t.Fatalf("sem rota por defeito: %q", got)
	}
}

func TestLanOf(t *testing.T) {
	_, lan, _ := net.ParseCIDR("192.168.1.249/24")
	_, dock, _ := net.ParseCIDR("172.17.0.1/16")
	ifaces := []ifaceAddr{
		{"docker0", []*net.IPNet{{IP: net.ParseIP("172.17.0.1"), Mask: dock.Mask}}},
		{"ovs_eth0", []*net.IPNet{{IP: net.ParseIP("192.168.1.249"), Mask: lan.Mask}}},
	}
	ip, iface, n := lanOf(ifaces, net.ParseIP("192.168.1.1"), "")
	if ip != "192.168.1.249" || iface != "ovs_eth0" || n == nil || !n.Contains(net.ParseIP("192.168.1.66")) {
		t.Fatal(ip, iface, n)
	}
	// the route's own interface wins over another one on the same network (a macvlan shim)
	ifaces = append([]ifaceAddr{{"shim", []*net.IPNet{{IP: net.ParseIP("192.168.1.250"), Mask: lan.Mask}}}}, ifaces...)
	if ip, iface, _ := lanOf(ifaces, net.ParseIP("192.168.1.1"), "ovs_eth0"); ip != "192.168.1.249" || iface != "ovs_eth0" {
		t.Fatal(ip, iface)
	}
	if ip, _, _ := lanOf(ifaces, nil, ""); ip != "" {
		t.Fatal("sem router não há LAN")
	}
}

// The NPM writes one file per proxy host; other files and odd lines are ignored.
func TestReadProxyHosts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data", "nginx", "proxy_host")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"1.conf": `# ------------------------------------------------------------
# bitwarden.engmariz.com, vault.engmariz.com
# ------------------------------------------------------------
server {
  set $forward_scheme http;
  set $server         "192.168.1.66";
  set $port           8080;
  listen 443 ssl;
  server_name bitwarden.engmariz.com vault.engmariz.com;
}`,
		"2.conf":  "server {\n  set $server immich_server;\n  set $port 2283;\n  server_name immich.engmariz.com;\n}\n",
		"3.conf":  "server {\n  server_name semdestino.engmariz.com;\n}\n", // no $server: skipped
		"x.txt":   "server_name nao.engmariz.com;",
		"4.conf~": "server_name backup.engmariz.com;",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := findProxyHostDir(root); got != dir {
		t.Fatalf("pasta: %q", got)
	}
	hosts := readProxyHosts(dir)
	if len(hosts) != 2 {
		t.Fatalf("%+v", hosts)
	}
	bw := hosts[slices.IndexFunc(hosts, func(h proxyHost) bool { return h.Port == 8080 })]
	if bw.Server != "192.168.1.66" || !slices.Equal(bw.Domains, []string{"bitwarden.engmariz.com", "vault.engmariz.com"}) {
		t.Fatalf("%+v", bw)
	}
	if findProxyHostDir(t.TempDir()) != "" {
		t.Fatal("inventou uma pasta")
	}
}

// docker compose config --format json of unifi (macvlan, fixed IP) and of
// immich (a GPU container beside the one the NPM serves).
const unifiJSON = `{"name":"unifi","services":{"unifi":{"container_name":"unifi","image":"x",
  "networks":{"lan":{"ipv4_address":"192.168.1.92"}},"ports":[{"mode":"ingress","target":8443,"published":"8443","protocol":"tcp"}]}},
  "networks":{"lan":{"name":"lan","driver":"macvlan","driver_opts":{"parent":"enp2s0"}}}}`

const immichJSON = `{"name":"immich","services":{
  "immich-server":{"container_name":"immich_server","image":"x","ports":[{"target":2283,"published":2283}]},
  "immich-machine-learning":{"image":"y","deploy":{"resources":{"reservations":{"devices":[{"capabilities":["gpu"]}]}}}},
  "redis":{"image":"z"}}}`

func TestAnalyzeCompose(t *testing.T) {
	u, err := analyzeCompose([]byte(unifiJSON), "ovs_eth0")
	if err != nil || u.FreeIP != "192.168.1.92" || !strings.Contains(u.overrideFor(nil), "parent: ovs_eth0") || !u.Names["unifi"] || u.Ports[8443] == "" {
		t.Fatalf("unifi: %v %+v", err, u)
	}
	im, err := analyzeCompose([]byte(immichJSON), "ovs_eth0")
	if err != nil || !im.Names["immich_server"] || !im.Names["immich-server"] || im.Ports[2283] != "immich-server" {
		t.Fatalf("immich: %v %+v", err, im)
	}
	if _, err = analyzeCompose([]byte("não é json"), ""); err == nil {
		t.Fatal("aceitou lixo")
	}
	// A service built here or with pull_policy: never is listed, with why the pull skips it.
	b, err := analyzeCompose([]byte(`{"name":"p","services":{"app":{"build":{"context":"."}},"db":{"image":"pg","pull_policy":"never"},"ok":{"image":"redis","build":{"context":"."}}}}`), "")
	if err != nil || !slices.Equal(b.Images, []string{"p-app", "pg", "redis"}) || b.NoPull["p-app"] == "" || b.NoPull["pg"] != "pull_policy: never" || b.NoPull["redis"] != "" {
		t.Fatalf("build/never: %v %v %v", err, b.Images, b.NoPull)
	}
}

func TestMatchHost(t *testing.T) {
	hosts := []proxyHost{
		{Domains: []string{"bitwarden.engmariz.com"}, Server: "192.168.1.66", Port: 8080},
		{Domains: []string{"immich.engmariz.com"}, Server: "immich_server", Port: 2283},
		{Domains: []string{"unifi.engmariz.com"}, Server: "192.168.1.92", Port: 8443},
	}
	vw := composeInfo{Names: map[string]bool{"vaultwarden": true}, keyOf: map[string]string{"vaultwarden": "vaultwarden"}, Ports: map[int]string{8080: "vaultwarden"}}
	im, _ := analyzeCompose([]byte(immichJSON), "")
	u, _ := analyzeCompose([]byte(unifiJSON), "")
	other := composeInfo{Names: map[string]bool{"x": true}, keyOf: map[string]string{"x": "x"}, Ports: map[int]string{9999: "x"}}
	got := assignHosts(hosts, []composeInfo{vw, im, u, other}, "192.168.1.66")
	for i, want := range []string{"bitwarden.engmariz.com", "immich.engmariz.com", "unifi.engmariz.com", ""} {
		if got[i].host != want {
			t.Errorf("%d: queria %q, deu %q", i, want, got[i].host)
		}
	}
}

// The NPM with network_mode: host forwards to 127.0.0.1: the server itself.
func TestMatchLoopback(t *testing.T) {
	hosts := []proxyHost{{Domains: []string{"bitwarden.engmariz.com"}, Server: "127.0.0.1", Port: 8080}}
	vw := composeInfo{Names: map[string]bool{"vaultwarden": true}, keyOf: map[string]string{"vaultwarden": "vaultwarden"}, Ports: map[int]string{8080: "vaultwarden"}}
	if got := assignHosts(hosts, []composeInfo{vw}, "192.168.1.66"); got[0].host != "bitwarden.engmariz.com" {
		t.Fatalf("%+v", got)
	}
	// a proxy host to another machine's IP with the same port is not this compose
	hosts[0].Server = "192.168.1.70"
	if got := assignHosts(hosts, []composeInfo{vw}, "192.168.1.66"); got[0].host != "" {
		t.Fatalf("inventou %+v", got)
	}
}

// A proxy host that names a service key two composes share (app, web) is
// given to neither: a note says so. A container name is specific.
func TestMatchAmbiguous(t *testing.T) {
	a := composeInfo{Names: map[string]bool{"app": true, "wiki": true}, keyOf: map[string]string{"app": "app", "wiki": "app"}, strong: map[string]bool{"wiki": true}, Ports: map[int]string{}}
	b := composeInfo{Names: map[string]bool{"app": true}, keyOf: map[string]string{"app": "app"}, Ports: map[int]string{}}
	hosts := []proxyHost{{Domains: []string{"app.engmariz.com"}, Server: "app", Port: 80}, {Domains: []string{"wiki.engmariz.com"}, Server: "wiki", Port: 80}}
	got := assignHosts(hosts, []composeInfo{a, b}, "")
	if got[0].host != "wiki.engmariz.com" || got[1].host != "" || len(got[1].notes) == 0 {
		t.Fatalf("%+v", got)
	}
}

// The server is the IP most proxy hosts point to on the LAN, never the
// loopback, docker0, the router or the TNAS itself.
func TestServerVote(t *testing.T) {
	var hosts []proxyHost
	for ip, n := range map[string]int{"127.0.0.1": 3, "172.17.0.1": 3, "192.168.1.1": 2, "192.168.1.249": 2, "192.168.1.66": 1} {
		for range n {
			hosts = append(hosts, proxyHost{Domains: []string{"x.engmariz.com"}, Server: ip, Port: 80})
		}
	}
	_, lan, _ := net.ParseCIDR("192.168.1.0/24")
	if got := serverFrom(hosts, nil, lan, "192.168.1.1", "192.168.1.249"); got != "192.168.1.66" {
		t.Fatalf("votou %q", got)
	}
	if got := serverFrom(hosts, nil, nil, "", ""); got != "" {
		t.Fatalf("sem rede, votou %q", got)
	}
}

// A wildcard, a regex or a catch-all server_name is never a service's address:
// a failover would overwrite the zone's wildcard record.
func TestProxyHostWildcards(t *testing.T) {
	h := parseProxyHost([]byte("server {\n set $server 192.168.1.66;\n server_name *.engmariz.com _ ~^(?<s>.+)$ localhost real.engmariz.com;\n}\n"))
	if !slices.Equal(h.Domains, []string{"real.engmariz.com"}) {
		t.Fatalf("%v", h.Domains)
	}
}

// The override disables the GPU container the NPM does not serve, and moves a
// macvlan to the TNAS's LAN interface.
func TestSuggestedOverride(t *testing.T) {
	im, _ := analyzeCompose([]byte(immichJSON), "ovs_eth0")
	ov := im.overrideFor([]string{"immich-server"})
	if !strings.Contains(ov, "immich-machine-learning:") || !strings.Contains(ov, `profiles: ["disabled"]`) || strings.Contains(ov, "immich-server:") {
		t.Fatalf("%s", ov)
	}
	if len(im.Notes) == 0 {
		t.Fatal("sem nota sobre a GPU")
	}
}

func TestSlugName(t *testing.T) {
	for in, want := range map[string]string{"speedtest-tracker": "speedtest-tracker", "Home Assistant": "home-assistant", "_x_": "x_", "Ünifi!": "nifi"} {
		if got := slugName(in); got != want {
			t.Errorf("%q: %q, queria %q", in, got, want)
		}
	}
}

// discoverSetup: a mirror with vaultwarden, unifi, immich, the NPM (with its
// proxy hosts), a compose that does not resolve and a folder without one; a
// route table and interfaces like the TNAS's.
func discoverSetup(t *testing.T) (*Agent, *fake) {
	t.Helper()
	a, f := svcSetup(t)
	root := filepath.Join(a.cfg.Paths.MirrorSubvol, a.cfg.Paths.MirrorRoot)
	for _, d := range []string{"vaultwarden", "unifi", "immich", "npm", "partido", "vazio", "speedtest-tracker"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, d, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ph := filepath.Join(root, "npm", "data", "nginx", "proxy_host")
	if err := os.MkdirAll(ph, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range map[string]string{
		"1": "server {\n set $server \"192.168.1.66\";\n set $port 8080;\n server_name bitwarden.engmariz.com;\n}\n",
		"2": "server {\n set $server immich_server;\n set $port 2283;\n server_name immich.engmariz.com;\n}\n",
		"3": "server {\n set $server 192.168.1.92;\n set $port 8443;\n server_name unifi.engmariz.com;\n}\n",
		"4": "server {\n set $server 192.168.1.66;\n set $port 81;\n server_name nginx.engmariz.com;\n}\n",
		"5": "server {\n set $server \"speedtest\";\n set $port 80;\n server_name speedtest.engmariz.com;\n}\n", // the container's hostname
	} {
		if err := os.WriteFile(filepath.Join(ph, n+".conf"), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	route := filepath.Join(t.TempDir(), "route")
	if err := os.WriteFile(route, []byte(procRoute), 0o600); err != nil {
		t.Fatal(err)
	}
	a.procRoute = route
	_, lan, _ := net.ParseCIDR("192.168.1.0/24")
	a.ifaces = func() []ifaceAddr {
		return []ifaceAddr{{"ovs_eth0", []*net.IPNet{{IP: net.ParseIP("192.168.1.249"), Mask: lan.Mask}}}}
	}
	vwJSON := `{"name":"vw","services":{"vaultwarden":{"container_name":"vaultwarden","ports":[{"published":"8080","target":80}]}}}`
	npmJSON := `{"name":"npm","services":{"app":{"container_name":"npm","ports":[{"published":"81"},{"published":"443"}]}}}`
	at := func(d string) string { return "docker compose -f " + filepath.Join(root, d, "docker-compose.yml") }
	f.outs = map[string]string{at("vaultwarden"): vwJSON, at("unifi"): unifiJSON, at("immich"): immichJSON, at("npm"): npmJSON, at("vazio"): `{"name":"vazio","services":{}}`,
		at("speedtest-tracker"): `{"name":"speedtest-tracker","services":{"speedtest-tracker":{"container_name":"speedtest-tracker","hostname":"speedtest"}}}`}
	api := strings.TrimRight(a.cfg.DNS.APIURL, "/")
	f.bodies = map[string][]byte{
		api + "/api/zones/list?": []byte(`{"status":"ok","response":{"zones":[{"name":"engmariz.com","type":"Primary","internal":false},{"name":"0.in-addr.arpa","type":"Primary","internal":true}]}}`),
		api + "/api/zones/records/get?domain=%2A.engmariz.com&zone=engmariz.com": []byte(`{"status":"ok","response":{"records":[{"name":"*.engmariz.com","type":"A","rData":{"ipAddress":"192.168.1.66"}}]}}`),
	}
	return a, f
}

func TestDiscover(t *testing.T) {
	a, _ := discoverSetup(t)
	d := a.discover()
	n := d.Network
	if n.RouterIP != "192.168.1.1" || n.TNASIP != "192.168.1.249" || n.LANIface != "ovs_eth0" || n.ServerIP != "192.168.1.66" || n.NPMCheckHost != "nginx.engmariz.com" {
		t.Fatalf("rede: %+v", n)
	}
	if d.DNS.Zone != "engmariz.com" || !d.NPM.Found || d.NPM.ProxyHosts != 5 || !d.Mirror.Found {
		t.Fatalf("dns %+v npm %+v espelho %+v", d.DNS, d.NPM, d.Mirror)
	}
	by := map[string]discoveredService{}
	for _, s := range d.Services {
		by[s.Dir] = s
	}
	if by["npm"].Dir != "" || by["vaultwarden"].Dir == "" || by["unifi"].Dir == "" || by["immich"].Dir == "" || by["partido"].Dir == "" {
		t.Fatalf("serviços: %+v", d.Services)
	}
	if s := by["vaultwarden"]; s.Host != "bitwarden.engmariz.com" || !s.Configured || s.Name != "vaultwarden" {
		t.Errorf("vaultwarden: %+v", s)
	}
	if s := by["unifi"]; s.Host != "unifi.engmariz.com" || s.RequireFreeIP != "192.168.1.92" || !strings.Contains(s.OverrideYAML, "parent: ovs_eth0") {
		t.Errorf("unifi: %+v", s)
	}
	if s := by["immich"]; s.Host != "immich.engmariz.com" || !strings.Contains(s.OverrideYAML, "immich-machine-learning") || len(s.Notes) == 0 {
		t.Errorf("immich: %+v", s)
	}
	if s := by["partido"]; s.Error == "" || s.Host != "" {
		t.Errorf("uma pasta que não resolve devia dar erro sem estragar as outras: %+v", s)
	}
	if s := by["speedtest-tracker"]; s.Host != "speedtest.engmariz.com" {
		t.Errorf("o NPM aponta para o hostname do contentor: %+v", s)
	}
	if s := by["vazio"]; !strings.Contains(s.Error, "nenhum serviço") || slices.ContainsFunc(s.Notes, func(n string) bool { return strings.Contains(n, "proxy host") }) {
		t.Errorf("um compose sem serviços: %+v", s)
	}
}

// An ignored folder is listed apart and never read: its compose, broken
// here, gives no error.
func TestDiscoverIgnored(t *testing.T) {
	a, _ := discoverSetup(t)
	a.cfg.Ignored = []string{"partido"}
	i := slices.IndexFunc(a.discover().Services, func(s discoveredService) bool { return s.Dir == "partido" })
	if i < 0 {
		t.Fatal("a pasta ignorada desapareceu da descoberta")
	}
	if s := a.discover().Services[i]; !s.Ignored || s.Error != "" {
		t.Fatalf("%+v", s)
	}
}

// No route (no network yet) and no NPM: empty values, not errors.
func TestDiscoverNothing(t *testing.T) {
	a, _ := svcSetup(t)
	a.procRoute = filepath.Join(t.TempDir(), "nao-existe")
	a.ifaces = func() []ifaceAddr { return nil }
	d := a.discover()
	if d.Network.RouterIP != "" || d.NPM.Found || d.Services == nil {
		t.Fatalf("%+v", d)
	}
}

// The Technitium's wildcard is what clients get (maybe the NPM's own IP); the
// match keeps the IP the proxy hosts point to. And compose names its project
// from the file, not from a -p the agent would make up.
func TestDiscoverDNSOnlyReports(t *testing.T) {
	a, f := discoverSetup(t)
	api := strings.TrimRight(a.cfg.DNS.APIURL, "/")
	f.bodies[api+"/api/zones/records/get?domain=%2A.engmariz.com&zone=engmariz.com"] = []byte(`{"status":"ok","response":{"records":[{"type":"A","rData":{"ipAddress":"192.168.1.67"}}]}}`)
	d := a.discover()
	if d.Network.ServerIP != "192.168.1.67" {
		t.Fatalf("o IP do wildcard não foi dito: %q", d.Network.ServerIP)
	}
	for _, s := range d.Services {
		if s.Dir == "vaultwarden" && s.Host != "bitwarden.engmariz.com" {
			t.Fatalf("a correspondência pela porta perdeu-se: %+v", s)
		}
	}
	for _, c := range f.scans {
		if strings.Contains(c, "config --format json") && strings.Contains(c, " -p ") {
			t.Fatalf("o nome do projeto foi inventado: %s", c)
		}
	}
}

// The discovery says where a copy's data would not come from the mirror,
// and ignores the host's own paths.
func TestDataNotes(t *testing.T) {
	js := `{"name":"fotos","services":{"app":{"image":"x","volumes":[
		{"type":"bind","source":"/Volume1/ServerBackup/homelab/fotos/data"},{"type":"bind","source":"/mnt/fotos"},
		{"type":"bind","source":"/var/run/docker.sock"},{"type":"volume","source":"db"}]}}}`
	info, err := analyzeCompose([]byte(js), "")
	if err != nil {
		t.Fatal(err)
	}
	n := dataNotes(info, "/Volume1/ServerBackup")
	if len(n) != 2 || !strings.Contains(n[0], "os dados de /mnt/fotos não estão no espelho") || !strings.Contains(n[1], "o volume db") {
		t.Fatalf("%q", n)
	}
}
