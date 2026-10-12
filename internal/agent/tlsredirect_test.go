package agent

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// On the UI's port, plain HTTP gets a redirect to HTTPS and TLS is served;
// a client that connects and sends nothing does not hold the others up.
func TestRedirectListener(t *testing.T) {
	a, _ := setup(t)
	cfg, err := a.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }),
		ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(NewRedirectListener(ln, cfg)) }()
	defer srv.Close()
	addr := ln.Addr().String()

	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	plain := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Get("http://" + addr + "/x?y=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://"+addr+"/x?y=1" {
		t.Fatalf("HTTP %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	pem, _ := os.ReadFile(CertFile(a.statePath))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	sec := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err = sec.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("HTTPS: %d", resp.StatusCode)
	}
}

// flakyListener fails its first Accept with a temporary error, like EMFILE.
type flakyListener struct {
	net.Listener
	failed bool
}

type tempErr struct{}

func (tempErr) Error() string   { return "too many open files" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

func (l *flakyListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, tempErr{}
	}
	return l.Listener.Accept()
}

// A temporary accept error does not leave the UI dead.
func TestRedirectListenerTemporaryError(t *testing.T) {
	a, _ := setup(t)
	cfg, err := a.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }),
		ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(NewRedirectListener(&flakyListener{Listener: ln}, cfg)) }()
	defer srv.Close()
	plain := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := plain.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("a interface morreu depois de um erro temporário: %v", err)
	}
	_ = resp.Body.Close()
}

// The UI negotiates HTTP/2: one connection carries the page's many small files.
func TestUIServesHTTP2(t *testing.T) {
	a, _ := setup(t)
	cfg, err := a.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }),
		TLSConfig: cfg, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(NewRedirectListener(ln, cfg)) }()
	defer srv.Close()
	pem, _ := os.ReadFile(CertFile(a.statePath))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	cl := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := cl.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("a interface fala %s", resp.Proto)
	}
}
