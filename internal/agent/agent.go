// Package agent is the failover agent: config, state machine, system calls and web UI.
package agent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// Version is shown in the UI; main sets it from its build-time version.
var Version = "dev"

type Service struct {
	Name          string `yaml:"name" json:"name"`
	Dir           string `yaml:"dir" json:"dir"`
	Host          string `yaml:"host" json:"host"`
	WaitMin       int    `yaml:"wait_min" json:"wait_min"`
	StabilityMin  int    `yaml:"stability_min" json:"stability_min"`
	Override      string `yaml:"override,omitempty" json:"override,omitempty"`
	RequireFreeIP string `yaml:"require_free_ip,omitempty" json:"require_free_ip,omitempty"`
	Icon          string `yaml:"icon,omitempty" json:"icon,omitempty"` // a dashboard-icons name; empty: by the service's name or folder
}

type Config struct {
	Mode   string `yaml:"mode"` // observe: only logs what it would do; auto: acts
	Server struct {
		IP           string `yaml:"ip"`
		NPMCheckHost string `yaml:"npm_check_host"`
	} `yaml:"server"`
	TNASIP          string `yaml:"tnas_ip"`
	RouterIP        string `yaml:"router_ip"`
	LANIface        string `yaml:"lan_iface"`
	CheckIntervalS  int    `yaml:"check_interval_s"`
	StartTimeoutMin int    `yaml:"start_timeout_min"`
	Paths           struct {
		MirrorSubvol string `yaml:"mirror_subvol"`
		MirrorRoot   string `yaml:"mirror_root"`
		SnapshotsDir string `yaml:"snapshots_dir"`
		OverridesDir string `yaml:"overrides_dir"`
		// the mirror unchanged for this many days: the TOS backup stopped (0: 2)
		MirrorStaleDays int `yaml:"mirror_stale_days,omitempty"`
	} `yaml:"paths"`
	NPM struct {
		Dir           string `yaml:"dir"`
		AlertAfterMin int    `yaml:"alert_after_min"`
	} `yaml:"npm"`
	Services    []Service `yaml:"services"`
	Ignored     []string  `yaml:"ignored,omitempty"` // folders of the mirror the discovery and its watch leave out
	Maintenance struct {
		DefaultExpiryMin int `yaml:"default_expiry_min"`
	} `yaml:"maintenance"`
	DNS struct {
		APIURL    string `yaml:"api_url"`
		TokenFile string `yaml:"token_file"`
		Zone      string `yaml:"zone"`
		TTL       int    `yaml:"ttl"`
	} `yaml:"dns"`
	Email   EmailConfig `yaml:"email"`
	Nightly struct {
		PrepullAt string `yaml:"prepull_at"`
	} `yaml:"nightly"`
	UI struct {
		Listen string `yaml:"listen"` // the login is in user.yml, next to this file
	} `yaml:"ui"`
}

// Service names end up in compose project names and snapshot paths.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// FieldError is a validation error of one key of failover.yml, so the UI can
// show it next to that field.
type FieldError struct{ Field, Msg string }

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

func fe(field, msg string) error { return &FieldError{field, msg} }

func isIP(s string) bool { return net.ParseIP(s) != nil }

// isName is a host or zone name: no scheme, port, path or spaces.
func isName(s string) bool { return s != "" && !strings.ContainsAny(s, " /:\t*~") }

func isURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// isRel is a folder inside another one: relative and never above it.
func isRel(s string) bool {
	c := filepath.Clean(s)
	return s != "" && !filepath.IsAbs(s) && c != "." && c != ".." && !strings.HasPrefix(c, "../")
}

func isListen(s string) bool {
	_, port, err := net.SplitHostPort(s)
	n, perr := strconv.Atoi(port)
	return err == nil && perr == nil && n >= 1 && n <= 65535
}

func (c *Config) validate() error {
	for _, r := range []struct {
		bad        bool
		field, msg string
	}{
		{c.Mode != "observe" && c.Mode != "auto", "mode", "tem de ser observe ou auto"},
		{c.CheckIntervalS < 10, "check_interval_s", "tem de ser pelo menos 10"},
		{c.StartTimeoutMin < 1, "start_timeout_min", "tem de ser pelo menos 1"},
		{c.NPM.AlertAfterMin < 0, "npm.alert_after_min", "não pode ser negativo"},
		{c.Maintenance.DefaultExpiryMin < 1 || c.Maintenance.DefaultExpiryMin > 7*24*60, "maintenance.default_expiry_min", "tem de estar entre 1 e 10080"},
		{!isIP(c.Server.IP), "server.ip", "tem de ser um endereço IP"},
		{!isName(c.Server.NPMCheckHost), "server.npm_check_host", "tem de ser um nome, sem https:// nem /"},
		{!isIP(c.TNASIP), "tnas_ip", "tem de ser um endereço IP"},
		{!isIP(c.RouterIP), "router_ip", "tem de ser um endereço IP"},
		{strings.ContainsAny(c.LANIface, " /\t"), "lan_iface", "tem de ser o nome de uma interface, sem espaços"},
		{!filepath.IsAbs(c.Paths.MirrorSubvol), "paths.mirror_subvol", "tem de ser um caminho absoluto"},
		{c.Paths.MirrorRoot != "" && c.Paths.MirrorRoot != "." && !isRel(c.Paths.MirrorRoot), "paths.mirror_root", "tem de ser uma pasta dentro do espelho"},
		{!filepath.IsAbs(c.Paths.SnapshotsDir), "paths.snapshots_dir", "tem de ser um caminho absoluto"},
		{c.Paths.MirrorStaleDays < 0, "paths.mirror_stale_days", "não pode ser negativo"},
		{c.Paths.OverridesDir != "" && !filepath.IsAbs(c.Paths.OverridesDir), "paths.overrides_dir", "tem de ser um caminho absoluto"},
		{!isRel(c.NPM.Dir), "npm.dir", "tem de ser uma pasta dentro do espelho"},
		{!isURL(c.DNS.APIURL), "dns.api_url", "tem de começar por http:// ou https://"},
		{c.DNS.TokenFile == "", "dns.token_file", "é obrigatório"},
		{!isName(c.DNS.Zone), "dns.zone", "tem de ser um nome, sem espaços"},
		{c.DNS.TTL < 1 || c.DNS.TTL > 86400, "dns.ttl", "tem de estar entre 1 e 86400 segundos"},
		{!isListen(c.UI.Listen), "ui.listen", "tem de ser endereço:porta, por exemplo 0.0.0.0:8099"},
	} {
		if r.bad {
			return fe(r.field, r.msg)
		}
	}
	if err := c.Email.validate(); err != nil {
		return err
	}
	if at := c.Nightly.PrepullAt; at != "" {
		if _, err := time.Parse("15:04", at); err != nil || len(at) != 5 {
			return fe("nightly.prepull_at", "tem de ser HH:MM")
		}
	}
	// One address per service (a return deletes the record a failover of
	// another would need) and one folder (else the same containers twice).
	seen, hosts, dirs := map[string]bool{"npm": true}, map[string]bool{}, map[string]bool{filepath.Clean(c.NPM.Dir): true}
	for _, s := range c.Services {
		err := c.validateService(s, seen)
		switch {
		case err != nil:
		case hosts[strings.ToLower(s.Host)]:
			err = fe("host", "já é o endereço de outro serviço")
		case dirs[filepath.Clean(s.Dir)]:
			err = fe("dir", "já é a pasta de outro serviço, ou a do NPM")
		}
		if err != nil {
			return fmt.Errorf("serviço %s: %w", s.Name, err)
		}
		seen[s.Name], hosts[strings.ToLower(s.Host)], dirs[filepath.Clean(s.Dir)] = true, true, true
	}
	return nil
}

func (c *Config) validateService(s Service, seen map[string]bool) error {
	for _, r := range []struct {
		bad        bool
		field, msg string
	}{
		{s.Name == "npm", "name", "npm está reservado para o NPM do TNAS"},
		{!validName.MatchString(s.Name), "name", "só minúsculas, números, - e _, a começar por letra ou número"},
		{seen[s.Name], "name", "já existe um serviço com este nome"},
		{!isRel(s.Dir), "dir", "tem de ser uma pasta dentro do espelho"},
		{s.Host == "", "host", "o nome que o NPM serve: sem ele o agente não consegue verificar o serviço"},
		{!isName(s.Host), "host", "tem de ser um nome, sem https:// nem /"},
		{s.WaitMin < 1, "wait_min", "tem de ser pelo menos 1"},
		{s.StabilityMin < 0, "stability_min", "não pode ser negativa"},
		{s.RequireFreeIP != "" && !isIP(s.RequireFreeIP), "require_free_ip", "tem de ser um endereço IP"},
		{s.RequireFreeIP != "" && c.LANIface == "", "require_free_ip", "precisa da interface da LAN (Definições → Rede)"},
		{s.Icon != "" && !isIconName(s.Icon), "icon", "tem de ser o nome de um ícone do dashboardicons.com"},
	} {
		if r.bad {
			return fe(r.field, r.msg)
		}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true) // a typo in the config must not be silently ignored
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	c.Maintenance.DefaultExpiryMin = cmp.Or(c.Maintenance.DefaultExpiryMin, 60)
	if err := c.validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func saveConfig(path string, c *Config) error {
	var buf bytes.Buffer
	e := yaml.NewEncoder(&buf)
	e.SetIndent(2)
	if err := e.Encode(c); err != nil {
		return err
	}
	return writeAtomic(path, buf.Bytes())
}

// writeAtomic writes path through a temporary file that is fsynced before
// the rename, and the folder after it, so a power cut leaves the old file or
// the new one, never an empty one (as LinuxIO's utils.WriteFileAtomic).
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp) // a leftover one would keep its mode
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	err = cmp.Or(err, f.Sync(), f.Close())
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	chownLikeDir(tmp)
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

const (
	Normal      = "NORMAL"
	FailingOver = "FAILING_OVER"
	Active      = "ACTIVE"
	Returning   = "RETURNING"
	Error       = "ERROR"
)

type SvcState struct {
	State      string    `json:"state"`
	Since      time.Time `json:"since,omitzero"`
	ServerOK   bool      `json:"server_ok"`
	FailSince  time.Time `json:"fail_since,omitzero"`
	OKSince    time.Time `json:"ok_since,omitzero"`
	Snapshot   string    `json:"snapshot,omitempty"`
	DNS        bool      `json:"dns,omitempty"` // A record added, must be removed on return
	Msg        string    `json:"msg,omitempty"`
	MaintUntil time.Time `json:"maint_until,omitzero"`
	Observed   bool      `json:"observed,omitempty"`
	Forced     bool      `json:"forced,omitempty"`   // a forced failover: stays on the TNAS until a forced return
	OnTNAS     time.Time `json:"on_tnas,omitzero"`   // since when the copy serves (ACTIVE), kept until it is back
	LastErr    string    `json:"last_err,omitempty"` // the last failover error: a retry that fails the same way is not emailed again
	Steps      []Step    `json:"steps,omitempty"`    // the last failover's steps, for the page and the email; kept until it is back
	CopyMAC    string    `json:"copy_mac,omitempty"` // the copy's MAC on the fixed IP (require_free_ip): ARP from another MAC means the server's container is back
}

// Step is one step of a failover, done at At.
type Step struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

// markStep records that the failover finished name now, and shows it at once.
func (a *Agent) markStep(s *SvcState, name string) {
	s.Steps = append(s.Steps, Step{name, time.Now()})
	a.publish()
}

// stepTimes is how long each step took: "snapshot 2 s, arranque 34 s".
func stepTimes(steps []Step) string {
	var parts []string
	for i := 1; i < len(steps); i++ {
		sec := int(steps[i].At.Sub(steps[i-1].At).Round(time.Second).Seconds())
		d := fmt.Sprintf("%d s", sec)
		if sec >= 60 {
			d = fmt.Sprintf("%d min %d s", sec/60, sec%60)
		}
		parts = append(parts, steps[i].Name+" "+d)
	}
	return strings.Join(parts, ", ")
}

type Event struct {
	T   time.Time `json:"t"`
	Svc string    `json:"svc,omitempty"`
	Msg string    `json:"msg"`
}

type State struct {
	RouterOK       bool              `json:"router_ok"`
	TNASNetOK      bool              `json:"tnas_net_ok"`   // its Technitium reaches the internet
	ServerNetOK    bool              `json:"server_net_ok"` // same for the server's; both kept as they were while their Technitium is down
	TNASDNSOK      bool              `json:"tnas_dns_ok"`   // its Technitium answers, internet or not: a member of the cluster
	ServerDNSOK    bool              `json:"server_dns_ok"` // same for the server's
	TNASUp         bool              `json:"tnas_up"`       // its LAN IP answers; with RouterOK, it is on the LAN
	ServerUp       bool              `json:"server_up"`     // NPM or ping answered; meaningless without the router
	ServerNPMOK    bool              `json:"server_npm_ok"`
	NPMFailSince   time.Time         `json:"npm_fail_since,omitzero"`
	ServerDown     time.Time         `json:"server_down,omitzero"` // since when neither its NPM nor a ping answers
	ServerDownTold bool              `json:"server_down_told,omitempty"`
	CertBad        map[string]string `json:"cert_bad,omitempty"`      // host → its certificate's problem, told once, across restarts
	MirrorGen      int64             `json:"mirror_gen,omitempty"`    // the mirror's btrfs generation, last seen
	MirrorChanged  time.Time         `json:"mirror_changed,omitzero"` // when it last moved
	MirrorStale    bool              `json:"mirror_stale,omitempty"`  // told that it stopped
	MirrorSeen     map[string]int64  `json:"mirror_seen,omitempty"`   // folder → its compose's mtime, an hour ago
	MirrorNew      []string          `json:"mirror_new,omitempty"`    // new folders, not protected, until seen in the page
	Verdict        Verdict           `json:"verdict,omitzero"`
	NPMAlerted     bool              `json:"npm_alerted,omitempty"`
	RouterChanged  time.Time         `json:"router_changed,omitzero"` // since when the router answers or not
	RouterTold     bool              `json:"router_told,omitempty"`   // its outage was emailed
	TNASNPM        struct {
		Snapshot string    `json:"snapshot,omitempty"`
		Since    time.Time `json:"since,omitzero"`
		OK       bool      `json:"ok"`
		Up       bool      `json:"up,omitempty"` // compose up done: a restart before it starts it again
		Msg      string    `json:"msg,omitempty"`
		// R8: on in place of the server's NPM, until that one answers
		StandIn  bool `json:"stand_in,omitempty"`
		Observed bool `json:"observed,omitempty"` // in observation, told once per outage
	} `json:"tnas_npm"`
	MaintUntil   time.Time            `json:"maint_until,omitzero"`
	LastPull     string               `json:"last_pull,omitempty"`
	Images       map[string]Stack     `json:"images,omitempty"` // by service; "npm" is the TNAS NPM
	ImagesAt     time.Time            `json:"images_at,omitzero"`
	ImagesMsg    string               `json:"images_msg,omitempty"`
	Services     map[string]*SvcState `json:"services"`
	Running      bool                 `json:"running,omitempty"`       // Run is on; still true at a start: the last run crashed
	SetupPending bool                 `json:"setup_pending,omitempty"` // a new install whose first-start guide is not finished; an older state lacks it: done
}

// System is every side effect the agent has, so tests can replace it.
type System interface {
	Run(name string, args ...string) error              // docker, btrfs, arping, ping
	Output(name string, args ...string) (string, error) // same, when stdout matters (image scan)
	Check(host, ip string) error                        // https://host with the connection sent to ip (curl --resolve)
	Get(url, bearer string) ([]byte, error)
	Resolve(ip string) error       // the resolver at ip answers for a name from the internet
	Answers(ip, zone string) error // the DNS server at ip answers for its own zone
	SendMail(m Mail) error
	PortFree(proto, addr string) error // nothing listens on addr of this host (the agent runs with its network)
}

type Agent struct {
	// One lock for config and state. The network probes of a tick run
	// outside it; the actions (compose up/down, btrfs) run inside, so a UI
	// write waits only while a failover or a return is being carried out.
	// Reads use view and never wait.
	mu         sync.Mutex
	cfg        Config
	st         State
	sys        System
	now        time.Time // time of the operation in progress
	cfgPath    string
	statePath  string
	eventsPath string
	evMu       sync.Mutex                // guards events, apart from mu so the page can read them mid-tick
	events     []Event                   // oldest first
	trimmedOn  string                    // the day trimEvents last ran, as 2006-01-02
	tnasIP     atomic.Pointer[string]    // for the certificate, read on handshakes without mu
	restart    func()                    // ends Run so Docker starts the agent again (SetRestart)
	listening  string                    // the UI's address in use, which a saved ui.listen may differ from
	certs      atomic.Pointer[certStore] // set when the UI serves TLS
	iconMu     sync.Mutex                // guards iconRev and the icon files; never held while taking mu
	iconRev    map[string]string         // service → version of its stored icon, for the page's cache
	iconJobs   sync.WaitGroup            // fetchIcons in the background (tests wait for it)
	iconPass   sync.Mutex                // one fetchIcons pass at a time
	alerts     []alertItem               // for the email of this check (flushAlerts)
	mailJobs   sync.WaitGroup            // emails being sent (tests wait for them)
	mailDown   bool                      // the last alerts email failed: told once, and when one goes again
	procRoute  string                    // the route table the discovery reads (tests set another)
	ifaces     func() []ifaceAddr        // the interfaces the discovery reads (tests set others)
	wake       chan struct{}
	view       atomic.Pointer[view]
	api        atomic.Pointer[view] // GET /api/
	pulling    atomic.Bool
	pullNow    bool // set at start: the next tick pulls whatever the hour, so missing or outdated images come down at once
	scanning   atomic.Bool
	rescan     atomic.Bool // asked for while a scan ran
	beats      map[string][]Beat
	tnasSeen   bool // last TNAS ping, so a change is logged once
	routerMs   int  // the router's last ping, for the UI
	userPath   string
	creds      atomic.Pointer[creds] // read by every request, so outside mu; nil: no account yet
	started    time.Time             // the account can be made in the first 30 minutes after it
	setupCode  string                // asked by POST /setup, so a device on the LAN cannot make the account first; written to the log
	lastBeat   atomic.Int64          // the last check or command that ended, as time since started (monotonic), for the watchdog
	watchCfg   atomic.Pointer[watchCfg]
	exit       func(code int)      // os.Exit; tests set another
	saved      []byte              // state.json as last written
	sleep      func(time.Duration) // time.Sleep; tests set one that does not wait
	mirrorAt   time.Time           // when watchMirror last looked
	tickAt     time.Time           // when the running tick started, on the wall clock
	sessions   sessions
}

func NewAgent(cfg Config, cfgPath, statePath string, sys System) (*Agent, error) {
	a := &Agent{cfg: cfg, cfgPath: cfgPath, statePath: statePath, started: time.Now(), wake: make(chan struct{}, 1), now: time.Now(), beats: map[string][]Beat{}, tnasSeen: true}
	a.sys = progress{sys, a}
	a.st.RouterOK, a.st.TNASNetOK, a.st.ServerNetOK, a.st.TNASDNSOK, a.st.ServerDNSOK = true, true, true, true, true
	b, err := os.ReadFile(statePath)
	switch {
	case err == nil:
		// A corrupt state would forget running failovers: refuse to start.
		if err = json.Unmarshal(b, &a.st); err != nil {
			return nil, fmt.Errorf("estado %s corrompido: %w", statePath, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	default:
		a.st.SetupPending = true // no state yet: a new install, which gets the guide
	}
	if a.st.Services == nil {
		a.st.Services = map[string]*SvcState{}
	}
	a.eventsPath = eventsPath(statePath)
	evs, err := loadEvents(a.eventsPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	a.events = evs
	a.trimEvents()
	a.tnasIP.Store(&cfg.TNASIP)
	a.iconRev = map[string]string{}
	if a.st.CertBad == nil {
		a.st.CertBad = map[string]string{}
	}
	a.procRoute, a.ifaces = "/proc/net/route", systemIfaces
	a.userPath = filepath.Join(filepath.Dir(cfgPath), "user.yml")
	c, err := loadUser(a.userPath)
	if err != nil {
		return nil, err
	}
	a.creds.Store(c)
	if c == nil { // whoever can read the container's log may make the account, not any device on the LAN
		a.setupCode = rand.Text()[:12]
		slog.Warn("sem conta: cria-a na interface nos próximos 30 minutos com este código", "code", a.setupCode)
	}
	a.exit, a.sleep = os.Exit, time.Sleep
	if a.st.Running {
		a.crashed()
	}
	a.publish()
	return a, nil
}

// Run ticks until ctx ends. A tick in progress always finishes: stopping in
// the middle of a failover would leave a snapshot the state does not know.
func (a *Agent) Run(ctx context.Context) {
	a.iconJobs.Go(a.fetchIcons)
	a.mu.Lock()
	a.st.Running = true
	a.pullNow = true
	a.save()
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.st.Running = false
		a.save()
		a.mu.Unlock()
		a.mailJobs.Wait() // a queued email is sent before the process ends: its timeouts are far under the stop grace
	}()
	for first := true; ; first = false {
		a.mu.Lock()
		stale := time.Since(a.st.ImagesAt) > 24*time.Hour
		pullSoon := a.pullNow && a.cfg.Nightly.PrepullAt != "" // the start pull scans after itself: one scan, with the images that came down
		a.mu.Unlock()
		if (first || stale) && !pullSoon && !a.scanning.Load() { // at start, after every pull, and once a day at most without one
			go a.scanImages()
		}
		a.Tick(time.Now())
		a.mu.Lock()
		d := time.Duration(a.cfg.CheckIntervalS) * time.Second
		if a.waiting() {
			d = min(d, fastCheck)
		}
		a.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		case <-a.wake:
		}
	}
}

// fastCheck is the check interval while a service waits out wait_min or
// stability_min, or while its copy is starting: its failover or return starts
// seconds after the wait ends, and the DNS changes seconds after the copy
// answers, not up to a whole check interval later. The waits themselves do not change.
const fastCheck = 5 * time.Second

// waiting says whether a service is failing before its failover, starting
// its copy, or back on the server before its return; under mu. One only
// observed (mode observação) or forced has nothing to start, so it does not count.
func (a *Agent) waiting() bool {
	for _, s := range a.st.Services {
		if !s.Observed && (s.State == FailingOver || s.State == Normal && !s.FailSince.IsZero() || s.State == Active && !s.OKSince.IsZero() && !s.Forced) {
			return true
		}
	}
	return false
}

func (a *Agent) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *Agent) Tick(now time.Time) {
	p := a.probe()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now, a.tickAt = now, time.Now()
	a.evaluate(p)
	a.stopIdleNPM()
	a.nightly()
	a.watchMirror()
	a.flushAlerts()
	if a.now.Format(time.DateOnly) != a.trimmedOn {
		a.trimEvents()
	}
	a.save()
	a.beat()
}

func minutes(n int) time.Duration { return time.Duration(n) * time.Minute }

func (a *Agent) svc(name string) *SvcState {
	s := a.st.Services[name]
	if s == nil {
		s = &SvcState{State: Normal}
		a.st.Services[name] = s
	}
	return s
}

func (a *Agent) service(name string) (Service, bool) {
	i := slices.IndexFunc(a.cfg.Services, func(s Service) bool { return s.Name == name })
	if i < 0 {
		return Service{}, false
	}
	return a.cfg.Services[i], true
}

// noteCert tells once that host's certificate stopped verifying (an alert:
// it will not renew itself while it goes unseen) and once that it is back.
func (a *Agent) noteCert(host, bad string) {
	was := a.st.CertBad[host]
	if was == bad {
		return
	}
	if bad == "" {
		delete(a.st.CertBad, host)
		a.event("", "certificado de "+host+" válido outra vez")
		return
	}
	a.st.CertBad[host] = bad
	a.alert("", bad+"; conta como a responder, sem failover")
}

// warn records a repeating problem once, not every tick.
func (a *Agent) warn(msg *string, svc, m string) {
	if *msg != m {
		*msg = m
		a.event(svc, m)
	}
}

func (a *Agent) ping(ip string) bool { return a.pingErr(ip) == nil }

func (a *Agent) pingErr(ip string) error { return a.sys.Run("ping", "-c", "1", "-W", "2", ip) }

// Preflight logs, once at the start, what the agent can reach. A failure is a
// warning: the ticks decide what to do about it.
func (a *Agent) Preflight() {
	a.mu.Lock()
	c := a.cfg
	a.mu.Unlock()
	check := func(what string, err error) {
		if err != nil {
			slog.Warn("arranque: "+what+" falhou", "error", err)
			return
		}
		slog.Info("arranque: " + what)
	}
	check("ping ao router "+c.RouterIP, a.pingErr(c.RouterIP))
	check("ping ao TNAS "+c.TNASIP, a.pingErr(c.TNASIP))
	check("ping ao servidor "+c.Server.IP, a.pingErr(c.Server.IP))
	check("NPM do servidor (https://"+c.Server.NPMCheckHost+")", a.sys.Check(c.Server.NPMCheckHost, c.Server.IP)) // a CertError says so here
	check("internet (registry-1.docker.io)", a.internetErr())
	check("internet pelo DNS do TNAS "+c.TNASIP, a.sys.Resolve(c.TNASIP))
	check("internet pelo DNS do servidor "+c.Server.IP, a.sys.Resolve(c.Server.IP))
	b, _ := os.ReadFile(c.DNS.TokenFile)
	check("token do Technitium ("+c.DNS.APIURL+")", testToken(a.sys, c.DNS.APIURL, c.DNS.Zone, strings.TrimSpace(string(b))))
}

func (a *Agent) inMaint(s *SvcState) bool {
	return !a.st.MaintUntil.IsZero() || !s.MaintUntil.IsZero()
}

// probe is everything a tick asks the network. It runs without the lock:
// with the server down it spends seconds on timeouts.
type probe struct {
	routerOK, npmOK, serverPing, tnasNPMOK bool
	routerMs                               int
	tnasNet, serverNet                     bool // each Technitium reaches the internet
	tnasDNS, serverDNS                     bool // each Technitium answers
	tnasPing                               bool // its own LAN IP is up (answered locally)
	npmMs                                  int
	svcOK                                  map[string]bool
	svcMs                                  map[string]int
	certs                                  map[string]string // host → its certificate's problem, "" when it verified
}

func (a *Agent) probe() probe {
	a.mu.Lock()
	c := a.cfg // the UI replaces a.cfg and never edits it in place, so this copy is safe to read
	tnasNPM := a.st.TNASNPM.Snapshot != ""
	macs := map[string]string{} // service → its copy's MAC, for one with its own IP whose copy is up
	for _, sv := range c.Services {
		if s := a.st.Services[sv.Name]; s != nil && sv.RequireFreeIP != "" && s.CopyMAC != "" {
			macs[sv.Name] = s.CopyMAC
		}
	}
	a.mu.Unlock()

	t0 := time.Now()
	routerOK := a.ping(c.RouterIP)
	// ponytail: wall time of the ping process, a ms or two over the real round trip
	p := probe{routerOK: routerOK, routerMs: int(time.Since(t0).Milliseconds()), tnasPing: a.ping(c.TNASIP), svcOK: map[string]bool{}, svcMs: map[string]int{}, certs: map[string]string{}}
	var certMu sync.Mutex
	check := func(host, ip string) bool {
		bad, err := certOK(a.sys.Check(host, ip))
		if err == nil {
			certMu.Lock()
			p.certs[host] = bad
			certMu.Unlock()
		}
		return err == nil
	}
	timed := func(host, ip string) (bool, int) {
		t := time.Now()
		ok := check(host, ip)
		return ok, int(time.Since(t).Milliseconds())
	}
	if !p.routerOK {
		return p
	}
	var pinged sync.WaitGroup // the ping runs beside the NPM check: with the server down both wait out a timeout
	var ping bool
	pinged.Go(func() { ping = a.ping(c.Server.IP) })
	p.npmOK, p.npmMs = timed(c.Server.NPMCheckHost, c.Server.IP)
	pinged.Wait()
	p.serverPing = !p.npmOK && ping // used only when the NPM failed
	ok, ms := make([]bool, len(c.Services)), make([]int, len(c.Services))
	var wg sync.WaitGroup
	for i, sv := range c.Services {
		wg.Go(func() {
			if p.npmOK { // with the NPM down every service is down for the clients
				ok[i], ms[i] = timed(sv.Host, c.Server.IP)
			}
			ip := sv.RequireFreeIP
			if ip == "" {
				return
			}
			// A service with its own IP is up for what uses that IP (UniFi's devices)
			// while the server's container answers ARP there, whatever the NPM says.
			if mac := macs[sv.Name]; mac != "" { // the copy holds the IP: only another MAC is the server's
				ok[i] = ok[i] && a.serverHoldsIP(c.LANIface, ip, mac)
			} else if !ok[i] {
				t := time.Now()
				ok[i] = a.serverHoldsIP(c.LANIface, ip, "")
				ms[i] = int(time.Since(t).Milliseconds())
			}
		})
	}
	if tnasNPM {
		wg.Go(func() { p.tnasNPMOK = check(c.Server.NPMCheckHost, c.TNASIP) })
	}
	wg.Go(func() { p.tnasNet = a.sys.Resolve(c.TNASIP) == nil })
	wg.Go(func() { p.tnasDNS = a.sys.Answers(c.TNASIP, c.DNS.Zone) == nil })
	if p.npmOK || p.serverPing { // evaluate ignores the server's DNS when it is down: not worth its timeouts
		wg.Go(func() { p.serverNet = a.sys.Resolve(c.Server.IP) == nil })
		wg.Go(func() { p.serverDNS = a.sys.Answers(c.Server.IP, c.DNS.Zone) == nil })
	}
	wg.Wait()
	for i, sv := range c.Services {
		p.svcOK[sv.Name], p.svcMs[sv.Name] = ok[i], ms[i]
	}
	return p
}

var reMAC = regexp.MustCompile(`\[([0-9A-Fa-f:]{17})\]`)

// serverHoldsIP: the fixed IP answers ARP from a MAC other than the copy's, so the
// server's container is back. No answer, or the copy's MAC, means it is not.
func (a *Agent) serverHoldsIP(iface, ip, copyMAC string) bool {
	out, err := a.sys.Output("arping", "-c", "1", "-w", "3", "-I", iface, ip)
	m := reMAC.FindStringSubmatch(out)
	return err == nil && m != nil && !strings.EqualFold(m[1], copyMAC)
}

// internetErr asks Docker Hub, where a failover pulls a missing image, at start. Any
// HTTP answer means it was reached: without a login the registry answers 401.
func (a *Agent) internetErr() error {
	_, err := a.sys.Get("https://registry-1.docker.io/v2/", "")
	if err != nil && strings.HasPrefix(err.Error(), "HTTP ") {
		return nil
	}
	return err
}

// Beat is one check of the server's NPM ("npm") or of a service, for the
// heartbeat bars in the UI. Kept in memory only.
type Beat struct {
	T  time.Time `json:"t"`
	S  string    `json:"s"` // up, down, unknown (not checked)
	Ms int       `json:"ms,omitempty"`
}

const maxBeats = 40

func (a *Agent) record(p probe) {
	add := func(name, s string, ms int) {
		b := a.beats[name]
		b = append(b, Beat{T: a.now, S: s, Ms: ms})
		a.beats[name] = b[max(0, len(b)-maxBeats):]
	}
	status := map[bool]string{true: "up", false: "down"}
	if !p.routerOK {
		add("npm", "unknown", 0)
		for _, sv := range a.cfg.Services {
			add(sv.Name, "unknown", 0)
		}
		return
	}
	add("npm", status[p.npmOK], p.npmMs)
	for _, sv := range a.cfg.Services {
		if _, checked := p.svcOK[sv.Name]; !checked {
			continue // added after this check started: the next one sees it
		}
		add(sv.Name, status[p.svcOK[sv.Name]], p.svcMs[sv.Name]) // false with the NPM down, unless its own IP answers
	}
}

// netSeen notes a box's internet, as its Technitium answered. One that did
// not answer (asked false) says nothing about it: the DNS cluster shows that.
func (a *Agent) netSeen(was *bool, asked, ok bool, who string) {
	if asked && ok != *was {
		*was = ok
		a.event("", map[bool]string{true: who + ": internet acessível", false: who + ": sem internet, o DNS não resolve nomes de fora"}[ok])
	}
}

// routerSeen logs a change of the router and emails an outage that lasts.
func (a *Agent) routerSeen(ok bool) {
	st := &a.st
	if st.RouterChanged.IsZero() { // an older state has no time: counted from this check
		st.RouterChanged = a.now
	}
	if ok != st.RouterOK {
		st.RouterOK, st.RouterChanged = ok, a.now
		a.event("", map[bool]string{true: "router acessível", false: "router inacessível: sem ações"}[ok])
		if ok && st.RouterTold { // the outage was emailed: so is its end
			a.mail("", "router acessível outra vez")
		}
		st.RouterTold = false
	}
	if !st.RouterOK && !st.RouterTold && a.now.After(st.RouterChanged) { // one lost ping is a blip: told at the second check
		st.RouterTold = true
		a.mail("", "router inacessível desde as "+st.RouterChanged.Format("15:04")+": sem ações")
	}
}

func (a *Agent) evaluate(p probe) {
	c, st := &a.cfg, &a.st
	a.record(p)
	for h, bad := range p.certs {
		a.noteCert(h, bad)
	}
	if !st.MaintUntil.IsZero() && !a.now.Before(st.MaintUntil) {
		st.MaintUntil = time.Time{}
		a.event("", "manutenção global terminou")
	}
	for _, sv := range c.Services {
		if s := a.svc(sv.Name); !s.MaintUntil.IsZero() && !a.now.Before(s.MaintUntil) {
			s.MaintUntil = time.Time{}
			a.event(sv.Name, "manutenção terminou")
		}
	}

	// R6: without the router we cannot tell who failed, so do nothing.
	st.TNASUp = p.tnasPing
	if p.tnasPing != a.tnasSeen {
		a.tnasSeen = p.tnasPing
		a.event("", map[bool]string{true: "o TNAS responde no IP da LAN", false: "o TNAS não responde no próprio IP da LAN"}[p.tnasPing])
	}
	a.routerMs = p.routerMs
	a.routerSeen(p.routerOK)
	if !st.RouterOK {
		return
	}
	dnsSeen := func(was *bool, ok bool, who string) {
		if ok != *was {
			*was = ok
			a.event("", map[bool]string{true: "Technitium do " + who + " responde outra vez", false: "Technitium do " + who + " não responde: o cluster DNS só tem o outro"}[ok])
		}
	}
	a.netSeen(&st.TNASNetOK, p.tnasDNS, p.tnasNet, "TNAS")
	dnsSeen(&st.TNASDNSOK, p.tnasDNS, "TNAS")
	if p.npmOK || p.serverPing { // a server that is down says nothing about its internet
		a.netSeen(&st.ServerNetOK, p.serverDNS, p.serverNet, "servidor")
		dnsSeen(&st.ServerDNSOK, p.serverDNS, "servidor")
	}

	a.serverNPM(p)
	for _, sv := range c.Services {
		if _, checked := p.svcOK[sv.Name]; !checked {
			continue // added after this check started: not failed, just not checked yet
		}
		a.step(sv, a.svc(sv.Name), p.svcOK[sv.Name])
	}
	a.standIn()
	if st.TNASNPM.Snapshot != "" {
		st.TNASNPM.OK = p.tnasNPMOK
	}
}

// standInAfter is how long the server's NPM fails before the TNAS NPM
// stands in for it (R8): well inside the ~3 min the Technitium's Failover app
// takes (3 failed tcp443 checks a minute apart) to send the zone's * to the TNAS.
const standInAfter = time.Minute

// standInFail starts the message of a TNAS NPM that R8 could not start.
const standInFail = "NPM do TNAS por arrancar no lugar do NPM do servidor: "

// standIn applies R8: with the server's NPM down, the server alive or not,
// the TNAS NPM runs, so the clients the Failover app sends to the TNAS find
// what does not live on the server (the TNAS, the router...). The services
// themselves still follow R2. It runs until the server's NPM answers again.
func (a *Agent) standIn() {
	st, n := &a.st, &a.st.TNASNPM
	if st.ServerNPMOK {
		n.StandIn, n.Observed = false, false // stopIdleNPM stops it once no copy needs it
		if strings.HasPrefix(n.Msg, standInFail) {
			n.Msg = ""
		}
		return
	}
	if !n.StandIn {
		if st.NPMFailSince.IsZero() || a.now.Sub(st.NPMFailSince) < standInAfter || !st.MaintUntil.IsZero() {
			return
		}
		if a.cfg.Mode != "auto" {
			if !n.Observed {
				n.Observed = true
				a.alert("", "[observação] o NPM do TNAS arrancaria agora: o NPM do servidor não responde")
			}
			return
		}
		n.StandIn = true
		a.event("", "o NPM do servidor não responde: o NPM do TNAS liga-se para os nomes que a app Failover do Technitium manda para o TNAS")
		a.save()
	}
	if _, err := a.ensureNPM(); err != nil {
		if !strings.HasPrefix(n.Msg, standInFail) {
			a.alert("", standInFail+err.Error()+"; tento outra vez em cada verificação")
		}
		n.Msg = standInFail + err.Error()
		return
	}
	if strings.HasPrefix(n.Msg, standInFail) {
		n.Msg = ""
	}
}

// serverNPM applies R2: the server's NPM first. It is the only path to the
// services, so down it takes them all with it, the server alive or not;
// with the server alive the warning says it is the NPM, not the apps.
func (a *Agent) serverNPM(p probe) {
	st := &a.st
	st.ServerNPMOK, st.ServerUp = p.npmOK, p.npmOK || p.serverPing
	switch {
	case !st.ServerUp && st.ServerDown.IsZero():
		st.ServerDown = a.now // one check could be a blip: told at the second
	case !st.ServerUp && !st.ServerDownTold && a.now.After(st.ServerDown):
		st.ServerDownTold = true
		msg := "o servidor não responde (nem o NPM nem o ping) desde as " + st.ServerDown.Format("15:04")
		if st.MaintUntil.IsZero() { // one email; in a maintenance (a planned reboot), the event
			a.alert("", msg)
		} else {
			a.event("", msg+", em manutenção")
		}
	case st.ServerUp && !st.ServerDown.IsZero():
		if st.ServerDownTold {
			a.event("", "o servidor responde outra vez, depois de "+fmtDur(a.now.Sub(st.ServerDown))+" em baixo")
		}
		st.ServerDown, st.ServerDownTold = time.Time{}, false
	}
	if st.ServerNPMOK {
		if st.NPMAlerted {
			a.alert("", "NPM do servidor voltou")
		}
		st.NPMFailSince, st.NPMAlerted = time.Time{}, false
		return
	}
	if st.NPMFailSince.IsZero() {
		st.NPMFailSince = a.now
	}
	if p.serverPing && !st.NPMAlerted && a.now.Sub(st.NPMFailSince) >= minutes(a.cfg.NPM.AlertAfterMin) {
		st.NPMAlerted = true
		a.alert("", "NPM do servidor em falha com o servidor vivo: sem ele nenhum serviço responde, cada um passa ao TNAS no fim da sua espera")
	}
}

// set moves a service to state and saves at once: the page shows every
// step of a tick, and a restart in the middle of one resumes from it.
func (a *Agent) set(s *SvcState, state string) {
	s.State, s.Since = state, a.now
	s.FailSince, s.OKSince, s.Observed, s.Msg = time.Time{}, time.Time{}, false, ""
	if state != FailingOver && state != Active { // a forced failover lasts until it is left
		s.Forced = false
	}
	switch state {
	case FailingOver:
		s.Steps = []Step{{"início", time.Now()}}
	case Active:
		s.OnTNAS = a.now
	case Normal:
		s.OnTNAS, s.LastErr, s.Steps, s.CopyMAC = time.Time{}, "", nil, ""
	case Error:
		s.OnTNAS, s.CopyMAC = time.Time{}, "" // the copy is torn down: nothing of it holds the IP
	}
	a.save()
}

// step advances one service; ok is whether it answered on the server.
//
//nolint:gocognit // the state machine: one case per state reads better than split up
func (a *Agent) step(sv Service, s *SvcState, ok bool) {
	auto := a.cfg.Mode == "auto"
	s.ServerOK = ok
	switch s.State {
	case Normal:
		if ok || a.inMaint(s) {
			if ok && !s.FailSince.IsZero() {
				a.event(sv.Name, "recuperou no servidor antes do failover")
			}
			s.FailSince, s.Observed = time.Time{}, false
			return
		}
		if s.FailSince.IsZero() {
			s.FailSince = a.now
			a.event(sv.Name, "falha no servidor")
		}
		if a.now.Sub(s.FailSince) < minutes(sv.WaitMin) {
			return
		}
		if !auto {
			if !s.Observed {
				s.Observed = true
				a.alert(sv.Name, "[observação] o failover começaria agora")
			}
			return
		}
		a.set(s, FailingOver)
		a.failover(sv, s)
	case FailingOver:
		a.failover(sv, s)
	case Active:
		if !ok {
			s.OKSince = time.Time{}
			return
		}
		if s.OKSince.IsZero() {
			s.OKSince = a.now
		}
		if s.Forced { // asked for by hand: only a forced return ends it
			return
		}
		if a.now.Sub(s.OKSince) < minutes(sv.StabilityMin) {
			return
		}
		if !auto {
			if !s.Observed {
				s.Observed = true
				a.alert(sv.Name, "[observação] o regresso começaria agora")
			}
			return
		}
		a.set(s, Returning)
		a.giveBack(sv, s)
	case Returning:
		a.giveBack(sv, s)
	case Error:
		switch {
		case ok: // healthy on the server again: clean up whatever is left and reset
			a.set(s, Returning)
			a.giveBack(sv, s)
		case auto && !a.inMaint(s) && a.now.Sub(s.Since) >= minutes(a.cfg.StartTimeoutMin):
			a.retryFailover(sv, s)
		}
	}
}

// retryFailover tries again a service in ERROR that is still down on the
// server, as the cause may be gone (the internet back for a pull, a port freed,
// an override fixed).
func (a *Agent) retryFailover(sv Service, s *SvcState) {
	if !a.home(s) { // the cleanup failed last time: it goes first, and a failure waits another timeout
		if err := a.teardown(sv, s); err != nil {
			s.Since = a.now
			a.event(sv.Name, "limpeza da cópia falhou outra vez: "+err.Error())
			return
		}
	}
	a.event(sv.Name, "nova tentativa de failover depois do erro: "+s.Msg)
	a.set(s, FailingOver)
	a.failover(sv, s)
}

// waitCopy asks the copy, through the TNAS NPM, whether it answers (R5):
// the DNS changes only once it does.
func (a *Agent) waitCopy(sv Service, s *SvcState) error {
	c := &a.cfg
	// It is asked again every 5 s for up to 15 s (never more than one check
	// interval), so the DNS changes seconds after the copy answers; a copy that
	// is still starting is asked again 5 s later by the next tick (waiting).
	// ponytail: services are stepped one after another under mu, so a copy
	// that never answers delays the next one by those 15 s each tick until
	// its start timeout; a shared poll over all FAILING_OVER copies if it hurts.
	var err error
	deadline := time.Now().Add(time.Duration(min(c.CheckIntervalS, 15)) * time.Second)
	for try := range max(1, min(c.CheckIntervalS, 15)/copyPoll) {
		if try > 0 {
			if time.Now().Add(copyPoll * time.Second).After(deadline) {
				break
			}
			s.Msg = "à espera da cópia: " + err.Error()
			a.publish()
			a.sleep(copyPoll * time.Second)
			a.beat() // still moving: the watchdog must not see a stuck check
		}
		var bad string
		if bad, err = certOK(a.sys.Check(sv.Host, c.TNASIP)); err == nil {
			a.noteCert(sv.Host, bad)
			if !hasStep(s, "resposta") {
				a.markStep(s, "resposta")
			}
			break
		}
	}
	return err
}

// copyPoll is how often, in seconds, a copy that is starting is asked
// whether it answers.
const copyPoll = 5

func (a *Agent) failover(sv Service, s *SvcState) {
	c := &a.cfg
	if s.Snapshot == "" && !c.hasToken() { // without DNS the copy would serve no one
		a.fail(sv, s, "falta o token do Technitium: põe-no em Definições")
		return
	}
	if s.Snapshot == "" && !a.startCopy(sv, s) {
		return
	}
	// a restart between the snapshot and the copy's start: start it now (both steps can run again)
	if s.Snapshot != "" && !hasStep(s, "arranque") && !a.upCopy(sv, s) {
		return
	}
	err := a.waitCopy(sv, s)
	if err == nil {
		err = a.addDNS(sv, s)
	}
	if err != nil {
		if a.now.Sub(s.Since) >= minutes(c.StartTimeoutMin) {
			a.fail(sv, s, "a cópia não ficou pronta: "+err.Error())
		} else {
			s.Msg = "à espera da cópia: " + err.Error()
		}
		return
	}
	msg := msgFailover
	if t := stepTimes(s.Steps); t != "" {
		msg += " (" + t + ")"
	}
	a.set(s, Active)
	a.alert(sv.Name, msg)
}

// addDNS points the service's name to the TNAS, unless it already does.
func (a *Agent) addDNS(sv Service, s *SvcState) error {
	if hasStep(s, "DNS") {
		return nil
	}
	if !s.DNS { // the record may exist even when the answer is lost: the cleanup must delete it
		s.DNS = true
		a.save()
	}
	if err := a.dns("add", sv.Host); err != nil {
		return fmt.Errorf("DNS: %w", err)
	}
	s.Msg = ""
	a.markStep(s, "DNS")
	a.event(sv.Name, "DNS: "+sv.Host+" → "+a.cfg.TNASIP+" (TNAS)")
	return nil
}

// startCopy starts the TNAS NPM and the service from a new snapshot; false
// means it failed and the service is in ERROR.
func (a *Agent) startCopy(sv Service, s *SvcState) bool {
	if ip := sv.RequireFreeIP; ip != "" { // R3
		if err := a.sys.Run("arping", "-D", "-q", "-c", "2", "-w", "3", "-I", a.cfg.LANIface, ip); err != nil {
			a.fail(sv, s, "IP "+ip+" ocupado, o failover não arranca: "+err.Error())
			return false
		}
	}
	started, err := a.ensureNPM()
	if err != nil {
		a.fail(sv, s, "NPM do TNAS: "+err.Error())
		return false
	}
	if started {
		a.markStep(s, "NPM")
	}
	snap, err := a.snapshot(sv.Name)
	if err != nil {
		a.fail(sv, s, "snapshot: "+err.Error())
		return false
	}
	s.Snapshot = snap
	a.event(sv.Name, "failover iniciado a partir de "+snap)
	a.markStep(s, "snapshot")
	a.save() // a restart from here on knows the snapshot: failover() starts the copy again
	return a.upCopy(sv, s)
}

// upCopy pulls the missing images and starts the copy from its snapshot;
// both can run again, after a restart in the middle. The start timeout
// counts from here: a slow download is not a copy that does not start.
func (a *Agent) upCopy(sv Service, s *SvcState) bool {
	if _, err := a.ensureNPM(); err != nil { // after a restart the NPM may not be up yet either
		a.fail(sv, s, "NPM do TNAS: "+err.Error())
		return false
	}
	files := a.cfg.files(s.Snapshot, sv.Dir, sv.Override)
	if err := a.pullMissing(sv.Name, s, files); err != nil {
		a.fail(sv, s, "sem as imagens: "+err.Error())
		return false
	}
	if err := a.compose(sv.Name, files, "up", "-d"); err != nil {
		a.fail(sv, s, "compose up: "+err.Error())
		return false
	}
	if sv.RequireFreeIP != "" {
		s.CopyMAC = a.copyMAC(sv)
	}
	// the tick's clock, moved by the whole minutes this tick has taken (a slow pull, the NPM's start)
	s.Since = a.now.Add(time.Since(a.tickAt).Truncate(time.Minute))
	a.markStep(s, "arranque")
	return true
}

// copyMAC is the MAC of the copy's container that holds the service's fixed IP; "" when
// Docker does not say (the probe then trusts the check, as before).
func (a *Agent) copyMAC(sv Service) string {
	ids, err := a.sys.Output("docker", "ps", "-q", "--filter", "label=com.docker.compose.project=failover-"+sv.Name)
	if err != nil || strings.TrimSpace(ids) == "" {
		return ""
	}
	args := append([]string{"inspect", "--format", `{{range .NetworkSettings.Networks}}{{.IPAddress}} {{.MacAddress}}{{"\n"}}{{end}}`}, strings.Fields(ids)...)
	out, _ := a.sys.Output("docker", args...)
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == sv.RequireFreeIP {
			return strings.ToLower(f[1])
		}
	}
	return ""
}

func hasStep(s *SvcState, name string) bool {
	return slices.ContainsFunc(s.Steps, func(x Step) bool { return x.Name == name })
}

// pullMissing pulls the images of stack the last scan did not find on the
// TNAS, as a step of its own the page shows.
func (a *Agent) pullMissing(stack string, s *SvcState, files []string) error {
	n := missingImages(a.st.Images[stack])
	if n == 0 {
		return nil
	}
	msg := fmt.Sprintf("a descarregar %d %s em falta no TNAS", n, map[bool]string{true: "imagem", false: "imagens"}[n == 1])
	a.event(stack, msg)
	if s != nil {
		s.Msg = msg + "…"
		a.publish()
	}
	// only the missing ones: a present :latest must stay the version the mirrored data knows
	if err := a.compose(stack, files, "pull", "--policy", "missing", "-q"); err != nil {
		return err
	}
	a.event(stack, "imagens descarregadas")
	if s != nil {
		s.Msg = ""
		a.markStep(s, "imagens")
	}
	return nil
}

func (a *Agent) fail(sv Service, s *SvcState, msg string) {
	a.set(s, Error)
	if err := a.teardown(sv, s); err != nil {
		msg += "; limpeza falhou: " + err.Error()
	}
	s.Msg = msg
	stage, _, _ := strings.Cut(msg, ": ") // compose up, snapshot, IP … ocupado: stderr carries ids that change
	if s.LastErr == stage {               // a retry that failed the same way: in the events, not another email
		a.event(sv.Name, "ERRO: "+msg)
		return
	}
	s.LastErr = stage
	a.alert(sv.Name, "ERRO: "+msg)
}

// teardown undoes a failover in the order R1 requires: DNS, containers
// (down, never just stop), and only then the snapshot under them.
func (a *Agent) teardown(sv Service, s *SvcState) error {
	if s.DNS {
		if err := a.dns("delete", sv.Host); err != nil {
			return fmt.Errorf("DNS: %w", err)
		}
		s.DNS = false
		a.event(sv.Name, "DNS: "+sv.Host+" de volta ao servidor")
	}
	if err := a.down(sv.Name, sv.Name); err != nil {
		return err
	}
	if s.Snapshot != "" {
		if err := a.deleteSnapshot(sv.Name, s.Snapshot); err != nil {
			return err
		}
		s.Snapshot = ""
	}
	return nil
}

func (a *Agent) giveBack(sv Service, s *SvcState) {
	if err := a.teardown(sv, s); err != nil {
		const stuck = "regresso por concluir: "
		if !strings.HasPrefix(s.Msg, stuck) { // clients may still be sent to the TNAS: someone should know, once
			a.alert(sv.Name, stuck+err.Error())
		}
		a.warn(&s.Msg, sv.Name, stuck+err.Error())
		return
	}
	on := a.now.Sub(s.OnTNAS)
	msg := msgBack
	if s.OnTNAS.IsZero() { // a failover that never served
		on = 0
	} else {
		msg += " após " + fmtDur(on) + " no TNAS"
	}
	a.set(s, Normal)
	a.alertFor(sv.Name, msg, on)
}

// ensureNPM starts the TNAS NPM from a snapshot unless it runs; started says
// it had to. A restart after its snapshot but before its start starts it now.
func (a *Agent) ensureNPM() (started bool, err error) {
	n := &a.st.TNASNPM
	if n.Snapshot != "" && n.Up {
		return false, nil
	}
	if n.Snapshot == "" {
		snap, err := a.snapshot("npm")
		if err != nil {
			return false, err
		}
		n.Snapshot, n.Since, n.OK, n.Msg = snap, a.now, false, ""
		a.save()
	}
	// On failure stopIdleNPM cleans it up at the end of the tick.
	files := a.cfg.files(n.Snapshot, a.cfg.NPM.Dir, "")
	if err := a.pullMissing("npm", nil, files); err != nil {
		return false, err
	}
	if err := a.compose("npm", files, "up", "-d"); err != nil {
		return false, err
	}
	n.Up = true
	a.event("", "NPM do TNAS arrancou a partir de "+n.Snapshot)
	a.save()
	return true, nil
}

func (a *Agent) stopIdleNPM() {
	n := &a.st.TNASNPM
	if n.Snapshot == "" || n.StandIn {
		return
	}
	for _, s := range a.st.Services {
		if s.State == FailingOver || s.State == Active || s.State == Returning {
			return
		}
	}
	err := a.down("", "npm")
	if err == nil {
		n.Up, n.OK = false, false // its containers are gone, whatever the snapshot delete does
		err = a.deleteSnapshot("", n.Snapshot)
	}
	if err != nil {
		a.warn(&n.Msg, "", "NPM do TNAS por parar: "+err.Error())
		return
	}
	n.Snapshot, n.Since, n.OK, n.Up, n.Msg = "", time.Time{}, false, false, ""
	a.event("", "NPM do TNAS parado")
}

func (a *Agent) snapshot(name string) (string, error) {
	p := a.cfg.Paths
	dst := filepath.Join(p.SnapshotsDir, "failover-"+name+"-"+a.now.Format("20060102-150405"))
	_ = os.MkdirAll(p.SnapshotsDir, 0o755) // if this fails, btrfs says so
	if err := a.copyMirror(p.MirrorSubvol, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// copyMirror makes a copy's "snapshot": a new subvolume with the mirror
// reflinked into it, not a btrfs snapshot. In a TOS share every new file
// inherits the share's permissions (the + in ls -l), and with them only root
// and each folder's owner get in: a container's own users (postgres,
// rabbitmq...) are refused through parents they do not own. The mirror's files
// have them, and a snapshot keeps them. A new subvolume inherits nothing, even
// inside a share, and cp does not copy them; the reflink shares the data
// blocks (seconds, almost no space).
func (a *Agent) copyMirror(src, dst string) error {
	if err := a.sys.Run("btrfs", "subvolume", "create", dst); err != nil {
		return err
	}
	if err := a.sys.Run("cp", "-dR", "--reflink=always", "--preserve=mode,ownership,timestamps", src+"/.", dst); err != nil {
		_ = a.sys.Run("btrfs", "subvolume", "delete", dst)
		return fmt.Errorf("cópia do espelho com reflink falhou: %w", err)
	}
	return nil
}

// down removes a compose project's containers and named volumes (O3), then
// asks Docker whether any is left: a return is only done when nothing is.
func (a *Agent) down(svc, project string) error {
	if err := a.compose(project, nil, "down", "-v"); err != nil {
		return err
	}
	label := "label=com.docker.compose.project=failover-" + project
	var left []string
	for _, ls := range [][]string{{"ps", "-a", "-q"}, {"volume", "ls", "-q"}} {
		out, err := a.sys.Output("docker", slices.Concat(ls, []string{"--filter", label})...)
		if err != nil {
			return err
		}
		left = append(left, strings.Fields(out)...)
	}
	if left != nil {
		return fmt.Errorf("ficaram containers ou volumes de failover-%s: %s", project, strings.Join(left, " "))
	}
	a.event(svc, "containers e volumes de failover-"+project+" removidos")
	return nil
}

// deleteSnapshot only touches failover-* entries directly inside snapshots_dir:
// btrfs subvolume delete does not ask and must never reach the mirror.
func (a *Agent) deleteSnapshot(svc, p string) error {
	dir := filepath.Clean(a.cfg.Paths.SnapshotsDir)
	if p != filepath.Clean(p) || filepath.Dir(p) != dir || !strings.HasPrefix(filepath.Base(p), "failover-") ||
		p == filepath.Clean(a.cfg.Paths.MirrorSubvol) {
		return fmt.Errorf("recusado apagar %q: não é um snapshot de failover em %s", p, dir)
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := a.sys.Run("btrfs", "subvolume", "delete", p); err != nil {
		return err
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("o snapshot %s continua em %s depois do btrfs subvolume delete", filepath.Base(p), filepath.Dir(p))
	}
	a.event(svc, "snapshot "+filepath.Base(p)+" apagado")
	return nil
}

func (c *Config) files(root, dir, override string) []string {
	f := []string{"-f", filepath.Join(root, c.Paths.MirrorRoot, dir, "docker-compose.yml")}
	if override != "" {
		f = append(f, "-f", filepath.Join(c.Paths.OverridesDir, override))
	}
	return f
}

// compose without files works by project name (used for down).
func (a *Agent) compose(project string, files []string, args ...string) error {
	return a.sys.Run("docker", slices.Concat([]string{"compose", "-p", "failover-" + project}, files, args)...)
}

func (a *Agent) dns(op, host string) error {
	d := a.cfg.DNS
	tok, err := os.ReadFile(d.TokenFile)
	if err != nil {
		return err
	}
	q := url.Values{"domain": {host}, "zone": {d.Zone}, "type": {"A"}, "ipAddress": {a.cfg.TNASIP}}
	if op == "add" {
		q.Set("ttl", strconv.Itoa(d.TTL))
		q.Set("overwrite", "true")
	}
	err = technitiumJSON(a.sys, d.APIURL, "zones/records/"+op, q, strings.TrimSpace(string(tok)), nil)
	if op == "delete" && err != nil && strings.Contains(err.Error(), "no such record exists") {
		return nil
	}
	return err
}

// testToken asks the Technitium for the zone with token: ok means DNS changes will work.
func testToken(sys System, apiURL, zone, token string) error {
	if token == "" {
		return errors.New("falta o token")
	}
	return technitiumJSON(sys, apiURL, "zones/records/get", url.Values{"domain": {zone}, "zone": {zone}}, token, nil)
}

// hasToken reports whether token_file holds a token; the token itself never leaves the file.
func (c *Config) hasToken() bool {
	b, err := os.ReadFile(c.DNS.TokenFile)
	return err == nil && strings.TrimSpace(string(b)) != ""
}

type Image struct {
	Ref     string    `json:"ref"`
	Present bool      `json:"present"`
	NoPull  string    `json:"no_pull,omitempty"` // why the pull leaves it (built, pull_policy: never): it is only on the TNAS by hand
	Size    int64     `json:"size,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

type Stack struct {
	Images []Image  `json:"images"`
	Err    string   `json:"err,omitempty"`
	Notes  []string `json:"notes,omitempty"` // what the TNAS lacks for it to start
}

// scanImages lists, for every stack a failover can start, the images its
// compose files ask for and whether the TNAS already has them (O4). It runs
// outside the lock: a few dozen docker calls.
func (a *Agent) scanImages() {
	if !a.scanning.CompareAndSwap(false, true) {
		a.rescan.Store(true) // the one running has the old config: go again when it ends
		return
	}
	defer func() {
		a.scanning.Store(false)
		if a.rescan.Swap(false) { // asked for just as this one ended
			go a.scanImages()
		}
	}()
	for {
		a.scanOnce()
		if !a.rescan.Swap(false) {
			break
		}
	}
	a.checkReady()
}

func (a *Agent) scanOnce() {
	a.mu.Lock()
	c := a.cfg
	running := map[string]bool{"npm": a.st.TNASNPM.Snapshot != ""}
	starting := false // a copy starting now binds its ports: probing them could make it fail
	for _, sv := range c.Services {
		s := a.svc(sv.Name)
		running[sv.Name] = !a.home(s)
		starting = starting || s.State == FailingOver
	}
	if starting {
		for k := range running {
			running[k] = true
		}
	}
	a.publish() // shows the scan in progress
	a.mu.Unlock()

	var mu sync.Mutex // guards stacks and missing: the stacks are read side by side
	stacks := map[string]Stack{}
	var missing []string
	scan := func(name, dir, override string, provided []string) []string {
		st, miss, prov := a.scanStack(c, name, dir, override, running[name], provided)
		mu.Lock()
		stacks[name], missing = st, append(missing, miss...)
		mu.Unlock()
		return prov
	}
	// the NPM first: the networks its compose creates are external to the apps, and exist only while it runs
	provided := scan("npm", c.NPM.Dir, "", nil)
	var wg sync.WaitGroup
	for _, sv := range c.Services {
		wg.Go(func() { scan(sv.Name, sv.Dir, sv.Override, provided) })
	}
	wg.Wait()
	slices.Sort(missing)
	missing = slices.Compact(missing)

	logImageBalance(stacks)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	a.st.Images, a.st.ImagesAt = stacks, a.now
	switch {
	case missing != nil:
		a.warn(&a.st.ImagesMsg, "", "imagens em falta no TNAS, o failover precisaria da internet: "+strings.Join(missing, ", "))
	case a.st.ImagesMsg != "":
		a.st.ImagesMsg = ""
		a.event("", "todas as imagens estão no TNAS")
	}
	a.save()
}

// scanStack reads one stack's resolved compose: its images and whether the
// TNAS has them, and what the TNAS lacks for it to start. missing are the
// images it lacks, or the stack when its compose does not resolve. provided
// are networks another copy creates, not to ask of the TNAS; the stack's own
// come back.
func (a *Agent) scanStack(c Config, name, dir, override string, running bool, provided []string) (st Stack, missing, own []string) {
	args := slices.Concat([]string{"compose", "-p", "failover-" + name}, c.files(c.Paths.MirrorSubvol, dir, override), []string{"config", "--format", "json"})
	out, err := a.sys.Output("docker", args...)
	var info composeInfo
	if err == nil {
		info, err = analyzeCompose([]byte(out), c.LANIface)
	}
	if err != nil {
		return Stack{Err: err.Error()}, []string{name + " (compose ilegível)"}, nil
	}
	for _, ref := range info.Images {
		img := Image{Ref: ref, NoPull: info.NoPull[ref]}
		if out, err := a.sys.Output("docker", "image", "inspect", "--format", "{{.Size}} {{.Created}}", ref); err == nil {
			img.Present = true
			if f := strings.Fields(out); len(f) == 2 {
				img.Size, _ = strconv.ParseInt(f[0], 10, 64)
				img.Created, _ = time.Parse(time.RFC3339Nano, f[1])
			}
		} else if img.NoPull != "" {
			missing = append(missing, ref+" ("+img.NoPull+", a descarga não a traz)")
		} else {
			missing = append(missing, ref)
		}
		st.Images = append(st.Images, img)
	}
	st.Notes = a.tnasNotes(info, running, provided)
	return st, missing, info.Provided
}

// tnasNotes is what the TNAS lacks for a stack to start there: an external
// network it does not have, a port it publishes that something already
// holds. A running copy holds its own ports, so running skips them. The
// networks the NPM copy creates (provided) are missing between failovers by
// design: creating them by hand would leave the NPM's compose networks it
// did not make.
func (a *Agent) tnasNotes(info composeInfo, running bool, provided []string) []string {
	var notes []string
	for _, n := range info.External {
		if slices.Contains(provided, n) {
			continue
		}
		if a.sys.Run("docker", "network", "inspect", n) != nil {
			notes = append(notes, "a rede "+n+" não existe no TNAS: cria-a com docker network create "+n)
		}
	}
	if running {
		return notes
	}
	for _, p := range info.Published {
		addr, shown := net.JoinHostPort(p.HostIP, strconv.Itoa(p.Port)), strconv.Itoa(p.Port)
		if p.HostIP != "" {
			shown = addr
		}
		switch err := a.sys.PortFree(p.Proto, addr); {
		case errors.Is(err, syscall.EADDRNOTAVAIL): // published on the server's IP: the copy could never bind it
			notes = append(notes, fmt.Sprintf("publica a porta %d no IP %s, que o TNAS não tem: a cópia não arrancaria", p.Port, p.HostIP))
		case err != nil:
			notes = append(notes, fmt.Sprintf("a porta %s/%s já está ocupada no TNAS", shown, p.Proto))
		}
	}
	return notes
}

// logImageBalance logs how many of the images the stacks need the TNAS has.
func logImageBalance(stacks map[string]Stack) { slog.Info("imagens: " + imageBalance(stacks)) }

// imageBalance is how many of the images the stacks need the TNAS has, each
// counted once however many stacks use it: "5 de 6 no TNAS, 2,5 GB".
func imageBalance(stacks map[string]Stack) string {
	present, total, size := countImages(stacks)
	gb := strings.Replace(strconv.FormatFloat(float64(size)/1e9, 'f', 1, 64), ".", ",", 1)
	return fmt.Sprintf("%d de %d no TNAS, %s GB", present, total, gb)
}

// nightly pulls every image once a day after prepull_at, and once at start,
// from the compose files in the mirror, so a failover never needs the
// internet (O4). A pull brings what is missing or outdated; the prune then
// deletes the images it replaced.
func (a *Agent) nightly() {
	at, day := a.cfg.Nightly.PrepullAt, a.now.Format("2006-01-02")
	due := a.st.LastPull != day && a.now.Format("15:04") >= at
	if at == "" || !due && !a.pullNow || !a.pulling.CompareAndSwap(false, true) {
		return
	}
	what := map[bool]string{true: "descarga ao arrancar", false: "descarga noturna"}[a.pullNow]
	a.pullNow = false
	if due { // a start before prepull_at leaves the night's pull, after the mirror, still to do
		a.st.LastPull = day
	}
	root := a.cfg.Paths.MirrorSubvol
	jobs := [][]string{slices.Concat([]string{"compose", "-p", "failover-npm"}, a.cfg.files(root, a.cfg.NPM.Dir, ""), []string{"pull", "-q"})}
	for _, sv := range a.cfg.Services {
		jobs = append(jobs, slices.Concat([]string{"compose", "-p", "failover-" + sv.Name}, a.cfg.files(root, sv.Dir, sv.Override), []string{"pull", "-q"}))
	}
	before := a.st.Images
	_, total, _ := countImages(before)
	slog.Info(what+": a verificar no registo", "imagens", total, "stacks", len(jobs))
	go a.pullAll(what, jobs, before)
}

// pullAll runs the pulls of nightly, prunes what they replaced and tells how
// it went. before is the image scan from before the pulls.
func (a *Agent) pullAll(what string, jobs [][]string, before map[string]Stack) {
	defer a.pulling.Store(false)
	start := time.Now()
	failed := a.pullJobs(what, jobs)
	if out, err := a.sys.Output("docker", "image", "prune", "-f"); err != nil {
		slog.Warn("docker image prune", "error", err)
	} else if _, freed, ok := strings.Cut(strings.TrimSpace(out), "Total reclaimed space: "); ok && freed != "0B" {
		slog.Info(what+": imagens antigas apagadas", "libertado", freed)
	}
	slog.Info(what+": terminou", "demorou", time.Since(start).Round(time.Second).String())
	a.scanImages()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	got := pulled(before, a.st.Images)
	for _, ref := range got {
		slog.Info(what+": imagem descarregada", "imagem", ref)
	}
	for ref, why := range unpulled(a.st.Images) {
		slog.Info(what+": imagem que a descarga não verifica", "imagem", ref, "porquê", why)
	}
	balance := map[bool]string{true: "nada de novo, ", false: fmt.Sprintf("%d descarregadas, ", len(got))}[len(got) == 0] + imageBalance(a.st.Images)
	if failed != nil { // a hole in the safety net: told the same morning
		a.alert("", what+" com falhas ("+balance+"): "+strings.Join(failed, "; "))
	} else {
		a.event("", what+": imagens "+balance)
	}
	a.save()
}

// pullJobs runs each stack's pull, once more after a minute when it fails,
// and is what still failed. No internet stops them all.
func (a *Agent) pullJobs(what string, jobs [][]string) (failed []string) {
	for _, j := range jobs {
		stack := strings.TrimPrefix(j[2], "failover-")
		slog.Info(what+": a verificar atualizações de "+stack, "stack", stack)
		err := a.sys.Run("docker", j...)
		if err != nil && a.internetErr() != nil { // no registry answers: the other stacks and a retry would only wait
			slog.Warn(what+": sem internet, fica para a próxima", "stack", stack, "error", err)
			failed = append(failed, "sem internet")
			break
		}
		if err != nil { // a registry's rate limit (ghcr.io's "retry-after") passes in a minute
			slog.Warn(what+": "+stack+" falhou, outra tentativa daqui a 1 min", "stack", stack, "error", err)
			a.sleep(time.Minute)
			err = a.sys.Run("docker", j...)
		}
		if err != nil {
			failed = append(failed, j[2]+": "+err.Error())
		}
	}
	return failed
}

// unpulled is every image the pull skips, once, with why.
func unpulled(stacks map[string]Stack) map[string]string {
	m := map[string]string{}
	for _, st := range stacks {
		for _, im := range st.Images {
			if im.NoPull != "" {
				m[im.Ref] = im.NoPull
			}
		}
	}
	return m
}

// pulled is every image the TNAS has after a pull that it lacked before, or
// that the pull replaced (another Created): the pull's own output goes by too
// fast to read, the scans before and after it say what came down.
func pulled(before, after map[string]Stack) []string {
	old := map[string]Image{}
	for _, st := range before {
		for _, im := range st.Images {
			old[im.Ref] = im
		}
	}
	var got []string
	for _, st := range after {
		for _, im := range st.Images {
			if o := old[im.Ref]; im.Present && (!o.Present || !im.Created.Equal(o.Created)) {
				got = append(got, im.Ref)
			}
		}
	}
	slices.Sort(got)
	return slices.Compact(got)
}

// save publishes the view and writes state.json, only when it changed: a
// quiet check changes nothing that must survive a restart.
func (a *Agent) save() {
	a.publish()
	b, _ := json.MarshalIndent(&a.st, "", "  ")
	if bytes.Equal(b, a.saved) {
		return
	}
	if err := writeAtomic(a.statePath, b); err != nil {
		slog.Error("guardar estado", "error", err)
		return
	}
	a.saved = b
}

func userOf(c *creds) string {
	if c == nil {
		return ""
	}
	return c.User
}

type certView struct {
	Names    []string  `json:"names"`
	NotAfter time.Time `json:"not_after,omitzero"`
}

// publish renders the UI status once, under the lock, so readers never wait.
func (a *Agent) publish() {
	type svcView struct {
		Service
		*SvcState
		IconV string `json:"icon_v,omitempty"`
	}
	svcs := make([]svcView, 0, len(a.cfg.Services))
	for _, sv := range a.cfg.Services {
		svcs = append(svcs, svcView{sv, a.svc(sv.Name), a.iconV(sv.Name)})
	}
	ui, _ := UIURL(cmp.Or(a.listening, a.cfg.UI.Listen), a.cfg.TNASIP, true)
	a.watchCfg.Store(&watchCfg{a.cfg.Email, time.Duration(a.cfg.CheckIntervalS) * time.Second, ui})
	names, notAfter := a.CertInfo()
	net, dns := a.netDNS()
	api := a.apiSummary(net, dns, notAfter)
	a.api.Store(&view{api, gzipBytes(api)})
	a.evMu.Lock()
	recent := slices.Clone(a.events[max(0, len(a.events)-statusEvents):])
	a.evMu.Unlock()
	// A struct, not a map: omitzero only works on fields, and a zero time
	// sent as "0001-01-01" reads as a date in the page.
	b, _ := json.Marshal(struct {
		Now              time.Time `json:"now"`
		Version          string    `json:"version"`
		User             string    `json:"user"`
		Mode             string    `json:"mode"`
		CheckIntervalS   int       `json:"check_interval_s"`
		StartTimeoutMin  int       `json:"start_timeout_min"`
		DefaultExpiryMin int       `json:"default_expiry_min"`
		DNSToken         bool      `json:"dns_token"`
		APIToken         bool      `json:"api_token"`
		DNSAPIURL        string    `json:"dns_api_url"`
		ServerIP         string    `json:"server_ip"`
		TNASIP           string    `json:"tnas_ip"`
		RouterIP         string    `json:"router_ip"`
		RouterOK         bool      `json:"router_ok"`
		RouterMs         int       `json:"router_ms"`
		Internet         netView   `json:"internet"`
		DNS              dnsView   `json:"dns"`
		TNASUp           bool      `json:"tnas_up"`
		ServerUp         bool      `json:"server_up"`
		ServerNPMOK      bool      `json:"server_npm_ok"`
		NPMFailSince     time.Time `json:"npm_fail_since,omitzero"`
		NPMAlerted       bool      `json:"npm_alerted"`
		TNASNPM          any       `json:"tnas_npm"`
		MaintUntil       time.Time `json:"maint_until,omitzero"`
		LastPull         string    `json:"last_pull,omitempty"`
		PrepullAt        string    `json:"prepull_at,omitempty"`
		NPMCheckHost     string    `json:"npm_check_host"`
		Images           any       `json:"images"`
		ImagesAt         time.Time `json:"images_at,omitzero"`
		ImagesScanning   bool      `json:"images_scanning"`
		DNSZone          string    `json:"dns_zone"`
		Beats            any       `json:"beats"`
		Services         []svcView `json:"services"`
		Events           []Event   `json:"events"`
		Settings         any       `json:"settings"`
		SetupPending     bool      `json:"setup_pending"`
		UIListenRunning  string    `json:"ui_listen_running"`
		Cert             any       `json:"cert"`
		MirrorChanged    time.Time `json:"mirror_changed,omitzero"`
		MirrorStale      bool      `json:"mirror_stale"`
		MirrorNew        []string  `json:"mirror_new"`
		Verdict          Verdict   `json:"verdict"`
	}{
		a.now, Version, userOf(a.creds.Load()), a.cfg.Mode, a.cfg.CheckIntervalS, a.cfg.StartTimeoutMin, a.cfg.Maintenance.DefaultExpiryMin, a.cfg.hasToken(), a.creds.Load() != nil && a.creds.Load().APITokenHash != "", a.cfg.DNS.APIURL,
		a.cfg.Server.IP, a.cfg.TNASIP, a.cfg.RouterIP, a.st.RouterOK, a.routerMs, net, dns, a.st.TNASUp, a.st.ServerUp, a.st.ServerNPMOK, a.st.NPMFailSince, a.st.NPMAlerted,
		a.st.TNASNPM, a.st.MaintUntil, a.st.LastPull, a.cfg.Nightly.PrepullAt, a.cfg.Server.NPMCheckHost,
		a.st.Images, a.st.ImagesAt, a.scanning.Load(), a.cfg.DNS.Zone, a.beats, svcs, recent,
		settingsView(&a.cfg), a.st.SetupPending, a.listening, certView{names, notAfter},
		a.st.MirrorChanged, a.st.MirrorStale, a.st.MirrorNew, a.st.Verdict,
	})
	a.view.Store(&view{b, gzipBytes(b)})
}

// view is the published status, and the same gzipped once for every poll.
type view struct{ raw, gz []byte }
