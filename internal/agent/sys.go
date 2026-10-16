package agent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

type RealSys struct{}

func (s RealSys) Run(name string, args ...string) error {
	_, err := s.Output(name, args...)
	return err
}

// Output returns stdout; stderr (compose warnings, errors) only goes into the error.
func (RealSys) Output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/" // compose must never pick up a compose file from the working directory
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(cmp.Or(stderr.String(), string(out)))
		if len(msg) > 400 {
			msg = "…" + msg[len(msg)-400:]
		}
		return string(out), fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return string(out), nil
}

// CertError: host answered, with a certificate that did not verify (expired,
// another name, an unknown CA). It is up: the TNAS serves the same
// certificate, from the mirror, so a failover would not help.
type CertError struct {
	Host string
	Err  error
}

func (e *CertError) Error() string {
	return "certificado de " + e.Host + " inválido: " + e.Err.Error()
}

// certOK is what was wrong with the certificate ("" when it verified), and
// err with a CertError counted as up.
func certOK(err error) (string, error) {
	var ce *CertError
	if errors.As(err, &ce) {
		return ce.Error(), nil
	}
	return "", err
}

// checkRoots are the CAs Check trusts; nil: the system's. Tests set their own.
var checkRoots *x509.CertPool

// Check asks https://host/ with the connection sent to ip (like curl
// --resolve), verifying the certificate for host.
func (RealSys) Check(host, ip string) error {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	c := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Like curl --resolve: connect to ip, keep host for SNI and the Host header.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			},
			TLSClientConfig:   &tls.Config{RootCAs: checkRoots, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := c.Get("https://" + host + "/")
	var ve *tls.CertificateVerificationError
	if errors.As(err, &ve) {
		return &CertError{Host: host, Err: ve.Err}
	}
	if err != nil {
		return unwrapURL(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode >= 500 { // a 401 or a 404 is the service answering: only a 5xx, or no answer, is down
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (RealSys) PortFree(proto, addr string) error {
	if strings.HasPrefix(proto, "udp") {
		c, err := net.ListenPacket(proto, addr)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	l, err := net.Listen("tcp", addr)
	if err == nil {
		_ = l.Close()
	}
	return err
}

// Resolve asks the DNS resolver at ip for a name no cache holds, so the answer
// has to come from the internet; NXDOMAIN counts as reached.
func (RealSys) Resolve(ip string) error {
	return lookupAt(ip, rand.Text()+".docker.io.") // rooted: no search domains
}

// Answers asks the DNS server at ip for a name of its own zone: any answer,
// "no such name" too, says it is up, with or without the internet.
func (RealSys) Answers(ip, zone string) error {
	return lookupAt(ip, rand.Text()+"."+zone+".")
}

func lookupAt(ip, name string) error {
	d := &net.Dialer{Timeout: 5 * time.Second}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, net.JoinHostPort(ip, "53"))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := r.LookupHost(ctx, name)
	if de, ok := errors.AsType[*net.DNSError](err); ok && de.IsNotFound {
		return nil
	}
	return err
}

var apiClient = &http.Client{Timeout: 10 * time.Second}

// loopClient is for a URL on this box, such as the TNAS's own Technitium on
// https://127.0.0.1:53443: its certificate is self-signed, and what goes to
// loopback never leaves the box. A redirect out of the box is refused.
var loopClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // loopback only
	},
	CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if !loopback(req.URL) {
			return errors.New("redirect para fora desta máquina")
		}
		return nil
	},
}

func loopback(u *url.URL) bool {
	h := u.Hostname()
	return h == "localhost" || net.ParseIP(h).IsLoopback()
}

func (RealSys) Get(u, bearer string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("URL inválido")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	c := apiClient
	if loopback(req.URL) {
		c = loopClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, unwrapURL(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return body, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, err
}

// unwrapURL drops the URL from errors: a URL may carry a secret (the
// Technitium login's password).
func unwrapURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}
