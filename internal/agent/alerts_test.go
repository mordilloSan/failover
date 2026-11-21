package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What needs a human goes by email: in observe mode the failover the agent
// would start, and a night whose pull failed.
func TestObserveAndNightlyMailed(t *testing.T) {
	a, f := setup(t)
	a.cfg.Mode = "observe"
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 6)
	a.mailJobs.Wait()
	if !mailed(f, "[observação] o failover começaria agora") {
		t.Fatalf("observação sem email: %+v", f.mails)
	}

	b, g := setup(t)
	b.cfg.Nightly.PrepullAt = "05:00"
	g.failCmd = []string{"docker compose -p failover-immich"}
	b.Tick(time.Date(2026, 9, 30, 5, 1, 0, 0, time.Local))
	waitFor(t, func() bool { return !b.pulling.Load() })
	b.mu.Lock()
	b.flushAlerts()
	b.mu.Unlock()
	b.mailJobs.Wait()
	if !mailed(g, "descarga noturna com falhas") || !mailed(g, "immich") {
		t.Fatalf("noite com falhas sem email: %+v", g.mails)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.cmds {
		if strings.HasPrefix(c, "docker compose -p failover-immich") {
			n++
		}
	}
	if n != 2 { // tried again after a minute: a registry's rate limit passes
		t.Fatalf("%d descargas do immich, esperava 2", n)
	}
}

// A certificate already told about is not told again after a restart.
func TestCertToldOnceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	f := &fake{noPing: map[string]bool{}, down: map[string]bool{}, badCert: map[string]bool{"bitwarden.engmariz.com": true}}
	told := func(evs []Event) (n int) {
		for _, e := range evs {
			if strings.Contains(e.Msg, "certificado de bitwarden.engmariz.com inválido") {
				n++
			}
		}
		return n
	}
	a := newTestAgent(t, dir, f)
	a.Tick(t0)
	b := newTestAgent(t, dir, f)
	// The second start trims events older than 30 days by the real clock, which
	// can drop the first start's event (dated t0), so count only what b adds.
	loaded := len(b.events)
	b.Tick(t0.Add(time.Minute))
	if n, m := told(a.events), told(b.events[loaded:]); n != 1 || m != 0 {
		t.Fatalf("%d aviso(s) do certificado no primeiro arranque e %d no segundo (queria 1 e 0)", n, m)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("não aconteceu em 5 s")
		}
	}
}

// A failover that failed is tried again every start_timeout_min while the
// server is down, and emailed only when the error changes. With nothing left
// running, the service counts as home: it can be edited and removed.
func TestRetryAfterError(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.failCmd = []string{"docker compose -p failover-vaultwarden -f"}
	at := t0
	tickTo(a, &at, 6)
	wantStates(t, a, map[string]string{"vaultwarden": Error})
	a.mu.Lock()
	home := a.home(a.svc("vaultwarden")) && a.allHome()
	a.mu.Unlock()
	if !home {
		t.Fatal("um ERROR sem nada a correr não conta como em casa")
	}
	tickTo(a, &at, 6+a.cfg.StartTimeoutMin+1)
	if !hasEvent(a, "nova tentativa de failover") {
		t.Fatal("sem nova tentativa")
	}
	errs := 0
	for _, e := range a.events {
		if strings.HasPrefix(e.Msg, "ERRO: compose up") {
			errs++
		}
	}
	a.mailJobs.Wait()
	mails := 0
	for _, m := range f.mails {
		if strings.Contains(m.Subject+m.Body, "ERRO: compose up") {
			mails++
		}
	}
	if errs < 2 || mails != 1 {
		t.Fatalf("%d erros, %d emails (queria 1)", errs, mails)
	}
	f.failCmd = nil
	tickTo(a, &at, 6+2*(a.cfg.StartTimeoutMin+1))
	wantStates(t, a, map[string]string{"vaultwarden": Active})
}

// A failover records its steps with their times; the email says how long
// each took; the return clears them.
func TestFailoverSteps(t *testing.T) {
	n := time.Now()
	st := []Step{{"início", n}, {"snapshot", n.Add(2 * time.Second)}, {"arranque", n.Add(96 * time.Second)}, {"resposta", n.Add(106 * time.Second)}}
	if got := stepTimes(st); got != "snapshot 2 s, arranque 1 min 34 s, resposta 10 s" {
		t.Fatalf("%q", got)
	}
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 6)
	var names []string
	for _, x := range a.st.Services["vaultwarden"].Steps {
		names = append(names, x.Name)
	}
	if strings.Join(names, " ") != "início NPM snapshot arranque resposta DNS" || !hasEvent(a, "em failover no TNAS (NPM ") {
		t.Fatalf("passos %v", names)
	}
}

// A restart after the snapshot but before compose up: the next tick starts
// the copy from that snapshot instead of waiting for a copy that never runs.
func TestResumeBeforeUp(t *testing.T) {
	dir := t.TempDir()
	f := &fake{noPing: map[string]bool{}, down: map[string]bool{"bitwarden.engmariz.com@" + srv: true}}
	a := newTestAgent(t, dir, f)
	a.mu.Lock()
	s := a.svc("vaultwarden")
	a.now = t0
	a.set(s, FailingOver)
	s.Snapshot = filepath.Join(a.cfg.Paths.SnapshotsDir, "failover-vaultwarden-x")
	a.st.TNASNPM.Snapshot = filepath.Join(a.cfg.Paths.SnapshotsDir, "failover-npm-x") // its compose up never ran either
	a.save()
	a.mu.Unlock()
	b := newTestAgent(t, dir, f)
	b.Tick(t0.Add(time.Minute))
	if st := b.st.Services["vaultwarden"].State; st != Active {
		t.Fatalf("estado %s depois de retomar: %s", st, b.st.Services["vaultwarden"].Msg)
	}
	if f.ran("docker compose -p failover-npm -f") < 0 || f.ran("docker compose -p failover-vaultwarden -f "+s.Snapshot) < 0 {
		t.Fatalf("não arrancou o NPM e a cópia: %v", f.cmds)
	}
}
