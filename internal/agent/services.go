package agent

import (
	"cmp"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Services added, edited and removed from the UI. The name never changes:
// it is in the compose projects, the snapshots, the state and the tokens.

type mirrorDir struct {
	Dir string `json:"dir"`
}

// getMirror lists the folders of the mirror with a docker-compose.yml, the
// ones a service can be made of; the NPM's is not one.
func (a *Agent) getMirror(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	root := a.cfg.mirrorRoot()
	npm, ignored := a.cfg.NPM.Dir, a.cfg.Ignored
	a.mu.Unlock()
	out := []mirrorDir{}
	for _, d := range mirrorDirs(root, npm) {
		if !slices.Contains(ignored, d) {
			out = append(out, mirrorDir{d})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// postMirrorIgnore leaves a folder of the mirror out of the discovery and of
// the mirror's watch (an old container's, still in the backup), or takes it back.
func (a *Agent) postMirrorIgnore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir    string `json:"dir"`
		Ignore bool   `json:"ignore"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !isRel(req.Dir) || strings.Contains(req.Dir, "..") || strings.Contains(req.Dir, "/") {
		http.Error(w, "tem de ser uma pasta do espelho", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Ignore && slices.ContainsFunc(a.cfg.Services, func(s Service) bool { return s.Dir == req.Dir }) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a pasta " + req.Dir + " é de um serviço protegido: remove-o primeiro"})
		return
	}
	next := a.cfg
	next.Ignored = slices.DeleteFunc(slices.Clone(a.cfg.Ignored), func(d string) bool { return d == req.Dir })
	msg := "pasta " + req.Dir + " já não é ignorada"
	if req.Ignore {
		next.Ignored = append(next.Ignored, req.Dir)
		slices.Sort(next.Ignored)
		msg = "pasta " + req.Dir + " ignorada"
	}
	if err := saveConfig(a.cfgPath, &next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "guardar configuração: " + err.Error()})
		return
	}
	a.cfg = next
	a.st.MirrorNew = slices.DeleteFunc(a.st.MirrorNew, func(d string) bool { return d == req.Dir })
	a.done(w, "", msg)
}

func (a *Agent) getServiceOverride(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	sv, ok := a.service(r.URL.Query().Get("name"))
	dir := a.cfg.Paths.OverridesDir
	a.mu.Unlock()
	if !ok {
		http.Error(w, "serviço desconhecido", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if sv.Override != "" {
		b, _ := os.ReadFile(filepath.Join(dir, sv.Override))
		_, _ = w.Write(b)
	}
}

type serviceReq struct {
	New           bool   `json:"new"`
	Name          string `json:"name"`
	Dir           string `json:"dir"`
	Host          string `json:"host"`
	WaitMin       int    `json:"wait_min"`
	StabilityMin  int    `json:"stability_min"`
	Icon          string `json:"icon"`
	OverrideYAML  string `json:"override_yaml"`
	RequireFreeIP string `json:"require_free_ip"`
}

// postService adds a service (new) or edits one. What runs while the service
// is away from the server (its folder, address, override, IP) only changes
// with it in NORMAL.
//
//nolint:gocognit,cyclop // one request, checked in the order the form reads
func (a *Agent) postService(w http.ResponseWriter, r *http.Request) {
	var req serviceReq
	if !decode(w, r, &req) {
		return
	}
	// The icon is downloaded before the lock: it may take seconds. The same
	// as saved, with its icon kept, is not asked again: the CDN down must not
	// stop the service from being edited.
	a.mu.Lock()
	prev, _ := a.service(req.Name)
	a.mu.Unlock()
	var iconBytes []byte
	if req.Icon = strings.TrimSpace(req.Icon); req.Icon != "" {
		n, ok := iconName(req.Icon)
		if !ok {
			fieldErr(w, "icon", "tem de ser o nome de um ícone do dashboardicons.com (por exemplo speedtest-tracker) ou o link da sua página")
			return
		}
		req.Icon = n
	}
	if req.Icon != "" && (req.Icon != prev.Icon || a.iconV(req.Name) == "") {
		var err error
		if iconBytes, err = a.downloadIcon(iconSources(Service{Name: req.Name, Icon: req.Icon})); err != nil {
			fieldErr(w, "icon", "não consegui usar este ícone: "+err.Error())
			return
		}
	}
	// The override is checked with docker compose before the lock too: it
	// runs a process. Only putting the checked file in place happens under it.
	yml := strings.TrimSpace(req.OverrideYAML)
	a.mu.Lock()
	paths, before, existed := a.cfg.Paths, Service{}, false
	if j := slices.IndexFunc(a.cfg.Services, func(s Service) bool { return s.Name == req.Name }); j >= 0 {
		before, existed = a.cfg.Services[j], true
	}
	a.mu.Unlock()
	if !validName.MatchString(req.Name) || req.Name == "npm" {
		fieldErr(w, "name", "só minúsculas, números, - e _, a começar por letra ou número (npm está reservado)")
		return
	}
	file := req.Name + ".override.yml"
	if existed && before.Override != "" {
		file = before.Override
	}
	var checked string // a checked override, waiting to be put in place
	if yml != "" && (!existed || !sameOverride(paths.OverridesDir, before, yml) || strings.TrimSpace(req.Dir) != before.Dir) {
		dir := strings.TrimSpace(req.Dir)
		if !isRel(dir) || strings.Contains(dir, "..") { // never above the mirror
			fieldErr(w, "dir", "tem de ser uma pasta dentro do espelho")
			return
		}
		compose := filepath.Join(a.cfg.mirrorRoot(), dir, "docker-compose.yml")
		if !fileExists(compose) {
			fieldErr(w, "dir", "não há docker-compose.yml nesta pasta do espelho")
			return
		}
		var ok bool
		if checked, ok = a.checkOverride(w, paths.OverridesDir, req.Name, file, compose, yml); !ok {
			return
		}
		defer func() { _ = os.Remove(checked) }() // gone by rename when used
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.cfg.Services, func(s Service) bool { return s.Name == req.Name })
	switch {
	case req.New && i >= 0:
		fieldErr(w, "name", "já existe um serviço com este nome")
		return
	case !req.New && i < 0:
		http.Error(w, "serviço desconhecido", http.StatusNotFound)
		return
	}
	sv := Service{Name: req.Name, Dir: strings.TrimSpace(req.Dir), Host: strings.TrimSpace(req.Host), WaitMin: req.WaitMin,
		StabilityMin: req.StabilityMin, Icon: req.Icon, RequireFreeIP: strings.TrimSpace(req.RequireFreeIP)}
	var old Service
	if i >= 0 {
		old = a.cfg.Services[i]
	}
	if yml != "" {
		sv.Override = cmp.Or(old.Override, sv.Name+".override.yml")
	}
	if i >= 0 && !a.home(a.svc(sv.Name)) {
		if sv.Dir != old.Dir || sv.Host != old.Host || sv.RequireFreeIP != old.RequireFreeIP || !sameOverride(a.cfg.Paths.OverridesDir, old, yml) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a pasta, o endereço, o override e o IP só mudam com o serviço no servidor"})
			return
		}
	}
	next := a.cfg
	next.Services = slices.Clone(a.cfg.Services)
	if i >= 0 {
		next.Services[i] = sv
	} else {
		next.Services = append(next.Services, sv)
	}
	next.Ignored = slices.DeleteFunc(slices.Clone(a.cfg.Ignored), func(d string) bool { return d == sv.Dir }) // protected: not ignored
	if err := next.validate(); err != nil {
		var fe *FieldError
		if errors.As(err, &fe) {
			fieldErr(w, fe.Field, fe.Msg)
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	if strings.Contains(sv.Dir, "..") { // validate() already refused it; kept where the path is built
		fieldErr(w, "dir", "tem de ser uma pasta dentro do espelho")
		return
	}
	compose := filepath.Join(next.mirrorRoot(), sv.Dir, "docker-compose.yml")
	if !fileExists(compose) {
		fieldErr(w, "dir", "não há docker-compose.yml nesta pasta do espelho")
		return
	}
	if clustered(compose) {
		fieldErr(w, "dir", "o Technitium corre em cluster no servidor e no TNAS: nunca faz failover")
		return
	}
	undo := func() {}
	if checked != "" {
		var err error
		if undo, err = placeOverride(checked, filepath.Join(next.Paths.OverridesDir, sv.Override)); err != nil {
			fieldErr(w, "override_yaml", "guardar o override: "+err.Error())
			return
		}
	}
	if err := saveConfig(a.cfgPath, &next); err != nil {
		undo()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "guardar configuração: " + err.Error()})
		return
	}
	a.cfg = next
	a.now = time.Now()
	if iconBytes != nil {
		a.storeIcon(sv.Name, iconSources(sv), iconBytes)
	} else {
		a.iconJobs.Go(a.fetchIcons) // no link: dashboard-icons by name or folder
	}
	if req.New || sv.Dir != old.Dir || sv.Override != old.Override || checked != "" { // the stack changed: its images may be others
		go a.scanImages()
	}
	a.done(w, sv.Name, map[bool]string{true: "serviço adicionado", false: "serviço alterado"}[req.New])
}

// sameOverride: yml is what the service's override already says. A save that
// leaves it alone neither rewrites it nor runs compose on it. CRLF (edited
// from Windows) reads as LF, as the browser's textarea does.
func sameOverride(overridesDir string, old Service, yml string) bool {
	if old.Override == "" {
		return yml == ""
	}
	cur, _ := os.ReadFile(filepath.Join(overridesDir, old.Override))
	return yml == strings.TrimSpace(strings.ReplaceAll(string(cur), "\r\n", "\n"))
}

// checkOverride checks yml as YAML and with docker compose against the
// service's compose, in a file beside the final one, and returns that file.
func (a *Agent) checkOverride(w http.ResponseWriter, dir, name, file, compose, yml string) (string, bool) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(yml), &m); err != nil || m == nil {
		msg := "tem de ser um YAML com as chaves do compose"
		if err != nil {
			msg = "YAML inválido: " + err.Error()
		}
		fieldErr(w, "override_yaml", msg)
		return "", false
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fieldErr(w, "override_yaml", "criar a pasta dos overrides: "+err.Error())
		return "", false
	}
	tmp := filepath.Join(dir, "."+file+".check")
	if err := writeAtomic(tmp, []byte(yml+"\n")); err != nil {
		fieldErr(w, "override_yaml", "guardar o override: "+err.Error())
		return "", false
	}
	if err := a.sys.Run("docker", "compose", "-p", "failover-"+name, "-f", compose, "-f", tmp, "config", "-q"); err != nil {
		_ = os.Remove(tmp)
		fieldErr(w, "override_yaml", "o docker compose recusou-o: "+err.Error())
		return "", false
	}
	return tmp, true
}

// placeOverride puts a checked override in place; undo puts back what was
// there, for when the config cannot be saved.
func placeOverride(checked, final string) (undo func(), err error) {
	prev, prevErr := os.ReadFile(final)
	if err = os.Rename(checked, final); err != nil {
		return nil, err
	}
	return func() {
		if prevErr == nil {
			_ = writeAtomic(final, prev)
		} else {
			_ = os.Remove(final)
		}
	}, nil
}

// postServiceRemove takes a service out of the config, the state and the
// tokens. Its override file stays: it may have been written by hand.
func (a *Agent) postServiceRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.cfg.Services, func(s Service) bool { return s.Name == req.Name })
	if i < 0 {
		http.Error(w, "serviço desconhecido", http.StatusNotFound)
		return
	}
	if !a.home(a.svc(req.Name)) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "só se remove com o serviço no servidor"})
		return
	}
	next := a.cfg
	next.Services = slices.Delete(slices.Clone(a.cfg.Services), i, i+1)
	if err := saveConfig(a.cfgPath, &next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "guardar configuração: " + err.Error()})
		return
	}
	a.cfg = next
	delete(a.st.Services, req.Name)
	delete(a.beats, req.Name)
	delete(a.st.Images, req.Name)
	a.dropIcon(req.Name)
	a.now = time.Now()
	a.done(w, req.Name, "serviço removido")
}
