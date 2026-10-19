package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syncSetup turns the sync on: the mirror subvolume gets the received
// snapshots, each a folder named by its time, with a compose inside.
func syncSetup(t *testing.T) (*Agent, *fake) {
	t.Helper()
	a, f := setup(t)
	a.cfg.Mirror = MirrorSync{Server: "failover@192.168.1.66", Subvolume: "/home/x/docker", At: "04:30", Keep: 2}
	a.cfg.Paths.MirrorSubvol = t.TempDir()
	return a, f
}

func receive(name string) func(dir string) error {
	return func(dir string) error {
		return os.MkdirAll(filepath.Join(dir, name, "homelab", "npm"), 0o755)
	}
}

// arm sets what the next Pipe leaves and whether it fails, under the fake's
// lock: the image scan a sync starts may still be reading them.
func arm(f *fake, recv func(string) error, fail ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recv, f.failCmd = recv, fail
}

// The newest received snapshot is the mirror; without any, or with the sync off, the folder itself.
func TestMirrorIsNewestSnapshot(t *testing.T) {
	a, _ := syncSetup(t)
	sub := a.cfg.Paths.MirrorSubvol
	if a.cfg.mirror() != sub {
		t.Fatalf("sem snapshots: %s", a.cfg.mirror())
	}
	for _, n := range []string{"20261018-043000", "20261019-043000", "notes"} {
		_ = os.MkdirAll(filepath.Join(sub, n), 0o755)
	}
	if got := a.cfg.mirror(); got != filepath.Join(sub, "20261019-043000") {
		t.Fatalf("espelho %s", got)
	}
	a.cfg.Mirror.Server = ""
	if a.cfg.mirror() != sub {
		t.Fatalf("sync desligado: %s", a.cfg.mirror())
	}
}

// A sync asks the server for the difference since the newest snapshot the
// TNAS has, keeps mirror.keep of them, and a failure leaves no partial one.
func TestMirrorSync(t *testing.T) {
	a, f := syncSetup(t)
	sub := a.cfg.Paths.MirrorSubvol
	arm(f, receive("20261019-043000"))
	a.syncMirror("teste")
	if i := f.ran("ssh -i"); i < 0 || !strings.HasSuffix(f.cmds[i], " failover@192.168.1.66 send  | btrfs receive "+sub) || !strings.Contains(f.cmds[i], "BatchMode=yes") {
		t.Fatalf("primeiro envio: %v", f.cmds)
	}
	if a.st.SyncMsg != "" || a.st.LastSync.IsZero() || a.st.MirrorChanged.IsZero() || !hasEvent(a, "20261019-043000 recebido") {
		t.Fatalf("estado depois do sync: %+v", a.st.SyncMsg)
	}
	for _, n := range []string{"20261020-043000", "20261021-043000"} {
		arm(f, receive(n))
		a.syncMirror("teste")
	}
	if i := f.ran("ssh -i /"); !strings.Contains(strings.Join(f.cmds[i:], "\n"), " send 20261020-043000 | ") {
		t.Fatalf("o pai não é o snapshot mais recente: %v", f.cmds)
	}
	if got := a.cfg.snapshots(); len(got) != 2 || got[0] != "20261020-043000" || !hasEvent(a, "apagados 20261019-043000") {
		t.Fatalf("retenção: %v", got)
	}
	// a failed receive: the partial snapshot is deleted and the mirror stays the last good one
	arm(f, receive("20261022-043000"), "ssh")
	a.syncMirror("teste")
	if got := a.cfg.snapshots(); len(got) != 2 || got[1] != "20261021-043000" || !strings.Contains(a.st.SyncMsg, "falhou") {
		t.Fatalf("depois da falha: %v %q", got, a.st.SyncMsg)
	}
	arm(f, nil)
	a.syncMirror("teste")
	if !strings.Contains(a.st.SyncMsg, "nenhum snapshot") {
		t.Fatalf("nada recebido: %q", a.st.SyncMsg)
	}
}

// The daily sync starts once a day, at the first check after mirror.at.
func TestMirrorSyncDaily(t *testing.T) {
	a, f := syncSetup(t)
	arm(f, receive("20261019-043000"))
	syncs := func() int {
		time.Sleep(100 * time.Millisecond)
		f.mu.Lock()
		defer f.mu.Unlock()
		return strings.Count(strings.Join(f.cmds, "\n"), "btrfs receive")
	}
	at := t0 // 12:00, past 04:30: the day's sync has not run
	tickTo(a, &at, 3)
	if syncs() != 1 {
		t.Fatalf("%d sincronizações no primeiro dia, esperada 1", syncs())
	}
	for _, h := range []time.Time{t0.Add(16 * time.Hour).Add(29 * time.Minute), t0.Add(16 * time.Hour).Add(31 * time.Minute)} { // 04:29 and 04:31 the next day
		a.Tick(h)
	}
	if syncs() != 2 {
		t.Fatalf("%d sincronizações ao fim de dois dias, esperadas 2", syncs())
	}
}

// The key is made once; the test lists the server's snapshots with it.
func TestMirrorKeyAndTest(t *testing.T) {
	a, f := syncSetup(t)
	a.cfgPath = filepath.Join(t.TempDir(), "failover.yml")
	pub, err := a.ensureKey()
	if err != nil || pub != "ssh-ed25519 AAAATEST failover-agent" {
		t.Fatalf("chave: %q %v", pub, err)
	}
	if _, err := a.ensureKey(); err != nil || strings.Count(strings.Join(f.cmds, "\n"), "ssh-keygen") != 1 {
		t.Fatalf("a chave foi gerada outra vez: %v", f.cmds)
	}
	if err := a.cfg.Mirror.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []MirrorSync{{Server: "nouser"}, {Server: "a@b", At: "4:30"}, {Server: "a@b", Subvolume: "rel"}} {
		var fe *FieldError
		if err := bad.validate(); !errors.As(err, &fe) {
			t.Errorf("%+v aceite", bad)
		}
	}
}

// The checklist reads the script's answer; a lost connection is one failed line.
func TestMirrorChecks(t *testing.T) {
	a, _ := syncSetup(t)
	out := "subvolume ok\nlivre 812G\nsnapshots 20261018-043000 \n"
	got := mirrorChecks(a.cfg, out, nil, "btrfs")
	if len(got) != 5 || !got[0].OK || !got[1].OK || !strings.Contains(got[2].Msg, "812G") || !strings.Contains(got[3].Msg, "20261018") || !got[4].OK {
		t.Fatalf("%+v", got)
	}
	got = mirrorChecks(a.cfg, "subvolume /x não é um subvolume btrfs\n", nil, "ext4")
	if len(got) != 3 || got[1].OK || got[2].OK {
		t.Fatalf("%+v", got)
	}
	if got = mirrorChecks(a.cfg, "", errors.New("timeout"), "btrfs"); len(got) != 2 || got[0].OK || !strings.Contains(got[0].Msg, "timeout") {
		t.Fatalf("%+v", got)
	}
}
