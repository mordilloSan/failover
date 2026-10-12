package agent

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func postTo(t *testing.T, h http.HandlerFunc, body string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return w.Code, w.Body.String()
}

// A section saves what it names, refuses a field of another section and,
// for Rede and Caminhos, a change while a service is away from the server.
func TestSectionRede(t *testing.T) {
	a, _ := setup(t)
	if code, body := postTo(t, a.postSection, `{"section":"rede","values":{"tnas_ip":"192.168.1.250"}}`); code != 204 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	if c, _ := LoadConfig(a.cfgPath); a.cfg.TNASIP != "192.168.1.250" || c.TNASIP != "192.168.1.250" || *a.tnasIP.Load() != "192.168.1.250" {
		t.Fatal("tnas_ip não aplicado, gravado ou passado ao certificado")
	}
	for body, field := range map[string]string{
		`{"section":"rede","values":{"tnas_ip":"x"}}`:   "tnas_ip",
		`{"section":"rede","values":{"dns.ttl":120}}`:   "dns.ttl",
		`{"section":"rede","values":{"nope":1}}`:        "nope",
		`{"section":"rede","values":{"server.ip":192}}`: "server.ip",
	} {
		code, got := postTo(t, a.postSection, body)
		if code != 400 || !strings.Contains(got, `"field":"`+field+`"`) {
			t.Errorf("%s: HTTP %d %s", body, code, got)
		}
	}
	if a.cfg.TNASIP != "192.168.1.250" {
		t.Fatal("um pedido recusado mudou a config")
	}
	a.st.Services["vaultwarden"] = &SvcState{State: Active}
	if code, _ := postTo(t, a.postSection, `{"section":"rede","values":{"tnas_ip":"192.168.1.251"}}`); code != 409 {
		t.Fatalf("mudou a rede com um serviço em failover: %d", code)
	}
	if code, _ := postTo(t, a.postSection, `{"section":"rede","values":{"tnas_ip":"192.168.1.250"}}`); code != 204 {
		t.Fatalf("o mesmo valor foi recusado: %d", code)
	}
}

func TestSectionNumbersAndListen(t *testing.T) {
	a, _ := setup(t)
	if code, body := postTo(t, a.postSection, `{"section":"verificacao","values":{"start_timeout_min":"x"}}`); code != 400 || !strings.Contains(body, "start_timeout_min") {
		t.Fatalf("texto num número: %d %s", code, body)
	}
	if code, _ := postTo(t, a.postSection, `{"section":"dns","values":{"dns.ttl":120}}`); code != 204 || a.cfg.DNS.TTL != 120 {
		t.Fatalf("dns.ttl: %d %d", code, a.cfg.DNS.TTL)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if code, body := postTo(t, a.postSection, `{"section":"interface","values":{"ui.listen":"`+ln.Addr().String()+`"}}`); code != 400 || !strings.Contains(body, `"field":"ui.listen"`) {
		t.Fatalf("aceitou uma porta ocupada: %d %s", code, body)
	}
}

// Caminhos: a snapshots folder the agent can make is fine, a test snapshot
// is made and deleted, and a failed one says why.
func TestSectionCheck(t *testing.T) {
	a, f := setup(t)
	check := func() map[string]checkResult {
		code, body := postTo(t, a.postCheck, `{"section":"caminhos"}`)
		var res []checkResult
		if err := json.Unmarshal([]byte(body), &res); code != 200 || err != nil {
			t.Fatalf("HTTP %d %s", code, body)
		}
		out := map[string]checkResult{}
		for _, r := range res {
			if r.Field == "paths.snapshots_dir" && strings.Contains(r.Msg, "snapshot") {
				out["snap"] = r
			} else {
				out[r.Field] = r
			}
		}
		return out
	}
	a.cfg.Paths.SnapshotsDir = filepath.Join(t.TempDir(), "ainda-nao")
	if r := check(); !r["paths.snapshots_dir"].OK || !r["snap"].OK {
		t.Fatalf("uma pasta que o agente cria deu erro: %+v", r)
	}
	if !slices.ContainsFunc(f.cmds, func(c string) bool {
		return strings.HasPrefix(c, "btrfs subvolume delete "+a.cfg.Paths.SnapshotsDir+"/failover-teste-")
	}) {
		t.Fatalf("o snapshot de teste não foi apagado: %v", f.cmds)
	}
	if !slices.ContainsFunc(f.cmds, func(c string) bool {
		return strings.HasPrefix(c, "cp --reflink=always ") && strings.Contains(c, "/docker-compose.yml ")
	}) {
		t.Fatalf("o teste copiou mais do que um ficheiro: %v", f.cmds)
	}
	f.failCmd = []string{"cp "}
	if r := check(); r["snap"].OK || !strings.Contains(r["snap"].Msg, "mesmo volume") {
		t.Fatalf("a cópia com reflink falhou e o teste passou: %+v", r)
	}
	if left, _ := filepath.Glob(a.cfg.Paths.SnapshotsDir + "/failover-teste-*"); len(left) > 0 {
		t.Fatalf("o subvolume da cópia falhada ficou: %v", left)
	}
	a.cfg.Paths.SnapshotsDir = filepath.Join(t.TempDir(), "nao", "existe")
	f.failCmd = []string{"btrfs subvolume create"}
	if r := check(); r["paths.snapshots_dir"].OK || r["snap"].OK || !strings.Contains(r["snap"].Msg, "subvolume") {
		t.Fatalf("sem pasta-mãe e sem snapshot: %+v", r)
	}
}

func TestRestart(t *testing.T) {
	a, _ := setup(t)
	done := make(chan struct{})
	a.SetRestart(func() { close(done) })
	if code, _ := postTo(t, a.postRestart, `{}`); code != 202 {
		t.Fatalf("HTTP %d", code)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("não reiniciou")
	}
}
