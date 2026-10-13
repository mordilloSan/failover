package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// fake is the TNAS as the agent sees it: pings, HTTPS checks and commands.
type fake struct {
	mu       sync.Mutex
	cmds     []string        // every command except ping, in order
	gets     []string        // every URL fetched
	badCert  map[string]bool // host → its certificate does not verify
	busy     map[string]bool // "tcp 10.0.0.1:443" → something listens there
	slow     map[string]int  // host@ip → checks it still fails before it answers
	noPing   map[string]bool // ip → does not answer ping
	down     map[string]bool // "host@ip" → HTTPS check fails
	failCmd  []string        // command prefixes that fail
	outs     map[string]string
	scans    []string          // commands run through Output
	badToken string            // the Technitium refuses this bearer token
	getErr   map[string]error  // URL prefix → Get fails with this
	left     map[string]string // compose project → ids docker ps / volume ls still list
	noNet    map[string]bool   // ip → its DNS resolver does not reach the internet
	noDNS    map[string]bool   // ip → its DNS server does not answer at all
	bodies   map[string][]byte // exact URL → what Get answers
	mails    []Mail            // every email sent
	failMail error             // SendMail fails with this
	resolved []string          // every ip whose resolver Resolve asked
}

func (f *fake) SendMail(m Mail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failMail != nil {
		return f.failMail
	}
	f.mails = append(f.mails, m)
	return nil
}

// mailed reports whether an email whose subject or body has sub was sent.
func mailed(f *fake, sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.mails, func(m Mail) bool { return strings.Contains(m.Subject+m.Body, sub) })
}

func (f *fake) PortFree(proto, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[proto+" "+addr] {
		return errors.New("address already in use")
	}
	return nil
}

func (f *fake) Answers(ip, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noDNS[ip] {
		return errors.New("i/o timeout")
	}
	return nil
}

func (f *fake) Resolve(ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, ip)
	if f.noNet[ip] {
		return errors.New("server misbehaving")
	}
	return nil
}

func (f *fake) Run(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "ping" {
		if f.noPing[args[len(args)-1]] {
			return errors.New("sem resposta")
		}
		return nil
	}
	c := strings.Join(append([]string{name}, args...), " ")
	f.cmds = append(f.cmds, c)
	for _, p := range f.failCmd {
		if strings.HasPrefix(c, p) {
			return errors.New("falhou")
		}
	}
	switch {
	case name == "btrfs" && args[1] == "create":
		return os.Mkdir(args[2], 0o755)
	case name == "btrfs" && args[1] == "delete":
		return os.Remove(args[2])
	}
	return nil
}

// Output answers the image scan: outs maps a command prefix to its stdout;
// a command with no entry fails, like docker image inspect of a missing image.
func (f *fake) Output(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := strings.Join(append([]string{name}, args...), " ")
	f.scans = append(f.scans, c)
	if name == "docker" && ((args[0] == "ps" && slices.Contains(args, "-a")) || args[0] == "volume") { // what compose down left behind
		return f.left[strings.TrimPrefix(args[len(args)-1], "label=com.docker.compose.project=")], nil
	}
	best := ""
	for p := range f.outs { // the longest matching prefix wins, whatever the map order
		if strings.HasPrefix(c, p) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return "", errors.New("não existe")
	}
	return f.outs[best], nil
}

func (f *fake) Check(host, ip string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[host+"@"+ip] {
		return errors.New("HTTP 502")
	}
	if n := f.slow[host+"@"+ip]; n > 0 { // a copy that answers after n more checks
		f.slow[host+"@"+ip] = n - 1
		return errors.New("HTTP 502")
	}
	if f.badCert[host] {
		return &CertError{Host: host, Err: errors.New("x509: certificate has expired")}
	}
	return nil
}

func (f *fake) Get(u, bearer string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, u)
	for p, err := range f.getErr {
		if strings.HasPrefix(u, p) {
			return nil, err
		}
	}
	if b, ok := f.bodies[u]; ok {
		return b, nil
	}
	if bearer != "" && bearer == f.badToken {
		return []byte(`{"status":"invalid-token","errorMessage":"Invalid token or session expired."}`), nil
	}
	switch { // a Technitium with the Failover app and the zone's * as the agent writes it
	case strings.HasSuffix(u, "/api/apps/list?"):
		return []byte(`{"status":"ok","response":{"apps":[{"name":"Failover"}]}}`), nil
	case strings.HasSuffix(u, "/api/zones/records/get?domain=%2A.engmariz.com&zone=engmariz.com"):
		return []byte(goodWildcard), nil
	}
	return []byte(`{"status":"ok","ok":true}`), nil
}

// goodWildcard is the zone's * as Definições → DNS → Configurar writes it.
const goodWildcard = `{"status":"ok","response":{"records":[{"type":"APP","ttl":60,"rData":{"appName":"Failover","classPath":"Failover.Address",
	"data":"{\"primary\":[\"192.168.1.66\"],\"secondary\":[\"192.168.1.249\"],\"serverDown\":[\"192.168.1.249\"],\"healthCheck\":\"tcp443\",\"healthCheckUrl\":null,\"allowTxtStatus\":true}"}}]}}`

// ran reports whether a command starting with prefix was run, and its position.
func (f *fake) ran(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.IndexFunc(f.cmds, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func (f *fake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds, f.gets = nil, nil
}

const (
	srv  = "192.168.1.66"
	tnas = "192.168.1.249"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func init() { bcryptCost, loginDelay = bcrypt.MinCost, 0 }

// newTestAgent loads the example config, so the shipped file is tested too.
func newTestAgent(t *testing.T, dir string, f *fake) *Agent {
	t.Helper()
	cfg, err := LoadConfig("testdata/failover.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Mode = "auto"
	cfg.Paths.SnapshotsDir = filepath.Join(dir, "snaps")
	cfg.Nightly.PrepullAt = ""
	cfg.DNS.TokenFile = filepath.Join(dir, "token")
	if err = os.WriteFile(cfg.DNS.TokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := NewAgent(cfg, filepath.Join(dir, "failover.yml"), filepath.Join(dir, "state.json"), f)
	if err != nil {
		t.Fatal(err)
	}
	a.sleep = func(time.Duration) {} // the copy's 5 s polls, instant
	t.Cleanup(a.iconJobs.Wait)       // before the temporary folder goes
	t.Cleanup(a.mailJobs.Wait)
	return a
}

func setup(t *testing.T) (*Agent, *fake) {
	f := &fake{noPing: map[string]bool{}, down: map[string]bool{}}
	return newTestAgent(t, t.TempDir(), f), f
}

// tickTo ticks once a minute from *at until t0+min, inclusive.
func tickTo(a *Agent, at *time.Time, min int) {
	for end := t0.Add(minutes(min)); !at.After(end); *at = at.Add(time.Minute) {
		a.Tick(*at)
	}
}

func state(a *Agent, svc string) string { return a.st.Services[svc].State }

func wantStates(t *testing.T, a *Agent, want map[string]string) {
	t.Helper()
	for svc, st := range want {
		if got := state(a, svc); got != st {
			t.Errorf("%s: estado %s, esperado %s (%s)", svc, got, st, a.st.Services[svc].Msg)
		}
	}
}

func hasGet(f *fake, sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.gets, func(u string) bool { return strings.Contains(u, sub) })
}

func hasEvent(a *Agent, sub string) bool {
	return slices.ContainsFunc(a.events, func(e Event) bool { return strings.Contains(e.Msg, sub) })
}

// A signal (ctx) stops Run after the tick, never in the middle of it.
func TestRunStops(t *testing.T) {
	a, _ := setup(t)
	a.st.ImagesAt = time.Now() // no background scan left running after the test
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run não parou")
	}
	if _, err := os.Stat(a.statePath); err != nil {
		t.Fatalf("o tick não acabou: %v", err)
	}
}

// The start pulls whatever the hour, then prunes what the pull replaced; a
// start before prepull_at leaves the night's pull to do.
func TestPullAtStart(t *testing.T) {
	a, f := setup(t)
	a.cfg.Nightly.PrepullAt = "05:00"
	pulls := func() int {
		for a.pulling.Load() { // the pull runs in the background
			time.Sleep(time.Millisecond)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(f.cmds), func(c string) bool { return c != "docker image prune -f" }))
	}
	night := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	a.Tick(night)
	if pulls() != 0 {
		t.Fatal("descarga antes de prepull_at sem ser o arranque")
	}
	a.pullNow = true
	a.Tick(night)
	if pulls() != 1 || a.st.LastPull != "" || !hasEvent(a, "descarga ao arrancar: imagens") {
		t.Fatalf("arranque: last_pull %q, comandos %v", a.st.LastPull, f.cmds)
	}
	a.Tick(night.Add(2 * time.Hour))
	if pulls() != 2 || a.st.LastPull != "2026-09-27" || !hasEvent(a, "descarga noturna: imagens") {
		t.Fatalf("noite: last_pull %q, comandos %v", a.st.LastPull, f.cmds)
	}
}

// logs sends slog to a buffer for the rest of the test.
func logs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// The start logs what the agent can reach; a failure is a warning, never a stop.
func TestPreflight(t *testing.T) {
	a, f := setup(t)
	buf := logs(t)
	f.noPing[srv] = true
	f.getErr = map[string]error{"https://registry-1.docker.io": errors.New("HTTP 401")} // no login: reached
	f.badToken = "secret"
	a.Preflight()
	out := buf.String()
	for _, want := range []string{
		`level=INFO msg="arranque: ping ao router 192.168.1.1"`,
		`level=INFO msg="arranque: ping ao TNAS 192.168.1.249"`,
		`level=WARN msg="arranque: ping ao servidor 192.168.1.66 falhou"`,
		`level=INFO msg="arranque: NPM do servidor (https://nginx.engmariz.com)"`,
		`level=INFO msg="arranque: internet (registry-1.docker.io)"`,
		`level=WARN msg="arranque: token do Technitium (http://127.0.0.1:5380) falhou" error="technitium invalid-token`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %s em:\n%s", want, out)
		}
	}
}

// R1 checked: a return that leaves a container behind is not finished, and
// every step that is done is in the events.
func TestReturnLeftovers(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	snap := a.st.Services["vaultwarden"].Snapshot
	delete(f.down, "bitwarden.engmariz.com@"+srv)
	f.left = map[string]string{"failover-vaultwarden": "abc123"}
	tickTo(a, &at, 16)
	if s := a.st.Services["vaultwarden"]; s.State != Returning || !strings.Contains(s.Msg, "abc123") || s.Snapshot != snap {
		t.Fatalf("regresso com um container a sobrar: %s %q", s.State, s.Msg)
	}
	f.left = nil
	a.Tick(at)
	wantStates(t, a, map[string]string{"vaultwarden": Normal})
	for _, want := range []string{"containers e volumes de failover-vaultwarden removidos", "snapshot " + filepath.Base(snap) + " apagado",
		"containers e volumes de failover-npm removidos", "NPM do TNAS parado"} {
		if !hasEvent(a, want) {
			t.Errorf("sem o evento %q: %v", want, a.events)
		}
	}
}

// Without the Technitium token a failover goes to ERROR before the snapshot:
// the copy could not be reached by name.
func TestFailoverWithoutToken(t *testing.T) {
	a, f := setup(t)
	if err := os.Remove(a.cfg.DNS.TokenFile); err != nil {
		t.Fatal(err)
	}
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	if s := a.st.Services["vaultwarden"]; s.State != Error || s.Snapshot != "" || !strings.Contains(s.Msg, "token") || f.ran("btrfs subvolume create") >= 0 {
		t.Fatalf("sem token: estado %s, msg %q, comandos %v", s.State, s.Msg, f.cmds)
	}
}

// T-14 + T-20 + R1: only vaultwarden fails; failover at 5 min, return after
// 10 min stable, undone in order: DNS, down, snapshot; then the TNAS NPM.
func TestPartialFailoverAndReturn(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 4)
	if f.ran("") >= 0 {
		t.Fatalf("agiu antes do wait_min: %v", f.cmds)
	}
	tickTo(a, &at, 5)
	wantStates(t, a, map[string]string{"vaultwarden": Active, "homepage": Normal, "immich": Normal, "unifi": Normal})
	npmUp, vwUp := f.ran("docker compose -p failover-npm -f"), f.ran("docker compose -p failover-vaultwarden -f")
	if npmUp < 0 || vwUp < npmUp || f.ran("cp -dR --reflink=always --preserve=mode,ownership,timestamps /Volume1/ServerBackup/.") > npmUp {
		t.Fatalf("arranque errado: %v", f.cmds)
	}
	if !strings.Contains(f.cmds[vwUp], "/homelab/vaultwarden/docker-compose.yml up -d") {
		t.Fatalf("compose errado: %s", f.cmds[vwUp])
	}
	if !hasGet(f, "records/add?") || !hasGet(f, "domain=bitwarden.engmariz.com") || !hasGet(f, "ttl=60") {
		t.Fatalf("DNS não mudou: %v", f.gets)
	}
	if !hasEvent(a, "DNS: bitwarden.engmariz.com → "+tnas) {
		t.Fatalf("sem evento do DNS: %v", a.events)
	}
	snap := a.st.Services["vaultwarden"].Snapshot
	npmSnap := a.st.TNASNPM.Snapshot

	delete(f.down, "bitwarden.engmariz.com@"+srv)
	f.reset()
	tickTo(a, &at, 14) // stable since minute 6: still < 10 min
	if state(a, "vaultwarden") != Active {
		t.Fatalf("voltou antes do stability_min")
	}
	tickTo(a, &at, 16)
	wantStates(t, a, map[string]string{"vaultwarden": Normal})
	down, del := f.ran("docker compose -p failover-vaultwarden down -v"), f.ran("btrfs subvolume delete "+snap)
	if !hasGet(f, "records/delete?") || down < 0 || del < down {
		t.Fatalf("R1 violado: %v", f.cmds)
	}
	if !hasEvent(a, "DNS: bitwarden.engmariz.com de volta ao servidor") {
		t.Fatalf("sem evento do regresso do DNS: %v", a.events)
	}
	if f.ran("docker compose -p failover-npm down -v") < del || f.ran("btrfs subvolume delete "+npmSnap) < 0 {
		t.Fatalf("NPM do TNAS não parou: %v", f.cmds)
	}
	if left, _ := os.ReadDir(a.cfg.Paths.SnapshotsDir); len(left) != 0 {
		t.Fatalf("sobraram snapshots: %v", left)
	}
}

// T-15: server NPM down but the server pings: warn after 3 min, never fail
// over a service. The TNAS NPM stands in once (R8): the Failover app sends
// the zone's * to the TNAS on 443 failing, the host up or not.
func TestServerNPMDownServerAlive(t *testing.T) {
	a, f := setup(t)
	f.down["nginx.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 2)
	if a.st.NPMAlerted {
		t.Fatal("avisou antes de alert_after_min")
	}
	tickTo(a, &at, 60)
	a.mailJobs.Wait()
	if !a.st.NPMAlerted || !mailed(f, "NPM do servidor em falha") {
		t.Fatal("sem aviso do NPM")
	}
	for _, c := range f.cmds {
		if !strings.Contains(c, "failover-npm") {
			t.Fatalf("agiu sobre um serviço no caso 2.3: %v", f.cmds)
		}
	}
	if npmStarts(f) != 1 || !a.st.TNASNPM.StandIn {
		t.Fatalf("o NPM do TNAS não ficou no lugar do do servidor: %d arranques, %+v", npmStarts(f), a.st.TNASNPM)
	}
	// the bars in the UI: the NPM failed, the services were not checked
	if b := a.beats["vaultwarden"]; len(b) != maxBeats || b[len(b)-1].S != "unknown" || a.beats["npm"][maxBeats-1].S != "down" {
		t.Fatalf("histórico das verificações errado: %d %+v", len(b), b[len(b)-1])
	}
}

// A forced failover stays on the TNAS with the server healthy, past the
// stability time, until a forced return.
func TestForcedFailoverStays(t *testing.T) {
	a, f := setup(t)
	if code, body := postTo(t, a.postAction, `{"service":"vaultwarden","action":"failover"}`); code != 204 {
		t.Fatalf("HTTP %d %s", code, body)
	}
	at := t0
	tickTo(a, &at, 60)
	wantStates(t, a, map[string]string{"vaultwarden": Active})
	if !a.st.Services["vaultwarden"].Forced || f.ran("docker compose -p failover-vaultwarden down") >= 0 {
		t.Fatalf("voltou sozinho: %+v", a.st.Services["vaultwarden"])
	}
	if code, body := postTo(t, a.postAction, `{"service":"vaultwarden","action":"return"}`); code != 204 {
		t.Fatalf("HTTP %d %s", code, body)
	}
	tickTo(a, &at, 62)
	wantStates(t, a, map[string]string{"vaultwarden": Normal})
	if a.st.Services["vaultwarden"].Forced {
		t.Fatal("o regresso deixou a marca de forçado")
	}
}

// A certificate that does not verify (every service's, as a wildcard would)
// counts as the service answering: no failover, one alert, one event when it
// verifies again.
func TestBadCertIsUp(t *testing.T) {
	a, f := setup(t)
	f.badCert = map[string]bool{"nginx.engmariz.com": true, "bitwarden.engmariz.com": true}
	at := t0
	tickTo(a, &at, 30)
	a.mailJobs.Wait()
	wantStates(t, a, map[string]string{"vaultwarden": Normal})
	if f.ran("") >= 0 || !a.st.ServerNPMOK {
		t.Fatalf("um certificado inválido contou como falha: %v", f.cmds)
	}
	n := 0
	for _, e := range a.events {
		if strings.Contains(e.Msg, "certificado de bitwarden.engmariz.com inválido") {
			n++
		}
	}
	if n != 1 || !mailed(f, "certificado de nginx.engmariz.com inválido") {
		t.Fatalf("%d eventos do certificado (queria 1), emails %+v", n, f.mails)
	}
	f.mu.Lock()
	f.badCert = nil
	f.mu.Unlock()
	tickTo(a, &at, 31)
	if !hasEvent(a, "certificado de bitwarden.engmariz.com válido outra vez") {
		t.Fatal("sem evento do certificado de volta")
	}
}

// T-16: router unreachable: nothing happens even with everything down.
func TestRouterDown(t *testing.T) {
	a, f := setup(t)
	f.noPing["192.168.1.1"], f.noPing[srv] = true, true
	f.down["nginx.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 60)
	if f.ran("") >= 0 || a.st.RouterOK {
		t.Fatalf("agiu sem router: %v", f.cmds)
	}
}

// T-19: server off: each service moves at its own wait_min, one TNAS NPM.
func TestTotalFailure(t *testing.T) {
	a, f := setup(t)
	f.noPing[srv] = true
	f.down["nginx.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	wantStates(t, a, map[string]string{"vaultwarden": Active, "homepage": Normal})
	tickTo(a, &at, 10)
	wantStates(t, a, map[string]string{"homepage": Active, "immich": Normal})
	tickTo(a, &at, 15)
	wantStates(t, a, map[string]string{"immich": Active, "unifi": Active, "speedtest": Normal})
	tickTo(a, &at, 30)
	wantStates(t, a, map[string]string{"speedtest": Active})
	if a.st.ServerUp || !a.st.TNASUp {
		t.Fatalf("servidor desligado tem de aparecer em baixo e o TNAS em cima: %+v", a.st)
	}
	f.mu.Lock()
	npmStarts := 0
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "docker compose -p failover-npm -f") {
			npmStarts++
		}
	}
	f.mu.Unlock()
	if npmStarts != 1 {
		t.Fatalf("NPM do TNAS arrancou %d vezes", npmStarts)
	}
	if i := f.ran("docker compose -p failover-immich"); !strings.Contains(f.cmds[i], "-f /Volume1/Docker/failover/overrides/immich.override.yml up -d") {
		t.Fatalf("immich sem override: %s", f.cmds[i])
	}
	if f.ran("arping -D -q -c 2 -w 3 -I ovs_eth0 192.168.1.92") < 0 {
		t.Fatal("unifi sem teste de IP duplicado")
	}
}

// npmStarts counts the TNAS NPM's compose up.
func npmStarts(f *fake) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "docker compose -p failover-npm -f") && strings.HasSuffix(c, " up -d") {
			n++
		}
	}
	return n
}

// R8: with the server off the TNAS NPM starts after 1 min, with no failover,
// for the Technitium's Failover app to send the * there. It stays with no
// copy, through a router failure and a server that only pings, and stops
// once the server's NPM answers.
func TestServerDownNPM(t *testing.T) {
	a, f := setup(t)
	a.cfg.Services = nil // nothing to fail over: only R8 starts it
	f.noPing[srv] = true
	f.down["nginx.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 0)
	if npmStarts(f) != 0 {
		t.Fatal("o NPM do TNAS arrancou à primeira verificação falhada")
	}
	tickTo(a, &at, 1)
	if npmStarts(f) != 1 || !a.st.TNASNPM.StandIn {
		t.Fatalf("o NPM do TNAS não arrancou com o servidor em baixo há 1 min: %v", f.cmds)
	}
	f.noPing["192.168.1.1"] = true // without the router nothing is decided, nothing stopped
	tickTo(a, &at, 5)
	delete(f.noPing, "192.168.1.1")
	delete(f.noPing, srv) // it pings, its NPM is still down: the TNAS NPM stays
	tickTo(a, &at, 8)
	if f.ran("docker compose -p failover-npm down") >= 0 {
		t.Fatalf("o NPM do TNAS parou antes do NPM do servidor: %v", f.cmds)
	}
	delete(f.down, "nginx.engmariz.com@"+srv)
	tickTo(a, &at, 9)
	if f.ran("docker compose -p failover-npm down -v") < 0 || a.st.TNASNPM.Snapshot != "" || a.st.TNASNPM.StandIn {
		t.Fatalf("o NPM do TNAS não parou com o servidor de volta: %+v %v", a.st.TNASNPM, f.cmds)
	}
	if npmStarts(f) != 1 {
		t.Fatalf("o NPM do TNAS arrancou %d vezes", npmStarts(f))
	}
}

// R8: a TNAS NPM that does not start is one email and a try at every check.
func TestServerDownNPMFails(t *testing.T) {
	a, f := setup(t)
	a.cfg.Services = nil
	f.noPing[srv] = true
	f.down["nginx.engmariz.com@"+srv] = true
	f.failCmd = []string{"docker compose -p failover-npm"}
	at := t0
	tickTo(a, &at, 4)
	a.mailJobs.Wait()
	f.mu.Lock()
	n := 0
	for _, m := range f.mails {
		n += strings.Count(m.Body, "NPM do TNAS por arrancar")
	}
	f.failCmd = nil
	f.mu.Unlock()
	if n != 1 || npmStarts(f) != 4 || !strings.HasPrefix(a.st.TNASNPM.Msg, "NPM do TNAS por arrancar") {
		t.Fatalf("%d avisos, %d tentativas, msg %q", n, npmStarts(f), a.st.TNASNPM.Msg)
	}
	tickTo(a, &at, 5)
	if npmStarts(f) != 5 || !a.st.TNASNPM.Up || a.st.TNASNPM.Msg != "" {
		t.Fatalf("não arrancou à tentativa seguinte: %+v", a.st.TNASNPM)
	}
}

// R8: in observation it is one email; in a global maintenance, nothing.
func TestServerDownNPMHeld(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(a *Agent)
		mail int
	}{
		{"observação", func(a *Agent) { a.cfg.Mode = "observe" }, 1},
		{"manutenção global", func(a *Agent) { a.st.MaintUntil = t0.Add(time.Hour) }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, f := setup(t)
			a.cfg.Services = nil
			c.set(a)
			f.noPing[srv] = true
			f.down["nginx.engmariz.com@"+srv] = true
			at := t0
			tickTo(a, &at, 10)
			a.mailJobs.Wait()
			n := 0
			for _, m := range f.mails {
				n += strings.Count(m.Body, "[observação] o NPM do TNAS arrancaria agora")
			}
			if npmStarts(f) != 0 || n != c.mail {
				t.Fatalf("%d arranques, %d avisos de observação", npmStarts(f), n)
			}
		})
	}
}

// T-11 / R3: the .92 answers: unifi does not start and nothing is left behind.
func TestUnifiIPBusy(t *testing.T) {
	a, f := setup(t)
	f.failCmd = []string{"arping"}
	f.down["unifi.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 15)
	wantStates(t, a, map[string]string{"unifi": Error})
	if f.ran("btrfs subvolume create") >= 0 || f.ran("docker compose -p failover-unifi -f") >= 0 {
		t.Fatalf("arrancou com o IP ocupado: %v", f.cmds)
	}
	tickTo(a, &at, 40)
	// tried again every start_timeout_min (10) while the server is down, never every tick
	if n := strings.Count(strings.Join(f.cmds, "\n"), "arping"); n < 2 || n > 4 {
		t.Fatalf("%d arpings em 25 min de ERROR: queria uma nova tentativa a cada 10 min", n)
	}
	delete(f.down, "unifi.engmariz.com@"+srv)
	tickTo(a, &at, 41)
	wantStates(t, a, map[string]string{"unifi": Normal})
}

// The copy never gets healthy: ERROR after start_timeout_min, everything removed.
func TestStartTimeout(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.down["bitwarden.engmariz.com@"+tnas] = true
	at := t0
	tickTo(a, &at, 14)
	wantStates(t, a, map[string]string{"vaultwarden": FailingOver})
	tickTo(a, &at, 15)
	wantStates(t, a, map[string]string{"vaultwarden": Error})
	if hasGet(f, "records/add") {
		t.Fatal("DNS mudado sem cópia saudável")
	}
	if a.st.Services["vaultwarden"].Snapshot != "" || a.st.TNASNPM.Snapshot != "" {
		t.Fatal("snapshots não apagados")
	}
	if left, _ := os.ReadDir(a.cfg.Paths.SnapshotsDir); len(left) != 0 {
		t.Fatalf("sobraram snapshots: %v", left)
	}
}

// A copy that answers a few seconds after compose up is caught in the same
// tick, polled every 5 s: the DNS does not wait for the next check.
func TestCopyPolledInTick(t *testing.T) {
	a, f := setup(t)
	var slept []time.Duration
	a.sleep = func(d time.Duration) { slept = append(slept, d) }
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.slow = map[string]int{"bitwarden.engmariz.com@" + tnas: 2}
	at := t0
	for end := t0.Add(minutes(10)); !at.After(end); at = at.Add(time.Minute) {
		a.Tick(at)
		if st := state(a, "vaultwarden"); st == FailingOver {
			t.Fatalf("%s: a cópia respondeu dentro do tick e o estado ficou %s", at.Format("15:04"), st)
		}
	}
	wantStates(t, a, map[string]string{"vaultwarden": Active})
	if len(slept) != 2 || slept[0] != copyPoll*time.Second {
		t.Fatalf("esperas entre verificações da cópia: %v", slept)
	}
}

// state.json is written when it changes, not on every quiet check.
func TestSaveOnlyWhenChanged(t *testing.T) {
	a, _ := setup(t)
	a.Tick(t0)
	if err := os.Remove(a.statePath); err != nil {
		t.Fatal(err)
	}
	a.Tick(t0.Add(time.Minute))
	if fileExists(a.statePath) {
		t.Fatal("state.json reescrito sem mudanças")
	}
	a.mu.Lock()
	a.st.MaintUntil = t0.Add(time.Hour)
	a.save()
	a.mu.Unlock()
	if !fileExists(a.statePath) {
		t.Fatal("state.json não gravado depois de uma mudança")
	}
}

// Images the scan did not find are pulled as a step of their own before
// compose up; a pull that fails is the failover's error.
func TestPullMissingBeforeUp(t *testing.T) {
	a, f := setup(t)
	a.st.Images = map[string]Stack{"vaultwarden": {Images: []Image{{Ref: "vaultwarden/server:latest"}}}}
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 6)
	wantStates(t, a, map[string]string{"vaultwarden": Active})
	pull, up := f.ran("docker compose -p failover-vaultwarden -f "), -1
	for i, c := range f.cmds {
		if strings.HasPrefix(c, "docker compose -p failover-vaultwarden") && strings.HasSuffix(c, "up -d") {
			up = i
		}
	}
	if pull < 0 || !strings.HasSuffix(f.cmds[pull], "pull --policy missing -q") || up < pull || !hasEvent(a, "a descarregar 1 imagem em falta no TNAS") {
		t.Fatalf("pull antes do up: %v", f.cmds)
	}

	b, g := setup(t)
	b.st.Images = a.st.Images
	g.down["bitwarden.engmariz.com@"+srv] = true
	g.failCmd = []string{"docker compose -p failover-vaultwarden -f"}
	at = t0
	tickTo(b, &at, 6)
	if s := b.st.Services["vaultwarden"]; s.State != Error || !strings.Contains(s.Msg, "sem as imagens") {
		t.Fatalf("pull falhado: %s %q", s.State, s.Msg)
	}
}

// T-17: maintenance blocks failovers while on; wait_min counts again after it expires.
func TestMaintenance(t *testing.T) {
	a, f := setup(t)
	a.st.MaintUntil = t0.Add(30 * time.Minute)
	a.st.Services["homepage"] = &SvcState{State: Normal, MaintUntil: t0.Add(90 * time.Minute)}
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.down["homepage.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 34)
	if f.ran("") >= 0 || !a.st.MaintUntil.IsZero() {
		t.Fatalf("failover durante a manutenção ou sem expirar: %v", f.cmds)
	}
	tickTo(a, &at, 35)
	wantStates(t, a, map[string]string{"vaultwarden": Active, "homepage": Normal})
	tickTo(a, &at, 99)
	wantStates(t, a, map[string]string{"homepage": Normal})
	tickTo(a, &at, 100) // its maintenance ended at 90
	wantStates(t, a, map[string]string{"homepage": Active})
}

// F2: observe mode only logs, once.
func TestObserveMode(t *testing.T) {
	a, f := setup(t)
	a.cfg.Mode = "observe"
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 60)
	if f.ran("") >= 0 {
		t.Fatalf("agiu em observação: %v", f.cmds)
	}
	n := 0
	for _, e := range a.events {
		if strings.HasPrefix(e.Msg, "[observação]") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d eventos de observação, esperado 1", n)
	}
}

// T-21 / R7: a restarted agent picks up an active failover from disk.
func TestRestartResumes(t *testing.T) {
	dir := t.TempDir()
	f := &fake{noPing: map[string]bool{}, down: map[string]bool{"bitwarden.engmariz.com@" + srv: true}}
	a := newTestAgent(t, dir, f)
	at := t0
	tickTo(a, &at, 5)
	snap := a.st.Services["vaultwarden"].Snapshot

	b := newTestAgent(t, dir, f)
	if state(b, "vaultwarden") != Active || b.st.Services["vaultwarden"].Snapshot != snap || !b.st.Services["vaultwarden"].DNS {
		t.Fatalf("estado não recuperado: %+v", b.st.Services["vaultwarden"])
	}
	delete(f.down, "bitwarden.engmariz.com@"+srv)
	tickTo(b, &at, 20)
	wantStates(t, b, map[string]string{"vaultwarden": Normal})
	if _, err := os.Stat(snap); !os.IsNotExist(err) {
		t.Fatal("snapshot ficou depois do regresso")
	}
}

func TestDeleteSnapshotGuard(t *testing.T) {
	a, f := setup(t)
	d := a.cfg.Paths.SnapshotsDir
	for _, p := range []string{"/Volume1/ServerBackup", d, d + "/other", d + "/failover-x/../../x", "/Volume1/failover-x"} {
		if err := a.deleteSnapshot("x", p); err == nil {
			t.Errorf("aceitou apagar %s", p)
		}
	}
	if f.ran("btrfs") >= 0 {
		t.Fatalf("btrfs chamado: %v", f.cmds)
	}
}

// O4: the scan finds each stack's images from its compose files (overrides
// included) and reports the ones the TNAS does not have.
func TestScanImages(t *testing.T) {
	a, f := setup(t)
	buf := logs(t)
	f.busy = map[string]bool{"tcp :2283": true}
	f.failCmd = []string{"docker network inspect proxy"}
	f.outs = map[string]string{
		// two services on redis:7: listed once; an external network the TNAS lacks; a port taken
		"docker compose -p failover-immich": `{"name":"failover-immich","services":{"server":{"image":"ghcr.io/immich-app/immich-server:v2","ports":[{"published":"2283","protocol":"tcp"}]},
			"redis":{"image":"redis:7"},"cache":{"image":"redis:7"}},"networks":{"proxy":{"name":"proxy","external":true}}}`,
		"docker compose -p failover-":                                   `{"name":"x","services":{"vaultwarden":{"image":"vaultwarden/server:latest"}}}`,
		"docker image inspect --format {{.Size}} {{.Created}} redis:7":  "41000000 2026-09-20T04:00:00.5Z\n",
		"docker image inspect --format {{.Size}} {{.Created}} vaultwar": "250000000 2026-09-21T04:00:00Z\n",
	}
	a.scanImages()
	im := a.st.Images["immich"].Images
	if len(im) != 2 || im[0].Present || !im[1].Present || im[1].Size != 41000000 || im[1].Created.IsZero() {
		t.Fatalf("imagens do immich: %+v", im)
	}
	if v := a.st.Images["vaultwarden"].Images; len(v) != 1 || !v[0].Present || a.st.Images["npm"].Images == nil {
		t.Fatalf("imagens: %+v", a.st.Images)
	}
	if n := a.st.Images["immich"].Notes; len(n) != 2 || !strings.Contains(n[0], "a rede proxy não existe no TNAS") || !strings.Contains(n[1], "a porta 2283/tcp já está ocupada") {
		t.Fatalf("pré-requisitos do TNAS: %q", n)
	}
	if !slices.ContainsFunc(f.scans, func(c string) bool {
		return strings.Contains(c, "immich.override.yml config --format json")
	}) {
		t.Fatalf("o scan do immich ignorou o override: %v", f.scans)
	}
	if !strings.Contains(a.st.ImagesMsg, "ghcr.io/immich-app/immich-server:v2") || strings.Contains(a.st.ImagesMsg, "redis") {
		t.Fatalf("aviso errado: %q", a.st.ImagesMsg)
	}
	f.outs["docker image inspect --format {{.Size}} {{.Created}} ghcr.io"] = "1 2026-09-22T04:00:00Z"
	a.scanImages()
	if !strings.Contains(buf.String(), `msg="imagens: 2 de 3 no TNAS, 0,3 GB"`) {
		t.Fatalf("sem o balanço das imagens: %s", buf)
	}
	if a.st.ImagesMsg != "" || a.events[len(a.events)-1].Msg != "todas as imagens estão no TNAS" {
		t.Fatalf("não avisou que as imagens voltaram: %q", a.st.ImagesMsg)
	}
}

// Each box's internet is its own Technitium; it is left as it was while that
// Technitium does not answer, and the server's while the server is down.
func TestInternetPerBox(t *testing.T) {
	a, f := setup(t)
	f.noNet = map[string]bool{a.cfg.TNASIP: true}
	f.noDNS = map[string]bool{a.cfg.TNASIP: true}
	a.Tick(t0)
	if !a.st.TNASNetOK || hasEvent(a, "TNAS: sem internet") {
		t.Fatalf("o Technitium do TNAS parado tirou-lhe a internet: %v", a.events)
	}
	f.noDNS = map[string]bool{}
	f.noNet = map[string]bool{a.cfg.Server.IP: true}
	a.Tick(t0.Add(time.Minute))
	if a.st.ServerNetOK || !a.st.TNASNetOK || !hasEvent(a, "servidor: sem internet, o DNS não resolve nomes de fora") {
		t.Fatalf("servidor sem internet: %v %v %v", a.st.ServerNetOK, a.st.TNASNetOK, a.events)
	}
	f.noNet = map[string]bool{}
	f.noPing[a.cfg.Server.IP] = true
	f.down[a.cfg.Server.NPMCheckHost+"@"+a.cfg.Server.IP] = true
	a.Tick(t0.Add(2 * time.Minute))
	if a.st.ServerNetOK {
		t.Fatal("com o servidor em baixo, a internet dele mudou")
	}
}

// Each member of the DNS cluster is asked for its own zone: one that does
// not answer is an event, and so is its return.
func TestDNSCluster(t *testing.T) {
	a, f := setup(t)
	f.noDNS = map[string]bool{a.cfg.TNASIP: true}
	a.Tick(t0)
	if a.st.TNASDNSOK || !a.st.ServerDNSOK || !hasEvent(a, "Technitium do TNAS não responde") {
		t.Fatalf("Technitium do TNAS em baixo: %v %v %v", a.st.TNASDNSOK, a.st.ServerDNSOK, a.events)
	}
	f.noDNS = nil
	a.Tick(t0.Add(time.Minute))
	if !a.st.TNASDNSOK || !hasEvent(a, "Technitium do TNAS responde outra vez") {
		t.Fatalf("de volta: %v", a.st.TNASDNSOK)
	}
}

// The server falling is one email with the time; its return is an event.
func TestServerDownMailed(t *testing.T) {
	a, f := setup(t)
	f.noPing[srv] = true
	f.down[a.cfg.Server.NPMCheckHost+"@"+srv] = true
	at := t0
	tickTo(a, &at, 3)
	a.mailJobs.Wait()
	n := 0
	for _, m := range f.mails {
		if strings.Contains(m.Subject, "o servidor não responde") {
			n++
		}
	}
	if n != 1 || !mailed(f, "desde as "+t0.Format("15:04")) {
		t.Fatalf("%d emails do servidor em baixo: %+v", n, f.mails)
	}
	delete(f.noPing, srv)
	delete(f.down, a.cfg.Server.NPMCheckHost+"@"+srv)
	tickTo(a, &at, 4)
	if !hasEvent(a, "o servidor responde outra vez, depois de 4 min em baixo") || !a.st.ServerDown.IsZero() {
		t.Fatalf("regresso do servidor: %v", a.events[len(a.events)-3:])
	}
}

// A service waiting out wait_min or stability_min makes the checks fast;
// one only observed, or forced, does not.
func TestWaiting(t *testing.T) {
	a, _ := setup(t)
	s := a.st.Services["vaultwarden"]
	cases := []struct {
		name string
		set  func()
		want bool
	}{
		{"todos bem", func() {}, false},
		{"em falha antes do failover", func() { s.FailSince = t0 }, true},
		{"em falha, só observado", func() { s.Observed = true }, false},
		{"no TNAS, servidor de volta", func() { *s = SvcState{State: Active, OKSince: t0} }, true},
		{"no TNAS, forçado", func() { s.Forced = true }, false},
		{"no TNAS, servidor ainda em baixo", func() { *s = SvcState{State: Active} }, false},
	}
	for _, c := range cases {
		c.set()
		if got := a.waiting(); got != c.want {
			t.Errorf("%s: waiting %v, esperado %v", c.name, got, c.want)
		}
	}
}
