package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Case 2.3 (server NPM down, server pinging) never fails a service over, even
// with two isolated lost pings far apart: nothing is known of the services.
func TestLostPingsNeverFailOver(t *testing.T) {
	a, f := setup(t)
	f.down["nginx.engmariz.com@"+srv] = true // NPM down, server alive
	at := t0
	for i := 0; i <= 60; i++ {
		f.mu.Lock()
		f.noPing[srv] = i == 1 || i == 60 // one lost ICMP packet at minute 1 and at minute 60
		f.mu.Unlock()
		a.Tick(at)
		at = at.Add(time.Minute)
	}
	if s := a.st.Services["vaultwarden"]; s.State != Normal {
		t.Errorf("vaultwarden %s (FailSince %v) com 2 pings perdidos isolados", s.State, s.FailSince)
	}
}

// A first reboot that leaves the NPM down must not make the first lost ping of
// a second one fail every service over at once.
func TestSecondRebootWaits(t *testing.T) {
	a, f := setup(t)
	at := t0
	step := func(npmDown, pingDown bool) {
		f.mu.Lock()
		f.down["nginx.engmariz.com@"+srv], f.noPing[srv] = npmDown, pingDown
		f.mu.Unlock()
		a.Tick(at)
		at = at.Add(time.Minute)
	}
	step(true, true) // reboot 1: 2 min down (< every wait_min)
	step(true, true)
	for range 40 { // back, but its NPM does not start: case 2.3
		step(true, false)
	}
	step(true, true) // reboot 2, first tick
	for n, s := range a.st.Services {
		if s.State != Normal {
			t.Errorf("%s: %s no primeiro tick do segundo reboot", n, s.State)
		}
	}
}

// The stability window survives a server-NPM outage no more than a failure does.
func TestOKSinceDoesNotSurviveNPMOutage(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	wantStates(t, a, map[string]string{"vaultwarden": Active})
	delete(f.down, "bitwarden.engmariz.com@"+srv)
	a.Tick(at) // OK once: OKSince set
	at = at.Add(time.Minute)
	f.down["nginx.engmariz.com@"+srv] = true // NPM down 30 min, server alive
	for range 30 {
		a.Tick(at)
		at = at.Add(time.Minute)
	}
	delete(f.down, "nginx.engmariz.com@"+srv)
	a.Tick(at) // first check with the NPM back
	if st := state(a, "vaultwarden"); st != Active {
		t.Errorf("voltou (%s) no primeiro OK depois de 30 min sem o NPM: a estabilidade não foi esperada", st)
	}
}

// A failover whose cleanup also failed is cleaned up and tried again.
func TestErrorWithLeftoversRetried(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.failCmd = []string{"docker compose -p failover-vaultwarden -f"} // compose up fails
	f.left = map[string]string{"failover-vaultwarden": "abc123"}      // and down leaves a container once
	at := t0
	tickTo(a, &at, 5)
	if s := a.st.Services["vaultwarden"]; s.State != Error || s.Snapshot == "" {
		t.Fatalf("pré-condição: %s, snapshot %q", s.State, s.Snapshot)
	}
	f.mu.Lock()
	f.failCmd, f.left = nil, nil // the transient causes are gone
	f.mu.Unlock()
	tickTo(a, &at, 180) // 3 h, server side still down
	if s := a.st.Services["vaultwarden"]; s.State != Active {
		t.Errorf("vaultwarden %s (%s, snapshot %q) 3 h depois: a limpeza e o failover não foram repetidos", s.State, s.Msg, s.Snapshot)
	}
}

// A cleanup that fails again waits another timeout and says so.
func TestErrorCleanupFailsAgain(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	f.failCmd = []string{"docker compose -p failover-vaultwarden"} // up and down fail
	at := t0
	tickTo(a, &at, 5)
	since := a.st.Services["vaultwarden"].Since
	tickTo(a, &at, 120)
	s := a.st.Services["vaultwarden"]
	if s.State != Error || !hasEvent(a, "limpeza da cópia falhou outra vez") || !s.Since.After(since) {
		t.Errorf("estado %s, desde %v (antes %v)", s.State, s.Since, since)
	}
	if hasEvent(a, "nova tentativa") {
		t.Error("tentou o failover com a limpeza por fazer")
	}
}

// stopIdleNPM: the NPM is down but its snapshot delete failed: it is not "up".
func TestNPMUpClearedAfterPartialStop(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	npmSnap := a.st.TNASNPM.Snapshot
	delete(f.down, "bitwarden.engmariz.com@"+srv)
	f.mu.Lock()
	f.failCmd = []string{"btrfs subvolume delete " + npmSnap}
	f.mu.Unlock()
	tickTo(a, &at, 16) // returned; NPM down -v ran, its snapshot delete failed
	if n := a.st.TNASNPM; n.Snapshot == "" || n.Up || n.OK {
		t.Fatalf("NPM do TNAS parado a meio devia ficar com snapshot e sem Up/OK: %+v", n)
	}
	f.reset()
	f.mu.Lock()
	f.failCmd = nil
	f.mu.Unlock()
	f.down["homepage.engmariz.com@"+srv] = true
	tickTo(a, &at, 27) // homepage fails over (wait_min 10)
	if f.ran("docker compose -p failover-npm -f") < 0 {
		t.Errorf("o NPM do TNAS não arrancou outra vez (homepage %s): %+v", state(a, "homepage"), a.st.TNASNPM)
	}
}

// An add that Technitium applied but answered with an error leaves no record
// behind once the failover gives up.
func TestDNSAddErrorIsDeleted(t *testing.T) {
	a, f := setup(t)
	f.getErr = map[string]error{"http://127.0.0.1:5380/api/zones/records/add": errors.New("context deadline exceeded")}
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 20)
	s := a.st.Services["vaultwarden"]
	if s.State != Error || !hasGet(f, "records/add") || !hasGet(f, "records/delete") || s.DNS {
		t.Errorf("estado %s, dns=%v, add=%v, delete=%v", s.State, s.DNS, hasGet(f, "records/add"), hasGet(f, "records/delete"))
	}
}

// One lost router ping is a blip: events, no email. An outage that lasts is
// emailed, and so is its end.
func TestRouterBlipNoMail(t *testing.T) {
	a, f := setup(t)
	a.cfg.Email = EmailConfig{Host: "smtp.example.com", Port: 587, Security: "starttls", User: "a@example.com"}
	at := t0
	for i := range 4 {
		f.mu.Lock()
		f.noPing["192.168.1.1"] = i == 1
		f.mu.Unlock()
		a.Tick(at)
		a.mailJobs.Wait()
		at = at.Add(time.Minute)
	}
	if len(f.mails) != 0 || !hasEvent(a, "router inacessível") || !hasEvent(a, "router acessível") {
		t.Errorf("%d emails, eventos: %v", len(f.mails), a.events)
	}
}

func TestRouterOutageMailedOnce(t *testing.T) {
	a, f := setup(t)
	a.cfg.Email = EmailConfig{Host: "smtp.example.com", Port: 587, Security: "starttls", User: "a@example.com"}
	at := t0
	for i := range 6 {
		f.mu.Lock()
		f.noPing["192.168.1.1"] = i >= 1 && i <= 3
		f.mu.Unlock()
		a.Tick(at)
		a.mailJobs.Wait()
		at = at.Add(time.Minute)
	}
	if len(f.mails) != 2 || !mailed(f, "router inacessível desde as 12:01") || !mailed(f, "router acessível outra vez") {
		t.Errorf("%d emails: %+v", len(f.mails), f.mails)
	}
}

// A state saved while the router was down, from before RouterChanged existed,
// does not email an outage "since 00:00".
func TestRouterOutageOldState(t *testing.T) {
	a, f := setup(t)
	a.cfg.Email = EmailConfig{Host: "smtp.example.com", Port: 587, Security: "starttls", User: "a@example.com"}
	a.st.RouterOK = false
	f.noPing["192.168.1.1"] = true
	at := t0
	for range 2 {
		a.Tick(at)
		a.mailJobs.Wait()
		at = at.Add(time.Minute)
	}
	if mailed(f, "00:00") || !mailed(f, "router inacessível desde as 12:00") {
		t.Errorf("emails: %+v", f.mails)
	}
}

// A failover in progress is waited on at the fast pace, a return is not.
func TestWaitingCountsFailingOver(t *testing.T) {
	a, _ := setup(t)
	s := a.svc("vaultwarden")
	s.State = FailingOver
	if !a.waiting() {
		t.Error("um failover em curso devia contar")
	}
	s.State = Returning
	if a.waiting() {
		t.Error("um regresso em curso não conta")
	}
}

// With the server down nothing asks its DNS: evaluate ignores it.
func TestProbeServerDownSkipsItsDNS(t *testing.T) {
	a, f := setup(t)
	f.noPing[srv] = true
	f.down["nginx.engmariz.com@"+srv] = true
	a.Tick(t0)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ip := range f.resolved {
		if ip == srv {
			t.Error("perguntou ao DNS do servidor em baixo")
		}
	}
	if len(f.resolved) == 0 {
		t.Error("o DNS do TNAS devia ter sido perguntado")
	}
}

// A record of its own on the service's name is a problem: overwrite=true would
// replace it and the return would delete it.
func TestVerdictOwnARecord(t *testing.T) {
	a, f := setup(t)
	a.checkReady()
	for _, p := range a.st.Verdict.Problems {
		if strings.Contains(p, "registo A próprio") {
			t.Fatalf("sem registo próprio, mas: %q", p)
		}
	}
	f.mu.Lock()
	f.bodies = map[string][]byte{}
	f.bodies["http://127.0.0.1:5380/api/zones/records/get?domain=bitwarden.engmariz.com&zone=engmariz.com"] = []byte(`{"status":"ok","response":{"records":[{"type":"A","rData":{"ipAddress":"192.168.1.66"}}]}}`)
	f.mu.Unlock()
	a.checkReady()
	if p := strings.Join(a.st.Verdict.Problems, "\n"); !strings.Contains(p, "vaultwarden: bitwarden.engmariz.com tem um registo A próprio (192.168.1.66)") {
		t.Errorf("problemas: %q", p)
	}
}

// The A record the agent itself wrote for a service in failover is not a
// record of its own; one pointing elsewhere is.
func TestVerdictOwnARecordSkipsTNAS(t *testing.T) {
	a, f := setup(t)
	a.st.Services["vaultwarden"].State = Active
	const api = "http://127.0.0.1:5380/api/zones/records/get?domain=%s&zone=engmariz.com"
	rec := func(ip string) []byte {
		return []byte(`{"status":"ok","response":{"records":[{"type":"A","rData":{"ipAddress":"` + ip + `"}}]}}`)
	}
	f.bodies = map[string][]byte{
		fmt.Sprintf(api, "bitwarden.engmariz.com"): rec(tnas),
		fmt.Sprintf(api, "homepage.engmariz.com"):  rec("192.168.1.99"),
	}
	a.checkReady()
	p := strings.Join(a.st.Verdict.Problems, "\n")
	if strings.Contains(p, "vaultwarden: bitwarden.engmariz.com tem um registo") || !strings.Contains(p, "homepage: homepage.engmariz.com tem um registo A próprio (192.168.1.99)") {
		t.Errorf("problemas: %q", p)
	}
}

// Each image counts once however many stacks use it.
func TestCountImages(t *testing.T) {
	stacks := map[string]Stack{
		"a": {Images: []Image{{Ref: "x", Present: true, Size: 10}, {Ref: "y"}}},
		"b": {Images: []Image{{Ref: "x", Present: true, Size: 10}}},
	}
	if p, n, size := countImages(stacks); p != 1 || n != 2 || size != 10 {
		t.Errorf("present %d total %d size %d", p, n, size)
	}
}

// macvlanFailover fails unifi (it has require_free_ip) over, with the copy's container
// at 192.168.1.92 answering as copyMAC, and then lets the server's NPM reach the copy
// there: its check passes again, though the server's container is still dead.
func macvlanFailover(t *testing.T, arp string) (*Agent, *fake, time.Time) {
	t.Helper()
	a, f := setup(t)
	f.outs = map[string]string{
		"docker ps -q":   "abc\n",
		"docker inspect": "192.168.1.92 02:42:c0:a8:01:5c\n",
		"arping -c 1 -w 3 -I ovs_eth0 192.168.1.92": arp,
	}
	f.down["unifi.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 16)
	wantStates(t, a, map[string]string{"unifi": Active})
	delete(f.down, "unifi.engmariz.com@"+srv)
	return a, f, at
}

// A service with its own IP answers on the server only when that IP answers ARP from
// a MAC that is not the copy's: else the server's NPM reaches the copy and it flaps.
func TestMacvlanCopyAnswers(t *testing.T) {
	const copyReply = "Unicast reply from 192.168.1.92 [02:42:C0:A8:01:5C]  0.650ms\n"
	t.Run("the copy answers: stays", func(t *testing.T) {
		a, _, at := macvlanFailover(t, copyReply)
		if mac := a.st.Services["unifi"].CopyMAC; mac != "02:42:c0:a8:01:5c" {
			t.Fatalf("MAC da cópia %q", mac)
		}
		tickTo(a, &at, 76)
		wantStates(t, a, map[string]string{"unifi": Active})
	})
	t.Run("nothing answers: stays", func(t *testing.T) {
		a, _, at := macvlanFailover(t, "")
		tickTo(a, &at, 76)
		wantStates(t, a, map[string]string{"unifi": Active})
	})
	t.Run("the server's container answers: returns", func(t *testing.T) {
		a, _, at := macvlanFailover(t, "Unicast reply from 192.168.1.92 [aa:bb:cc:dd:ee:ff]  0.650ms\n")
		tickTo(a, &at, 20)
		wantStates(t, a, map[string]string{"unifi": Normal})
		if mac := a.st.Services["unifi"].CopyMAC; mac != "" {
			t.Errorf("MAC da cópia %q depois do regresso", mac)
		}
	})
	t.Run("no MAC known: trusts the check, as before", func(t *testing.T) {
		a, _, at := macvlanFailover(t, copyReply)
		a.st.Services["unifi"].CopyMAC = ""
		tickTo(a, &at, 20)
		wantStates(t, a, map[string]string{"unifi": Normal})
	})
}

// A record points at the TNAS, so the zone and the API it is deleted in stay as they are
// until the return; the TTL does not matter to it.
func TestZoneLockedWhileActive(t *testing.T) {
	a, f := setup(t)
	f.down["bitwarden.engmariz.com@"+srv] = true
	at := t0
	tickTo(a, &at, 5)
	wantStates(t, a, map[string]string{"vaultwarden": Active})
	for _, body := range []string{`{"section":"dns","values":{"dns.zone":"other.com"}}`, `{"section":"dns","values":{"dns.api_url":"http://10.0.0.9:5380"}}`} {
		if code, got := postTo(t, a.postSection, body); code != 409 {
			t.Errorf("%s: HTTP %d %s", body, code, got)
		}
	}
	if a.cfg.DNS.Zone != "engmariz.com" {
		t.Errorf("zona %q", a.cfg.DNS.Zone)
	}
	if code, _ := postTo(t, a.postSection, `{"section":"dns","values":{"dns.ttl":120}}`); code != 204 || a.cfg.DNS.TTL != 120 {
		t.Errorf("dns.ttl: %d %d", code, a.cfg.DNS.TTL)
	}
}
