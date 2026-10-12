package agent

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"
)

// What a first start needs, inside the binary, so the TNAS only needs the compose file.
var (
	//go:embed config/failover.yml
	defaultConfig []byte
)

// FirstRun writes the shipped config when there is none yet.
func FirstRun(cfgPath string) error {
	if fileExists(cfgPath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	slog.Info("sem configuração: criada a por defeito (modo observe)", "file", cfgPath)
	return writeAtomic(cfgPath, defaultConfig)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// chownLikeDir gives a file the owner of its directory: the agent runs as
// root in the container, and the files must stay editable from the TNAS.
func chownLikeDir(path string) {
	if fi, err := os.Stat(filepath.Dir(path)); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(path, int(st.Uid), int(st.Gid))
		}
	}
}

// creds is the UI login, kept in user.yml next to the config. Only the
// bcrypt hash is stored: the password is in no file and in no log.
type creds struct {
	User         string `yaml:"user"`
	PasswordHash string `yaml:"password_hash"`
	APITokenHash string `yaml:"api_token_sha256,omitempty"` // GET /api/ with this token, for a dashboard
}

var bcryptCost = bcrypt.DefaultCost // tests lower it: the race detector makes bcrypt slow

func newCreds(user, pw string) (*creds, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcryptCost)
	if err != nil {
		return nil, err
	}
	return &creds{User: user, PasswordHash: string(h)}, nil
}

// loadUser reads user.yml; without one there is no account yet (nil), and
// the first visit makes it.
func loadUser(path string) (*creds, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c creds
	if err := yaml.Unmarshal(b, &c); err != nil || c.User == "" || c.PasswordHash == "" {
		return nil, fmt.Errorf("%s inválido: apaga-o e reinicia para criar a conta outra vez", path)
	}
	return &c, nil
}

func saveUser(path string, c *creds) error {
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return writeAtomic(path, append([]byte("# Gerido pela interface. Só guarda os hashes da password (bcrypt) e do token da API (sha256).\n"), b...))
}
