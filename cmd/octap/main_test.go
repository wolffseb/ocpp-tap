package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/wolffseb/ocpp-tap/internal/config"
)

func TestRunCommands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	var out, errOut bytes.Buffer
	if got := run(ctx, []string{"version"}, &out, &errOut); got != 0 || !strings.HasPrefix(out.String(), "octap ") {
		t.Errorf("version: exit %d, out %q", got, out.String())
	}
	if got := run(ctx, nil, &out, &errOut); got != 2 {
		t.Errorf("no command: exit %d, want 2", got)
	}
	if got := run(ctx, []string{"bogus"}, &out, &errOut); got != 2 {
		t.Errorf("unknown command: exit %d, want 2", got)
	}
	errOut.Reset()
	missing := filepath.Join(t.TempDir(), "absent.json")
	if got := run(ctx, []string{"run", "-config", missing}, &out, &errOut); got != 1 || !strings.Contains(errOut.String(), "absent.json") {
		t.Errorf("missing config: exit %d, stderr %q", got, errOut.String())
	}
}

// echoUpstream accepts any charger and echoes every message back.
func echoUpstream(t *testing.T, tlsServer bool) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{Subprotocols: websocket.Subprotocols(r)}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			typ, data, err := ws.ReadMessage()
			if err != nil || ws.WriteMessage(typ, data) != nil {
				return
			}
		}
	})
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

func writeConfig(t *testing.T, c map[string]any) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startServe runs serve on a free port and returns its address and a stop
// function that reports serve's result.
func startServe(t *testing.T, cfgPath string) (string, func() error) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	stopped := false
	stop := func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("serve did not return after cancel")
			return nil
		}
	}
	t.Cleanup(func() { _ = stop() })
	return ln.Addr().String(), stop
}

func echo(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	msg := []byte(`[2,"1","Heartbeat",{}]`)
	if err := ws.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, got, err := ws.ReadMessage()
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo: %q, %v", got, err)
	}
}

func basicHeader(user, pass string) http.Header {
	return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))}}
}

func TestServeForwards(t *testing.T) {
	t.Parallel()

	up := echoUpstream(t, false)
	cfg := writeConfig(t, map[string]any{"version": 1, "chargers": []any{
		map[string]any{"id": "CP1", "password": "pw", "upstream_url": "ws" + strings.TrimPrefix(up.URL, "http") + "/ocpp/CP1"},
	}})
	addr, _ := startServe(t, cfg)

	d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}}
	ws, resp, err := d.Dial("ws://"+addr+"/ocpp/CP1", basicHeader("CP1", "pw"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	echo(t, ws)
}

// Security Profile 2: Basic Auth over TLS on the charger-facing listener, and
// a wss:// upstream trusted through upstream_ca_file.
func TestServeTLSBothSides(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	certFile, keyFile, downCert := selfSigned(t, dir)

	up := echoUpstream(t, true)
	caFile := filepath.Join(dir, "upstream-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := writeConfig(t, map[string]any{
		"version": 1, "tls_cert_file": certFile, "tls_key_file": keyFile, "upstream_ca_file": caFile,
		"chargers": []any{map[string]any{"id": "CP1", "password": "pw", "upstream_url": "wss" + strings.TrimPrefix(up.URL, "https") + "/ocpp/CP1"}},
	})
	addr, _ := startServe(t, cfg)

	roots := x509.NewCertPool()
	roots.AddCert(downCert)
	d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost"}}
	ws, resp, err := d.Dial("wss://"+addr+"/ocpp/CP1", basicHeader("CP1", "pw"))
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	echo(t, ws)

	// Plain ws:// is not served on a TLS listener.
	plain := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}, HandshakeTimeout: time.Second}
	if ws, resp, err := plain.Dial("ws://"+addr+"/ocpp/CP1", basicHeader("CP1", "pw")); err == nil {
		_ = ws.Close()
		t.Error("plain ws accepted on a TLS listener")
	} else if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestServeRejectsBadCAFile(t *testing.T) {
	t.Parallel()

	bad := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, map[string]any{"version": 1, "upstream_ca_file": bad, "chargers": []any{}})
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), ln, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("serve accepted a CA file without certificates")
	}
}

// Shutdown closes chargers with 1001 (going away) and serve returns nil.
func TestServeShutdown(t *testing.T) {
	t.Parallel()

	up := echoUpstream(t, false)
	cfg := writeConfig(t, map[string]any{"version": 1, "chargers": []any{
		map[string]any{"id": "CP1", "upstream_url": "ws" + strings.TrimPrefix(up.URL, "http") + "/ocpp/CP1"},
	}})
	addr, stop := startServe(t, cfg)

	d := websocket.Dialer{Subprotocols: []string{"ocpp1.6"}}
	ws, resp, err := d.Dial("ws://"+addr+"/ocpp/CP1", nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	if err := stop(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = ws.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
		t.Errorf("charger got %v, want close 1001", err)
	}
}

// selfSigned writes a localhost certificate and key and returns their paths
// and the parsed certificate.
func selfSigned(t *testing.T, dir string) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, cert
}
