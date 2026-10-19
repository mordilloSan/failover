package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Discovery: what the TNAS can find out by itself, so the settings are
// proposed and not typed. It only reads; saving goes through the usual
// requests. Everything here is pure, over what discover() gathers.

// defaultRoute is the router's address in /proc/net/route, and the
// interface it is reached by: the default route (destination 0, up, through
// a gateway) with the lowest metric; "" without one.
func defaultRoute(procRoute []byte) (gw, iface string) {
	metric := -1
	sc := bufio.NewScanner(bytes.NewReader(procRoute))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 7 || f[1] != "00000000" {
			continue
		}
		flags, err1 := strconv.ParseUint(f[3], 16, 32)
		m, err2 := strconv.Atoi(f[6])
		b, err3 := hex.DecodeString(f[2])
		if err1 != nil || err2 != nil || err3 != nil || len(b) != 4 || flags&0x3 != 0x3 { // RTF_UP|RTF_GATEWAY
			continue
		}
		if metric < 0 || m < metric {
			gw, iface, metric = net.IPv4(b[3], b[2], b[1], b[0]).String(), f[0], m // little endian
		}
	}
	return gw, iface
}

type ifaceAddr struct {
	name  string
	addrs []*net.IPNet
}

// lanOf is the TNAS's address, interface and network on the router's
// network; the route's own interface wins over another on the same network
// (a macvlan shim, a second card).
func lanOf(ifaces []ifaceAddr, gw net.IP, routeIface string) (ip, iface string, lan *net.IPNet) {
	if gw == nil {
		return "", "", nil
	}
	for _, i := range ifaces {
		for _, n := range i.addrs {
			if n.Contains(gw) && (lan == nil || i.name == routeIface) {
				ip, iface, lan = n.IP.String(), i.name, &net.IPNet{IP: n.IP.Mask(n.Mask), Mask: n.Mask}
			}
		}
	}
	return ip, iface, lan
}

func systemIfaces() []ifaceAddr {
	ifs, _ := net.Interfaces()
	var out []ifaceAddr
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		var nets []*net.IPNet
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				nets = append(nets, n)
			}
		}
		out = append(out, ifaceAddr{i.Name, nets})
	}
	return out
}

// proxyHost is one proxy host of the server's NPM, as its generated config says.
type proxyHost struct {
	Domains []string `json:"domains"`
	Server  string   `json:"server"` // the forward host: an IP or a container's name
	Port    int      `json:"port"`
}

// findProxyHostDir looks for nginx/proxy_host under the NPM's folder in the
// mirror (the data volume's place varies), a few levels down at most; the
// shallowest wins over, say, a backup copy further down.
func findProxyHostDir(npmRoot string) string {
	found, depth := "", 99
	_ = filepath.WalkDir(npmRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(npmRoot, p)
		n := strings.Count(rel, string(filepath.Separator))
		if n >= 4 {
			return filepath.SkipDir
		}
		if filepath.Base(p) == "proxy_host" && filepath.Base(filepath.Dir(p)) == "nginx" && n < depth {
			found, depth = p, n
		}
		return nil
	})
	return found
}

var (
	reServerName = regexp.MustCompile(`^\s*server_name\s+([^;]+);`)
	reServer     = regexp.MustCompile(`^\s*set\s+\$server\s+"?([^";\s]+)"?\s*;`)
	rePort       = regexp.MustCompile(`^\s*set\s+\$port\s+(\d+)\s*;`)
	reDashes     = regexp.MustCompile(`-+`)
	reNotSlug    = regexp.MustCompile(`[^a-z0-9_-]+`)
)

// readProxyHosts reads the NPM's proxy_host/*.conf; a file without a
// forward host or a domain is not a proxy host and is left out.
func readProxyHosts(dir string) []proxyHost {
	files, _ := filepath.Glob(filepath.Join(dir, "*.conf"))
	var out []proxyHost
	for _, f := range files {
		if b, err := os.ReadFile(f); err == nil {
			if h := parseProxyHost(b); h.Server != "" && len(h.Domains) > 0 {
				out = append(out, h)
			}
		}
	}
	return out
}

func parseProxyHost(b []byte) proxyHost {
	var h proxyHost
	for line := range strings.SplitSeq(string(b), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		switch {
		case reServerName.MatchString(line):
			for _, d := range strings.Fields(reServerName.FindStringSubmatch(line)[1]) {
				// a wildcard, a regex or a catch-all is never one service's address:
				// a failover would overwrite the zone's wildcard record
				if !slices.Contains(h.Domains, d) && isName(d) && strings.Contains(d, ".") && !strings.ContainsAny(d, "*~") {
					h.Domains = append(h.Domains, d)
				}
			}
		case reServer.MatchString(line) && h.Server == "":
			h.Server = reServer.FindStringSubmatch(line)[1]
		case rePort.MatchString(line) && h.Port == 0:
			h.Port, _ = strconv.Atoi(rePort.FindStringSubmatch(line)[1])
		}
	}
	return h
}

// composeInfo is what matters of a resolved compose for a failover.
type composeInfo struct {
	Names     map[string]bool   // service keys, container names and hostnames, as a proxy host may name them
	strong    map[string]bool   // the container names and hostnames alone: a bare key (app, web) may be in any compose
	keyOf     map[string]string // any of Names → its service key
	Ports     map[int]string    // published port → service key
	FixedIPs  map[string]string // fixed IP on a macvlan → service key
	FreeIP    string            // the IP a copy must find free (a macvlan's)
	Notes     []string
	lan       string
	macvlans  []string          // networks to move to the TNAS's LAN interface
	heavy     []string          // services with a GPU or devices
	Images    []string          // the image of every service, sorted, once each (built ones by compose's name)
	NoPull    map[string]string // image → why `docker compose pull` leaves it: built here, or pull_policy: never
	External  []string          // external networks: they must exist on the TNAS
	Provided  []string          // networks the compose creates itself: the NPM's *-proxy hub, external to the apps
	Published []pubPort
	Binds     []string // absolute sources of bind mounts
	Named     []string // named volumes: they start empty on the TNAS and go on the return
}

// pubPort is a port a service publishes on its host.
type pubPort struct {
	HostIP, Proto string
	Port          int
}

// analyzeCompose reads `docker compose config --format json`. lanIface is the
// TNAS's LAN interface, for a macvlan's parent.
func analyzeCompose(js []byte, lanIface string) (composeInfo, error) {
	var c struct {
		Name     string                    `json:"name"`
		Services map[string]composeService `json:"services"`
		Networks map[string]struct {
			Name     string `json:"name"`
			Driver   string `json:"driver"`
			External bool   `json:"external"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(js, &c); err != nil {
		return composeInfo{}, fmt.Errorf("docker compose config não deu JSON: %w", err)
	}
	info := composeInfo{Names: map[string]bool{}, strong: map[string]bool{}, keyOf: map[string]string{}, Ports: map[int]string{}, FixedIPs: map[string]string{}, NoPull: map[string]string{}, lan: lanIface}
	for n, net := range c.Networks {
		if net.Driver == "macvlan" || net.Driver == "ipvlan" {
			info.macvlans = append(info.macvlans, n)
		}
		if net.External {
			info.External = append(info.External, cmp.Or(net.Name, n))
		} else {
			info.Provided = append(info.Provided, cmp.Or(net.Name, n))
		}
	}
	slices.Sort(info.macvlans)
	slices.Sort(info.External)
	slices.Sort(info.Provided)
	for _, k := range slices.Sorted(maps.Keys(c.Services)) {
		info.addService(c.Name, k, c.Services[k])
	}
	slices.Sort(info.Images)
	info.Images = slices.Compact(info.Images)
	slices.Sort(info.Binds)
	info.Binds = slices.Compact(info.Binds)
	info.overrideFor(nil) // the notes
	return info, nil
}

// composeService is what analyzeCompose reads of one service.
type composeService struct {
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	PullPolicy    string `json:"pull_policy"`
	Hostname      string `json:"hostname"` // an NPM may point to it: it is as specific as the container name
	Ports         []struct {
		Published any    `json:"published"`
		HostIP    string `json:"host_ip"`
		Protocol  string `json:"protocol"`
	} `json:"ports"`
	Volumes []struct {
		Type   string `json:"type"`
		Source string `json:"source"`
	} `json:"volumes"`
	Networks map[string]*struct {
		IPv4 string `json:"ipv4_address"`
	} `json:"networks"`
	Devices     json.RawMessage `json:"devices"`
	Gpus        json.RawMessage `json:"gpus"`
	Runtime     string          `json:"runtime"`
	NetworkMode string          `json:"network_mode"`
	Deploy      struct {
		Resources struct {
			Reservations struct {
				Devices json.RawMessage `json:"devices"`
			} `json:"reservations"`
		} `json:"resources"`
	} `json:"deploy"`
}

// hardware: the service asks for a GPU or devices of the machine it runs on.
func (s composeService) hardware() bool {
	has := func(r json.RawMessage) bool {
		v := strings.TrimSpace(string(r))
		return v != "" && v != "null" && v != "[]"
	}
	return has(s.Devices) || has(s.Gpus) || has(s.Deploy.Resources.Reservations.Devices) || s.Runtime == "nvidia"
}

func (info *composeInfo) addService(project, k string, s composeService) {
	info.Names[k], info.keyOf[k] = true, k
	strong := []string{s.ContainerName, s.Hostname}
	if project != "" { // compose's own container names
		strong = append(strong, project+"-"+k+"-1", project+"_"+k+"_1")
	}
	for _, n := range strong {
		if n != "" {
			info.Names[n], info.strong[n], info.keyOf[n] = true, true, k
		}
	}
	ref := cmp.Or(s.Image, project+"-"+k) // a built image is named <project>-<service>
	info.Images = append(info.Images, ref)
	switch { // what compose pull skips: the TNAS only gets these by hand
	case s.PullPolicy == "never":
		info.NoPull[ref] = "pull_policy: never"
	case s.Image == "":
		info.NoPull[ref] = "construída no servidor (build:)"
	}
	info.addPortsAndMounts(k, s)
	for n, sn := range s.Networks {
		if sn != nil && sn.IPv4 != "" && slices.Contains(info.macvlans, n) {
			info.FixedIPs[sn.IPv4] = k
			if info.FreeIP == "" {
				info.FreeIP = sn.IPv4
			}
		}
	}
	if s.hardware() {
		info.heavy = append(info.heavy, k)
	}
	if s.NetworkMode == "host" {
		info.Notes = append(info.Notes, k+" usa a rede do anfitrião: no TNAS fica com as portas do TNAS")
	}
}

// addPortsAndMounts reads what service k publishes on its host and mounts.
func (info *composeInfo) addPortsAndMounts(k string, s composeService) {
	for _, p := range s.Ports {
		if port, err := strconv.Atoi(fmt.Sprint(p.Published)); err == nil && port > 0 {
			info.Ports[port] = k
			info.Published = append(info.Published, pubPort{p.HostIP, cmp.Or(p.Protocol, "tcp"), port})
		}
	}
	for _, v := range s.Volumes {
		switch {
		case v.Type == "bind" && filepath.IsAbs(v.Source):
			info.Binds = append(info.Binds, v.Source)
		case v.Type == "volume" && v.Source != "":
			info.Named = append(info.Named, v.Source)
		}
	}
}

// overrideFor is the override for a failover of the compose; served are the
// services the NPM points to: a GPU or device container other than them is
// left off on the TNAS, and a macvlan moves to the TNAS's LAN interface.
func (c *composeInfo) overrideFor(served []string) string {
	var b strings.Builder
	c.Notes = slices.DeleteFunc(c.Notes, func(n string) bool { return strings.Contains(n, "GPU") })
	var off []string
	for _, k := range c.heavy {
		if slices.Contains(served, k) {
			c.Notes = append(c.Notes, k+" usa GPU ou dispositivos do servidor: no TNAS pode não arrancar")
			continue
		}
		off = append(off, k)
		c.Notes = append(c.Notes, k+" usa GPU ou dispositivos do servidor: fica desligado no TNAS")
	}
	if len(off) > 0 {
		b.WriteString("services:\n")
		for _, k := range off {
			fmt.Fprintf(&b, "  %s:\n    profiles: [\"disabled\"]\n", k)
		}
	}
	if len(c.macvlans) > 0 && c.lan != "" {
		b.WriteString("networks:\n")
		for _, n := range c.macvlans {
			fmt.Fprintf(&b, "  %s:\n    driver_opts:\n      parent: %s\n", n, c.lan)
		}
	}
	return b.String()
}

// dataNotes says where a copy's data would not come from the mirror: a bind
// of a folder outside it is the TNAS's own folder there (empty, or another
// one), and a named volume starts empty on the TNAS and goes on the return.
func dataNotes(info composeInfo, mirror string) []string {
	var notes []string
	for _, b := range info.Binds {
		if systemPath(b) || b == mirror || strings.HasPrefix(b, strings.TrimSuffix(mirror, "/")+"/") {
			continue
		}
		notes = append(notes, "os dados de "+b+" não estão no espelho: no TNAS a cópia usa a pasta "+b+" do TNAS (vazia ou outra)")
	}
	for _, v := range info.Named {
		notes = append(notes, "o volume "+v+" não está no espelho: no TNAS nasce vazio e perde-se no regresso")
	}
	return notes
}

// systemPath is a bind of the host itself (the docker socket, the clock,
// devices), the same on the TNAS: never data.
func systemPath(p string) bool {
	for _, s := range []string{"/var/run/", "/run/", "/dev", "/proc", "/sys", "/etc/localtime", "/etc/timezone", "/lib/modules", "/usr/share/zoneinfo"} {
		if strings.HasPrefix(p, s) {
			return true
		}
	}
	return false
}

// assignment is the proxy hosts found for one compose.
type assignment struct {
	host  string   // the first domain
	all   []string // every domain
	keys  []string // the services they point to
	notes []string
}

// serverLocal: from the NPM's side, the server itself: its address, the
// loopback (an NPM with network_mode: host) or docker0.
func serverLocal(ip, serverIP string) bool {
	p := net.ParseIP(ip)
	return p != nil && (ip == serverIP && serverIP != "" || p.IsLoopback() || ip == "172.17.0.1")
}

// strength of the tie between a proxy host and a compose: a container name
// or a macvlan's fixed IP is specific (3), a port the compose publishes on the
// server is fair (2), a bare service key is weak (1): app or web may be in
// any compose.
func strength(h proxyHost, info composeInfo, serverIP string) (int, string) {
	switch {
	case info.strong[h.Server]:
		return 3, info.keyOf[h.Server]
	case info.FixedIPs[h.Server] != "":
		return 3, info.FixedIPs[h.Server]
	case serverLocal(h.Server, serverIP) && info.Ports[h.Port] != "":
		return 2, info.Ports[h.Port]
	case info.Names[h.Server]:
		return 1, info.keyOf[h.Server]
	}
	return 0, ""
}

// assignHosts gives each proxy host to the compose it ties to most. A tie
// between composes gives it to none, with a note: a wrong address ticked in
// the guide is worse than none.
func assignHosts(hosts []proxyHost, infos []composeInfo, serverIP string) []assignment {
	out := make([]assignment, len(infos))
	for _, h := range hosts {
		best, who, keys := 0, []int{}, map[int]string{}
		for i, info := range infos {
			n, k := strength(h, info, serverIP)
			switch {
			case n > best:
				best, who = n, []int{i}
			case n == best && n > 0:
				who = append(who, i)
			}
			keys[i] = k
		}
		if best == 0 {
			continue
		}
		if len(who) > 1 {
			for _, i := range who {
				out[i].notes = append(out[i].notes, "o proxy host "+h.Domains[0]+" aponta para "+h.Server+", que também existe noutro compose: escolhe o endereço à mão")
			}
			continue
		}
		a := &out[who[0]]
		if a.host == "" {
			a.host = h.Domains[0]
		}
		a.all = append(a.all, h.Domains...)
		if !slices.Contains(a.keys, keys[who[0]]) {
			a.keys = append(a.keys, keys[who[0]])
		}
	}
	return out
}

// slugName is a service name from a folder's: lowercase, [a-z0-9_-] only,
// starting with a letter or a number.
func slugName(dir string) string {
	s := reDashes.ReplaceAllString(reNotSlug.ReplaceAllString(strings.ToLower(dir), "-"), "-")
	return strings.TrimRight(strings.TrimLeft(s, "-_"), "-")
}

// discovery is what GET /api/discover answers: proposals, never saved by it.
type discovery struct {
	Network struct {
		RouterIP     string `json:"router_ip"`
		TNASIP       string `json:"tnas_ip"`
		LANIface     string `json:"lan_iface"`
		ServerIP     string `json:"server_ip"`
		NPMCheckHost string `json:"npm_check_host"`
	} `json:"network"`
	DNS struct {
		Zone  string   `json:"zone"`
		Zones []string `json:"zones"`
	} `json:"dns"`
	NPM struct {
		Found      bool `json:"found"`
		ProxyHosts int  `json:"proxy_hosts"`
	} `json:"npm"`
	Mirror struct {
		Found bool   `json:"found"`
		Root  string `json:"root"`
	} `json:"mirror"`
	Services []discoveredService `json:"services"`
}

type discoveredService struct {
	Dir           string   `json:"dir"`
	Name          string   `json:"name"`
	Host          string   `json:"host"`
	Hosts         []string `json:"hosts,omitempty"`
	OverrideYAML  string   `json:"override_yaml,omitempty"`
	RequireFreeIP string   `json:"require_free_ip,omitempty"`
	Notes         []string `json:"notes,omitempty"`
	Configured    bool     `json:"configured"`
	Ignored       bool     `json:"ignored,omitempty"` // left out by hand: listed apart, not resolved
	Error         string   `json:"error,omitempty"`
}

// mirrorDirs are the folders of the mirror with a docker-compose.yml, but the
// NPM's and the clustered ones.
func mirrorDirs(root, npm string) []string {
	var out []string
	ents, _ := os.ReadDir(root) // sorted; unreadable is none
	for _, e := range ents {
		compose := filepath.Join(root, e.Name(), "docker-compose.yml")
		if e.IsDir() && e.Name() != npm && fileExists(compose) && !clustered(compose) {
			out = append(out, e.Name())
		}
	}
	return out
}

// clustered is a compose that runs on the server and on the TNAS at once and
// never fails over: the Technitium, a cluster of the two.
func clustered(compose string) bool {
	b, _ := os.ReadFile(compose)
	return bytes.Contains(b, []byte("technitium/dns-server"))
}

// resolveCompose is `docker compose config --format json` of a folder of the
// mirror, read by analyzeCompose.
func (a *Agent) resolveCompose(root, dir, lan string) (composeInfo, error) {
	// no -p: the project name is compose's own (name: or the folder), the one
	// its container names carry
	out, err := a.sys.Output("docker", "compose", "-f", filepath.Join(root, dir, "docker-compose.yml"), "config", "--format", "json")
	if err != nil {
		return composeInfo{}, err
	}
	return analyzeCompose([]byte(out), lan)
}

// resolveAll resolves the folders' composes, a few at a time; an ignored
// folder is left unread.
func (a *Agent) resolveAll(root string, dirs, ignored []string, lan string) ([]composeInfo, []error) {
	infos, errs := make([]composeInfo, len(dirs)), make([]error, len(dirs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, dir := range dirs {
		if slices.Contains(ignored, dir) {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			infos[i], errs[i] = a.resolveCompose(root, dir, lan)
		})
	}
	wg.Wait()
	return infos, errs
}

// discover gathers what the TNAS can see: its network, the NPM's proxy hosts
// and the composes in the mirror, the Technitium's zones. Outside mu: it runs
// docker compose once per folder.
func (a *Agent) discover() discovery {
	a.mu.Lock()
	c := a.cfg
	running := map[string]bool{} // a running copy holds its own ports
	for _, sv := range c.Services {
		running[sv.Name] = !a.home(a.svc(sv.Name))
	}
	a.mu.Unlock()
	var d discovery
	route, _ := os.ReadFile(a.procRoute)
	var routeIface string
	d.Network.RouterIP, routeIface = defaultRoute(route)
	var lan *net.IPNet
	d.Network.TNASIP, d.Network.LANIface, lan = lanOf(a.ifaces(), net.ParseIP(d.Network.RouterIP), routeIface)

	root := filepath.Join(c.Paths.MirrorSubvol, c.Paths.MirrorRoot)
	d.Mirror.Root = root
	_, err := os.Stat(root)
	d.Mirror.Found = err == nil
	hosts := readProxyHosts(findProxyHostDir(filepath.Join(root, c.NPM.Dir)))
	d.NPM.Found, d.NPM.ProxyHosts = len(hosts) > 0, len(hosts)

	dirs := mirrorDirs(root, c.NPM.Dir)
	infos, errs := a.resolveAll(root, dirs, c.Ignored, d.Network.LANIface)

	// the IP the proxy hosts point to ties them to composes; the Technitium's
	// wildcard, what the clients get, may be the NPM's own and only reports
	voted := serverFrom(hosts, infos, lan, d.Network.RouterIP, d.Network.TNASIP)
	d.Network.ServerIP = voted
	a.discoverDNS(c, hosts, &d)
	assigned := assignHosts(hosts, infos, voted)
	npm, err := a.resolveCompose(root, c.NPM.Dir, "") // unreadable: no names, no networks
	if err == nil {
		for _, h := range hosts {
			if h.Port == 81 || npm.Names[h.Server] { // the NPM's own admin
				d.Network.NPMCheckHost = h.Domains[0]
				break
			}
		}
	}

	d.Services = []discoveredService{}
	for i, dir := range dirs {
		s := discoveredService{Dir: dir, Name: slugName(dir)}
		if j := slices.IndexFunc(c.Services, func(sv Service) bool { return sv.Dir == dir }); j >= 0 {
			s.Name, s.Configured = c.Services[j].Name, true
		}
		if slices.Contains(c.Ignored, dir) {
			s.Ignored = true
			d.Services = append(d.Services, s)
			continue
		}
		if errs[i] != nil {
			s.Error = "o docker compose não leu o compose: " + errs[i].Error()
			d.Services = append(d.Services, s)
			continue
		}
		info, as := infos[i], assigned[i]
		if len(info.keyOf) == 0 { // a failover would start nothing
			s.Error = "o docker compose config não deu nenhum serviço: estão todos com profiles, o ficheiro só tem include, ou falta o .env no espelho?"
			d.Services = append(d.Services, s)
			continue
		}
		s.Host, s.Hosts = as.host, as.all
		s.OverrideYAML, s.RequireFreeIP = info.overrideFor(as.keys), info.FreeIP
		s.Notes = slices.Concat(info.Notes, as.notes, dataNotes(info, c.Paths.MirrorSubvol), a.tnasNotes(info, running[s.Name], npm.Provided))
		if s.Host == "" && len(hosts) > 0 {
			s.Notes = append(s.Notes, "nenhum proxy host do NPM aponta para este compose: o agente só vigia pelo NPM, não o consegue proteger; se é um auxiliar de outro serviço, ignora-o")
		}
		d.Services = append(d.Services, s)
	}
	return d
}

// serverFrom is the server's address: the IP on the TNAS's LAN the proxy
// hosts point to most, leaving out the router, the TNAS itself and the fixed
// IPs of macvlans (those are containers'). The loopback and docker0 are the
// NPM's own machine, but not an address the TNAS can check.
func serverFrom(hosts []proxyHost, infos []composeInfo, lan *net.IPNet, router, tnas string) string {
	if lan == nil {
		return ""
	}
	count := map[string]int{}
	for _, h := range hosts {
		ip := net.ParseIP(h.Server)
		fixed := slices.ContainsFunc(infos, func(i composeInfo) bool { return i.FixedIPs[h.Server] != "" })
		if ip != nil && lan.Contains(ip) && h.Server != router && h.Server != tnas && !fixed {
			count[h.Server]++
		}
	}
	best := ""
	for ip, n := range count {
		if n > count[best] || n == count[best] && ip < best {
			best = ip
		}
	}
	return best
}

// discoverDNS asks the Technitium, with the token there is, for its zones:
// the one that holds the proxy hosts' domains, and its wildcard (the
// server's address, as the clients see it).
func (a *Agent) discoverDNS(c Config, hosts []proxyHost, d *discovery) {
	b, _ := os.ReadFile(c.DNS.TokenFile)
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return
	}
	var zl struct {
		Response struct {
			Zones []techZone `json:"zones"`
		} `json:"response"`
	}
	if technitiumJSON(a.sys, c.DNS.APIURL, "zones/list", nil, tok, &zl) != nil {
		return
	}
	d.DNS.Zones, d.DNS.Zone = pickZone(zl.Response.Zones, hosts)
	best := d.DNS.Zone
	if best == "" {
		return
	}
	var rec struct {
		Response struct {
			Records []struct {
				Type  string `json:"type"`
				RData struct {
					IPAddress string `json:"ipAddress"`
				} `json:"rData"`
			} `json:"records"`
		} `json:"response"`
	}
	if technitiumJSON(a.sys, c.DNS.APIURL, "zones/records/get", url.Values{"domain": {"*." + best}, "zone": {best}}, tok, &rec) == nil {
		for _, r := range rec.Response.Records {
			if r.Type == "A" && isIP(r.RData.IPAddress) {
				d.Network.ServerIP = r.RData.IPAddress // what the clients are sent to wins
				break
			}
		}
	}
}

type techZone struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Internal bool   `json:"internal"`
}

// pickZone: of the primary zones, the one that holds the most proxy hosts'
// domains; with none holding any, only a lone primary zone is a fair guess.
func pickZone(zones []techZone, hosts []proxyHost) (primary []string, best string) {
	most := 0
	for _, z := range zones {
		if z.Internal || z.Type != "Primary" {
			continue
		}
		primary = append(primary, z.Name)
		n := 0
		for _, h := range hosts {
			for _, x := range h.Domains {
				if strings.HasSuffix(x, "."+z.Name) {
					n++
				}
			}
		}
		if n > most {
			best, most = z.Name, n
		}
	}
	if best == "" && len(primary) == 1 {
		best = primary[0]
	}
	return primary, best
}

// technitiumJSON calls /api/<path> of the Technitium and reads its answer into
// out; a status other than ok is the error, with its message.
func technitiumJSON(sys System, apiURL, path string, q url.Values, token string, out any) error {
	body, err := sys.Get(strings.TrimRight(apiURL, "/")+"/api/"+path+"?"+q.Encode(), token)
	if err != nil {
		return err
	}
	var st struct {
		Status       string `json:"status"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return fmt.Errorf("resposta inválida do Technitium: %w", err)
	}
	if st.Status != "ok" {
		return fmt.Errorf("technitium %s: %s", st.Status, st.ErrorMessage)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (a *Agent) getDiscover(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.discover())
}
