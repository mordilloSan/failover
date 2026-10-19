package agent

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The mirror by btrfs send/receive: the agent connects to the server over
// SSH (outbound only: nothing listens on the TNAS), a forced command there
// takes a read-only snapshot of the docker subvolume and streams the
// difference since the last one, and btrfs receive lands it under
// paths.mirror_subvol as a read-only subvolume named by its time. The newest
// one is the mirror every failover copies from. Off while mirror.server is
// empty: then the mirror is whatever the TOS backup writes.

// MirrorSync is the mirror section of failover.yml.
type MirrorSync struct {
	Server    string `yaml:"server,omitempty"`    // user@host; empty: no sync, the TOS backup fills the mirror
	Subvolume string `yaml:"subvolume,omitempty"` // the btrfs subvolume on the server (only shown in the script)
	At        string `yaml:"at,omitempty"`        // HH:MM of the daily sync; empty: only by hand
	Keep      int    `yaml:"keep,omitempty"`      // received snapshots kept on the TNAS (0: 3)
	KeyFile   string `yaml:"key_file,omitempty"`  // the agent's private key; empty: mirror_key next to failover.yml
}

func (m MirrorSync) on() bool  { return m.Server != "" }
func (m MirrorSync) keep() int { return cmp.Or(m.Keep, 3) }

func (m MirrorSync) validate() error {
	user, host, ok := strings.Cut(m.Server, "@")
	switch {
	case m.Server != "" && (!ok || !isName(user) || !isName(host)):
		return fe("mirror.server", "tem de ser utilizador@servidor, por exemplo failover@192.168.1.66")
	case m.Subvolume != "" && !filepath.IsAbs(m.Subvolume):
		return fe("mirror.subvolume", "tem de ser um caminho absoluto no servidor")
	case m.Keep < 0:
		return fe("mirror.keep", "não pode ser negativo")
	case m.KeyFile != "" && !filepath.IsAbs(m.KeyFile):
		return fe("mirror.key_file", "tem de ser um caminho absoluto")
	}
	if m.At != "" {
		if _, err := time.Parse("15:04", m.At); err != nil || len(m.At) != 5 {
			return fe("mirror.at", "tem de ser HH:MM")
		}
	}
	return nil
}

// snapName is how the server's script names a snapshot: its local time.
var snapName = regexp.MustCompile(`^\d{8}-\d{6}$`)

// snapshots lists the received snapshots under the mirror subvolume, oldest first.
func (c Config) snapshots() []string {
	ents, _ := os.ReadDir(c.Paths.MirrorSubvol)
	var names []string
	for _, e := range ents {
		if e.IsDir() && snapName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names
}

// mirror is the subvolume a failover copies from: the newest received
// snapshot with the sync on, the TOS backup's folder without.
func (c Config) mirror() string {
	if c.Mirror.on() {
		if s := c.snapshots(); len(s) > 0 {
			return filepath.Join(c.Paths.MirrorSubvol, s[len(s)-1])
		}
	}
	return c.Paths.MirrorSubvol
}

// mirrorRoot is the folder of the composes inside the mirror.
func (c Config) mirrorRoot() string { return filepath.Join(c.mirror(), c.Paths.MirrorRoot) }

func (a *Agent) mirrorKey() string {
	return cmp.Or(a.cfg.Mirror.KeyFile, filepath.Join(filepath.Dir(a.cfgPath), "mirror_key"))
}

// ensureKey makes the agent's key pair once; the public key is what the
// server's authorized_keys gets.
func (a *Agent) ensureKey() (pub string, err error) {
	key := a.mirrorKey()
	if _, statErr := os.Stat(key); statErr != nil {
		if kerr := a.sys.Run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "failover-agent", "-f", key); kerr != nil {
			return "", fmt.Errorf("criar a chave: %w", kerr)
		}
	}
	b, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", fmt.Errorf("ler a chave pública: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// sshArgs is the connection to the server's forced command: the key, no
// prompts, the host key remembered on first use next to the key.
func (a *Agent) sshArgs(c Config, cmd ...string) []string {
	key := a.mirrorKey()
	return slices.Concat([]string{"ssh", "-i", key, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + key + ".known_hosts", "-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=30", c.Mirror.Server}, cmd)
}

// syncDue starts the day's sync at mirror.at, once.
func (a *Agent) syncDue() {
	m, day := a.cfg.Mirror, a.now.Format(time.DateOnly)
	if !m.on() || m.At == "" || a.st.SyncDay == day || a.now.Format("15:04") < m.At {
		return
	}
	a.st.SyncDay = day
	go a.syncMirror("sincronização diária")
}

// syncMirror receives one snapshot from the server; a partial one left by a
// failure is deleted, old ones beyond mirror.keep too. Outside the lock: it
// takes as long as the data that changed.
func (a *Agent) syncMirror(what string) {
	if !a.syncing.CompareAndSwap(false, true) {
		return
	}
	defer a.syncing.Store(false)
	a.mu.Lock()
	c := a.cfg
	a.publish()
	a.mu.Unlock()

	t0 := time.Now()
	before := c.snapshots()
	parent := ""
	if len(before) > 0 {
		parent = before[len(before)-1]
	}
	err := a.sys.Pipe(a.sshArgs(c, "send", parent), []string{"btrfs", "receive", c.Paths.MirrorSubvol})
	after := c.snapshots()
	fresh := slices.DeleteFunc(slices.Clone(after), func(s string) bool { return slices.Contains(before, s) })
	if err == nil && len(fresh) == 0 {
		err = errors.New("o servidor não enviou nenhum snapshot")
	}
	if err != nil {
		for _, s := range fresh { // a partial subvolume: never the mirror
			_ = a.sys.Run("btrfs", "subvolume", "delete", filepath.Join(c.Paths.MirrorSubvol, s))
		}
	}
	var old []string
	if err == nil && len(after) > c.Mirror.keep() {
		old = after[:len(after)-c.Mirror.keep()]
		for _, s := range old {
			if derr := a.sys.Run("btrfs", "subvolume", "delete", filepath.Join(c.Paths.MirrorSubvol, s)); derr != nil {
				err = fmt.Errorf("apagar o snapshot antigo %s: %w", s, derr)
				break
			}
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.now = time.Now()
	if err != nil {
		a.warn(&a.st.SyncMsg, "", what+" do espelho falhou: "+err.Error())
		a.save()
		return
	}
	a.st.SyncMsg, a.st.LastSync = "", a.now
	a.st.MirrorChanged, a.st.MirrorStale = a.now, false
	msg := what + " do espelho: " + fresh[len(fresh)-1] + " recebido em " + fmtDur(time.Since(t0).Round(time.Second))
	if parent != "" {
		msg += ", a diferença desde " + parent
	}
	if len(old) > 0 {
		msg += "; apagados " + strings.Join(old, ", ")
	}
	a.event("", msg)
	a.mirrorFolders() // new folders or composes in the snapshot
	a.save()
	go a.scanImages()
}

// postMirrorSync starts a sync now.
func (a *Agent) postMirrorSync(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	on := a.cfg.Mirror.on()
	a.mu.Unlock()
	if !on {
		http.Error(w, "o servidor do espelho não está definido", http.StatusBadRequest)
		return
	}
	go a.syncMirror("sincronização pedida na interface")
	w.WriteHeader(http.StatusAccepted)
}

// postMirrorTest is the section's checklist: the connection (the key, the
// forced command, sudo and the script, all proven by one answer), what the
// script says of the server's subvolume, and btrfs on the TNAS's side.
func (a *Agent) postMirrorTest(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	c := a.cfg
	a.mu.Unlock()
	if !c.Mirror.on() {
		http.Error(w, "o servidor do espelho não está definido", http.StatusBadRequest)
		return
	}
	if _, err := a.ensureKey(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ssh := a.sshArgs(c, "check")
	out, err := a.sys.Output(ssh[0], ssh[1:]...)
	fs, _ := a.sys.Output("stat", "-f", "-c", "%T", c.Paths.MirrorSubvol)
	writeJSON(w, http.StatusOK, mirrorChecks(c, out, err, strings.TrimSpace(fs)))
}

// mirrorChecks reads the script's check answer into the page's list.
func mirrorChecks(c Config, out string, sshErr error, tnasFS string) []checkResult {
	var res []checkResult
	add := func(field string, ok bool, msg string) { res = append(res, checkResult{field, ok, msg}) }
	if sshErr != nil {
		add("mirror.server", false, "a ligação ao servidor falhou: "+sshErr.Error())
	} else {
		add("mirror.server", true, "o servidor respondeu com a chave do agente: ligação, comando forçado, sudo e script a funcionar")
		for line := range strings.SplitSeq(out, "\n") {
			k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch k {
			case "subvolume":
				add("mirror.subvolume", v == "ok", map[bool]string{true: c.Mirror.Subvolume + " é um subvolume btrfs", false: v}[v == "ok"])
			case "livre":
				add("mirror.subvolume", true, "espaço livre no servidor: "+v)
			case "snapshots":
				add("mirror.server", true, map[bool]string{true: "snapshots no servidor: " + v, false: "ainda sem snapshots no servidor: o primeiro envio será completo"}[v != ""])
			}
		}
	}
	add("paths.mirror_subvol", tnasFS == "btrfs", map[bool]string{true: c.Paths.MirrorSubvol + " está em btrfs: o TNAS pode receber", false: c.Paths.MirrorSubvol + " não está em btrfs (" + tnasFS + "): btrfs receive não funcionaria aqui"}[tnasFS == "btrfs"])
	if s := c.snapshots(); len(s) > 0 {
		add("paths.mirror_subvol", true, "snapshots no TNAS: "+strings.Join(s, " ")+"; o espelho é o mais recente")
	}
	return res
}

// getMirrorScript is the server's side, with the subvolume filled in.
func (a *Agent) getMirrorScript(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	sub := cmp.Or(a.cfg.Mirror.Subvolume, "/caminho/do/subvolume/docker")
	a.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(strings.ReplaceAll(mirrorScript, "__SUBVOL__", sub)))
}

// mirrorView is the sync's state for the page.
type mirrorView struct {
	On        bool      `json:"on"`
	PubKey    string    `json:"pubkey,omitempty"`
	Snapshots []string  `json:"snapshots"`
	Path      string    `json:"path"`
	Last      time.Time `json:"last,omitzero"`
	Msg       string    `json:"msg,omitempty"`
	Syncing   bool      `json:"syncing"`
}

func (a *Agent) mirrorView() mirrorView {
	c := a.cfg
	pub := ""
	if c.Mirror.on() {
		pub, _ = a.ensureKey()
	}
	snaps := c.snapshots()
	if snaps == nil {
		snaps = []string{}
	}
	return mirrorView{c.Mirror.on(), pub, snaps, c.mirror(), a.st.LastSync, a.st.SyncMsg, a.syncing.Load()}
}

// mirrorScript runs on the server as the forced command of the agent's key:
// list the snapshots, or take one and send the difference since the parent
// the agent names. Nothing else can be asked of that key.
const mirrorScript = `#!/bin/sh
# failover-mirror: o lado do servidor do espelho do failover-agent.
# Corre como comando forçado da chave do agente (authorized_keys), nunca à mão.
# Pedidos, em SSH_ORIGINAL_COMMAND:  check  |  send [snapshot-pai]
set -eu
SUBVOL="__SUBVOL__"                          # o subvolume btrfs a espelhar
SNAPS="$(dirname "$SUBVOL")/.failover-snapshots" # os snapshots só de leitura, ao lado (dentro não iam no envio)

set -f # sem globbing: o pedido divide-se em palavras e nada mais
# shellcheck disable=SC2086
set -- ${SSH_ORIGINAL_COMMAND:-}
set +f
cmd=${1:-}
parent=${2:-}
case "$parent" in
'' | [0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]) ;;
*)
	echo "snapshot-pai inválido" >&2
	exit 2
	;;
esac
mkdir -p "$SNAPS"
case "$cmd" in
check) # o subvolume, o espaço e os snapshots que cá estão
	if btrfs subvolume show "$SUBVOL" >/dev/null 2>&1; then echo "subvolume ok"; else echo "subvolume $SUBVOL não é um subvolume btrfs"; fi
	echo "livre $(df -h --output=avail "$SUBVOL" | tail -1 | tr -d ' ')"
	echo "snapshots $(find "$SNAPS" -mindepth 1 -maxdepth 1 -printf '%f ')"
	;;
send)
	new=$(date +%Y%m%d-%H%M%S)
	btrfs subvolume snapshot -r "$SUBVOL" "$SNAPS/$new" >&2
	if [ -n "$parent" ] && [ -d "$SNAPS/$parent" ]; then
		btrfs send -q -p "$SNAPS/$parent" "$SNAPS/$new"
	else
		btrfs send -q "$SNAPS/$new"
	fi
	for s in "$SNAPS"/*; do # só o mais recente fica, como pai do próximo envio
		[ "$s" != "$SNAPS/$new" ] && btrfs subvolume delete "$s" >&2
	done
	;;
*)
	echo "uso: check | send [snapshot-pai]" >&2
	exit 2
	;;
esac
`
