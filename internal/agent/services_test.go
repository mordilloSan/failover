package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// svcSetup points the mirror and the overrides at a temporary folder with
// the composes of nextcloud and paperless (and a folder without one).
func svcSetup(t *testing.T) (*Agent, *fake) {
	t.Helper()
	a, f := setup(t)
	dir := t.TempDir()
	for _, d := range []string{"nextcloud", "npm", "paperless"} {
		if err := os.MkdirAll(filepath.Join(dir, "mirror", "homelab", d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mirror", "homelab", d, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "mirror", "homelab", "sem-compose"), 0o755); err != nil {
		t.Fatal(err)
	}
	// in a cluster on the server and the TNAS: never offered, never added
	tech := filepath.Join(dir, "mirror", "homelab", "technitium")
	if err := os.MkdirAll(tech, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tech, "docker-compose.yml"), []byte("services:\n  dns:\n    image: technitium/dns-server:latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.Paths.MirrorSubvol = filepath.Join(dir, "mirror")
	a.cfg.Paths.MirrorRoot = "homelab"
	a.cfg.Paths.OverridesDir = filepath.Join(dir, "overrides") // not there yet
	a.scanning.Store(true)                                     // the image scan a save starts would race the test's reads
	return a, f
}

const nextcloud = `{"new":true,"name":"nextcloud","dir":"nextcloud","host":"cloud.engmariz.com","wait_min":5,"stability_min":10,
	"icon":"","override_yaml":"services:\n  cron:\n    profiles: [\"disabled\"]\n"}`

func TestServiceAdd(t *testing.T) {
	a, _ := svcSetup(t)
	if code, body := postTo(t, a.postService, nextcloud); code != 204 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	sv, ok := a.service("nextcloud")
	if !ok || sv.Override != "nextcloud.override.yml" {
		t.Fatalf("serviço: %+v", sv)
	}
	if b, err := os.ReadFile(filepath.Join(a.cfg.Paths.OverridesDir, "nextcloud.override.yml")); err != nil || !strings.Contains(string(b), "cron") {
		t.Fatalf("override: %v %s", err, b)
	}
	if c, _ := LoadConfig(a.cfgPath); !slices.ContainsFunc(c.Services, func(s Service) bool { return s.Name == "nextcloud" }) {
		t.Fatal("não gravou a config")
	}
}

func TestServiceAddRefused(t *testing.T) {
	a, f := svcSetup(t)
	with := func(k, v string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(nextcloud), &m)
		var val any
		_ = json.Unmarshal([]byte(v), &val)
		m[k] = val
		b, _ := json.Marshal(m)
		return string(b)
	}
	for _, c := range []struct{ body, field string }{
		{with("name", `"vaultwarden"`), "name"},
		{with("name", `"Nextcloud"`), "name"},
		{with("name", `"npm"`), "name"},
		{with("dir", `"sem-compose"`), "dir"},
		{with("dir", `"technitium"`), "dir"},
		{with("host", `""`), "host"},
		{with("host", `"https://cloud.engmariz.com"`), "host"},
		{with("wait_min", `0`), "wait_min"},
		{with("require_free_ip", `"x"`), "require_free_ip"},
		{with("override_yaml", `"services: [x"`), "override_yaml"},
	} {
		if code, body := postTo(t, a.postService, c.body); code != 400 || !strings.Contains(body, `"field":"`+c.field+`"`) {
			t.Errorf("%s: HTTP %d %s", c.field, code, body)
		}
	}
	f.failCmd = []string{"docker compose -p failover-nextcloud"}
	if code, body := postTo(t, a.postService, nextcloud); code != 400 || !strings.Contains(body, `"field":"override_yaml"`) {
		t.Errorf("compose config a falhar: HTTP %d %s", code, body)
	}
	if _, ok := a.service("nextcloud"); ok || fileExists(filepath.Join(a.cfg.Paths.OverridesDir, "nextcloud.override.yml")) {
		t.Fatal("um pedido recusado deixou o serviço ou o override")
	}
}

func TestServiceEdit(t *testing.T) {
	a, _ := svcSetup(t)
	if code, body := postTo(t, a.postService, nextcloud); code != 204 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	edit := `{"name":"nextcloud","dir":"nextcloud","host":"cloud2.engmariz.com","wait_min":5,"stability_min":10,"icon":"","override_yaml":""}`
	underLock(a, func() { a.st.Services["nextcloud"] = &SvcState{State: Active} })
	if code, _ := postTo(t, a.postService, edit); code != 409 {
		t.Fatalf("mudou o endereço com o serviço em failover: %d", code)
	}
	keep := `{"name":"nextcloud","dir":"nextcloud","host":"cloud.engmariz.com","wait_min":15,"stability_min":10,"icon":"",
		"override_yaml":"services:\n  cron:\n    profiles: [\"disabled\"]\n"}`
	if code, body := postTo(t, a.postService, keep); code != 204 {
		t.Fatalf("a espera em failover foi recusada: %d %s", code, body)
	}
	if sv, _ := a.service("nextcloud"); sv.WaitMin != 15 {
		t.Fatalf("%+v", sv)
	}
	underLock(a, func() { a.st.Services["nextcloud"].State = Normal })
	if code, _ := postTo(t, a.postService, edit); code != 204 {
		t.Fatal("editar em NORMAL recusado")
	}
	if sv, _ := a.service("nextcloud"); sv.Override != "" || sv.Host != "cloud2.engmariz.com" {
		t.Fatalf("override vazio não o tirou: %+v", sv)
	}
	if code, _ := postTo(t, a.postService, strings.Replace(edit, "nextcloud", "nao-existe", 1)); code != 404 {
		t.Fatal("editou um serviço que não existe")
	}
}

// When the config cannot be saved, the override goes back to what it was.
func TestServiceSaveFails(t *testing.T) {
	a, _ := svcSetup(t)
	if code, _ := postTo(t, a.postService, nextcloud); code != 204 {
		t.Fatal("adicionar")
	}
	ov := filepath.Join(a.cfg.Paths.OverridesDir, "nextcloud.override.yml")
	before, _ := os.ReadFile(ov)
	ro := t.TempDir()
	a.cfgPath = filepath.Join(ro, "failover.yml")
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(ro, 0o700) }()
	body := strings.Replace(nextcloud, `"new":true,`, "", 1)
	body = strings.Replace(body, "cron", "outro", 1)
	if code, _ := postTo(t, a.postService, body); code != 500 {
		t.Fatalf("HTTP %d", code)
	}
	if after, _ := os.ReadFile(ov); string(after) != string(before) {
		t.Fatalf("o override ficou com o conteúdo novo: %s", after)
	}
}

func TestServiceRemove(t *testing.T) {
	a, _ := svcSetup(t)
	if code, _ := postTo(t, a.postService, nextcloud); code != 204 {
		t.Fatal("adicionar")
	}
	underLock(a, func() { a.beats["nextcloud"] = []Beat{{S: "up"}} })
	underLock(a, func() { a.st.Services["nextcloud"] = &SvcState{State: Active} })
	if code, _ := postTo(t, a.postServiceRemove, `{"name":"nextcloud"}`); code != 409 {
		t.Fatalf("removeu um serviço em failover: %d", code)
	}
	underLock(a, func() { a.st.Services["nextcloud"].State = Normal })
	if code, _ := postTo(t, a.postServiceRemove, `{"name":"nextcloud"}`); code != 204 {
		t.Fatalf("remover: %d", code)
	}
	if _, ok := a.service("nextcloud"); ok || a.st.Services["nextcloud"] != nil || a.beats["nextcloud"] != nil {
		t.Fatal("ficou algo do serviço removido")
	}
	if code, _ := postTo(t, a.postServiceRemove, `{"name":"nextcloud"}`); code != 404 {
		t.Fatalf("remover um que não existe: %d", code)
	}
}

func TestMirrorDirs(t *testing.T) {
	a, _ := svcSetup(t)
	w := httptest.NewRecorder()
	a.getMirror(w, httptest.NewRequest(http.MethodGet, "/api/mirror", nil))
	var dirs []mirrorDir
	if err := json.Unmarshal(w.Body.Bytes(), &dirs); err != nil || len(dirs) != 2 || dirs[0].Dir != "nextcloud" || dirs[1].Dir != "paperless" {
		t.Fatalf("%s", w.Body)
	}
}

// Outside NORMAL, a save that leaves the override as it is neither rewrites
// it nor runs compose on it: only wait, stability, icon and token change.
// An override written with CRLF (edited from Windows) counts as unchanged.
func TestServiceEditKeepsOverride(t *testing.T) {
	a, f := svcSetup(t)
	if code, _ := postTo(t, a.postService, nextcloud); code != 204 {
		t.Fatal("adicionar")
	}
	ov := filepath.Join(a.cfg.Paths.OverridesDir, "nextcloud.override.yml")
	crlf := "services:\r\n  cron:\r\n    profiles: [\"disabled\"]\r\n"
	if err := os.WriteFile(ov, []byte(crlf), 0o644); err != nil {
		t.Fatal(err)
	}
	underLock(a, func() { a.st.Services["nextcloud"] = &SvcState{State: Active} })
	f.failCmd = []string{"docker compose -p failover-nextcloud"}
	keep := `{"name":"nextcloud","dir":"nextcloud","host":"cloud.engmariz.com","wait_min":20,"stability_min":10,"icon":"",
		"override_yaml":"services:\n  cron:\n    profiles: [\"disabled\"]\n"}`
	if code, body := postTo(t, a.postService, keep); code != 204 {
		t.Fatalf("mudar só a espera em failover: HTTP %d %s", code, body)
	}
	if b, _ := os.ReadFile(ov); string(b) != crlf {
		t.Fatalf("reescreveu o override da cópia a correr: %q", b)
	}
}

// Two services cannot share an address (one's return would delete the
// other's DNS record) or a folder (the same containers twice).
func TestServiceDuplicateHostDir(t *testing.T) {
	a, _ := svcSetup(t)
	vw, _ := a.service("vaultwarden")
	for field, body := range map[string]string{
		"host": strings.Replace(nextcloud, "cloud.engmariz.com", vw.Host, 1),
		"dir":  strings.Replace(strings.Replace(nextcloud, `"dir":"nextcloud"`, `"dir":"npm"`, 1), `"override_yaml":"services:\n  cron:\n    profiles: [\"disabled\"]\n",`, "", 1),
	} {
		if code, got := postTo(t, a.postService, body); code != 400 || !strings.Contains(got, `"field":"`+field+`"`) {
			t.Errorf("%s repetido: HTTP %d %s", field, code, got)
		}
	}
}

// underLock changes the agent the way a handler does, so the background icon
// fetch (which publishes under mu) does not race the test.
func underLock(a *Agent, f func()) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f()
}
