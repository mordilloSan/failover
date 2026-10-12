package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Service icons come from dashboard-icons (dashboardicons.com, what Homepage
// and Homarr use): the one named in the form, or else the service's name and
// then its folder. Only from its CDN, never a link of anyone's choosing. The
// agent downloads it once into the state folder and the page gets it from
// there, never from the internet.
const (
	iconsCDN   = "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/"
	maxIcon    = 512 << 10
	iconRetry  = 24 * time.Hour // a guess that found nothing is tried again after this
	iconMissed = "miss "
)

func (a *Agent) iconDir() string { return filepath.Join(filepath.Dir(a.statePath), "icons") }

// iconPath is where name's icon is kept; false for a name that is not a
// service's, which could reach outside the folder. Every use goes through it.
func (a *Agent) iconPath(name string) (string, bool) {
	if !validName.MatchString(name) || strings.Contains(name, "..") {
		return "", false
	}
	return filepath.Join(a.iconDir(), name), true
}

var reIconName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// iconName is the dashboard-icons name in s: the name itself, or a link to
// its page on dashboardicons.com or to its file on the CDN. false for
// anything else.
func iconName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return "", false
		}
		switch h := strings.TrimPrefix(u.Hostname(), "www."); {
		case h == "dashboardicons.com", h == "cdn.jsdelivr.net" && strings.HasPrefix(u.Path, "/gh/homarr-labs/dashboard-icons"):
		default:
			return "", false
		}
		s = path.Base(u.Path)
		if ext := path.Ext(s); slices.Contains([]string{".svg", ".png", ".webp"}, ext) {
			s = strings.TrimSuffix(s, ext)
		}
	}
	s = strings.ToLower(s)
	return s, reIconName.MatchString(s) && !strings.Contains(s, "..")
}

// isIconName: s is already a dashboard-icons name, as saved.
func isIconName(s string) bool { n, ok := iconName(s); return ok && n == s }

// iconSources is where a service's icon comes from, in order: the name given,
// or its name and then its folder, SVG first (not every icon has one) and
// then PNG.
func iconSources(sv Service) []string {
	names := []string{sv.Name}
	if n, ok := iconName(sv.Icon); ok && sv.Icon != "" {
		names = []string{n}
	} else if d := filepath.Base(sv.Dir); d != sv.Name && validName.MatchString(d) {
		names = append(names, d)
	}
	var out []string
	for _, n := range names {
		out = append(out, iconsCDN+"svg/"+n+".svg", iconsCDN+"png/"+n+".png")
	}
	return out
}

// iconType is the image's content type, or "" when it is not one the page shows.
func iconType(b []byte) string {
	t := http.DetectContentType(b)
	for _, ok := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon", "image/vnd.microsoft.icon"} {
		if t == ok {
			return t
		}
	}
	if head := bytes.ToLower(b[:min(len(b), 4096)]); bytes.Contains(head, []byte("<svg")) && !bytes.Contains(head, []byte("<html")) {
		return "image/svg+xml"
	}
	return ""
}

// downloadIcon returns the first source that answers with an image.
func (a *Agent) downloadIcon(srcs []string) ([]byte, error) {
	err := errors.New("sem fontes")
	for _, u := range srcs {
		var b []byte
		if b, err = a.sys.Get(u, ""); err == nil {
			switch {
			case len(b) > maxIcon:
				err = errors.New("maior do que 512 KB")
			case iconType(b) == "":
				err = errors.New("não é uma imagem (PNG, JPEG, WebP, GIF, SVG ou ICO)")
			default:
				return b, nil
			}
		}
	}
	return nil, err
}

func rev(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:6]) }

// storeIcon keeps b as the icon of name, with where it came from; nil
// records a miss. A failure to write only goes to the log.
func (a *Agent) storeIcon(name string, srcs []string, b []byte) {
	file, ok := a.iconPath(name)
	if !ok {
		return
	}
	a.iconMu.Lock()
	defer a.iconMu.Unlock()
	dir, src := a.iconDir(), strings.Join(srcs, " ")
	err := os.MkdirAll(dir, 0o755)
	chownLikeDir(dir) // the state folder's owner, not root
	if b == nil {
		src = iconMissed + src
		_ = os.Remove(file)
		delete(a.iconRev, name)
	} else if err == nil {
		if err = writeAtomic(file, b); err == nil {
			a.iconRev[name] = rev(b)
		}
	}
	if err == nil {
		err = writeAtomic(file+".src", []byte(src))
	}
	if err != nil {
		slog.Error("guardar o ícone", "svc", name, "error", err)
	}
}

func (a *Agent) dropIcon(name string) {
	file, ok := a.iconPath(name)
	if !ok {
		return
	}
	a.iconMu.Lock()
	defer a.iconMu.Unlock()
	_ = os.Remove(file)
	_ = os.Remove(file + ".src")
	delete(a.iconRev, name)
}

// fetchIcons gets the icon of each service whose sources changed since the
// last try, or whose guess found nothing a day ago. It runs at start and
// after a service is saved, outside mu.
func (a *Agent) fetchIcons() {
	a.mu.Lock()
	svcs := slices.Clone(a.cfg.Services)
	a.mu.Unlock()
	a.fetchIconsFrom(svcs)
}

// fetchIconsFrom works on a copy of the services, one pass at a time. Before
// keeping an icon it checks the service is still there with the same sources:
// a link saved, or the service removed, during the pass wins over it.
func (a *Agent) fetchIconsFrom(svcs []Service) {
	a.iconPass.Lock()
	defer a.iconPass.Unlock()
	current := func(sv Service, srcs []string) bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		now, ok := a.service(sv.Name)
		return ok && slices.Equal(iconSources(now), srcs)
	}
	for _, sv := range svcs {
		srcs := iconSources(sv)
		file, ok := a.iconPath(sv.Name)
		if !ok {
			continue
		}
		src := file + ".src"
		had, _ := os.ReadFile(src)
		fi, _ := os.Stat(src)
		switch {
		case string(had) == strings.Join(srcs, " "):
			if b, err := os.ReadFile(file); err == nil {
				a.iconMu.Lock()
				a.iconRev[sv.Name] = rev(b)
				a.iconMu.Unlock()
			}
			continue
		case string(had) == iconMissed+strings.Join(srcs, " ") && time.Since(fi.ModTime()) < iconRetry:
			continue
		}
		b, _ := a.downloadIcon(srcs)
		if current(sv, srcs) {
			a.storeIcon(sv.Name, srcs, b)
		}
	}
	a.mu.Lock()
	a.publish()
	a.mu.Unlock()
}

func (a *Agent) iconV(name string) string {
	a.iconMu.Lock()
	defer a.iconMu.Unlock()
	return a.iconRev[name]
}

// getIcon serves a stored icon. The policy keeps an SVG from running
// anything even when opened on its own; the address carries the version, so
// it is cached for good.
func (a *Agent) getIcon(w http.ResponseWriter, r *http.Request) {
	file, ok := a.iconPath(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(file)
	if err != nil || iconType(b) == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", iconType(b))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(b)
}
