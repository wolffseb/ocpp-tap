package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wolffseb/ocpp-tap/internal/forwarder"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const valid = `{
  "version": 1,
  "listen": ":9000",
  "tls_cert_file": "/data/cert.pem",
  "tls_key_file": "/data/key.pem",
  "upstream_ca_file": "/data/ca.pem",
  "chargers": [
    {"id": "CP1", "password": "pw1", "upstream_url": "wss://cpms.example/ocpp/CP1",
     "upstream_username": "u1", "upstream_password": "up1"},
    {"id": "CP2", "upstream_url": "ws://10.0.0.5:8180/steve/websocket/CentralSystemService/CP2"}
  ]
}`

func TestLoadValid(t *testing.T) {
	t.Parallel()

	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.TLSCertFile != "/data/cert.pem" || c.TLSKeyFile != "/data/key.pem" || c.UpstreamCAFile != "/data/ca.pem" {
		t.Errorf("top level = %+v", c)
	}
	got, ok := c.Charger("CP1")
	want := forwarder.Charger{ID: "CP1", Password: "pw1", UpstreamURL: "wss://cpms.example/ocpp/CP1", UpstreamUsername: "u1", UpstreamPassword: "up1"}
	if !ok || got != want {
		t.Errorf("CP1 = %+v, %v", got, ok)
	}
	if _, ok := c.Charger("CP2"); !ok {
		t.Error("CP2 missing")
	}
	if _, ok := c.Charger("CP3"); ok {
		t.Error("CP3 found")
	}
	// A *Config is what the forwarder reads.
	var _ forwarder.Chargers = c
}

func TestLoadDefaultsListen(t *testing.T) {
	t.Parallel()

	c, err := Load(write(t, `{"version":1,"chargers":[{"id":"A","upstream_url":"ws://h/A"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" {
		t.Errorf("Listen = %q, want :9000", c.Listen)
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ body, want string }{
		"not json":        {`{`, "config"},
		"unknown field":   {`{"version":1,"chargers":[],"listne":":1"}`, "listne"},
		"missing version": {`{"chargers":[]}`, "version"},
		"newer version":   {`{"version":2,"chargers":[]}`, "version 2"},
		"empty id":        {`{"version":1,"chargers":[{"id":"","upstream_url":"ws://h/x"}]}`, "id"},
		"slash in id":     {`{"version":1,"chargers":[{"id":"a/b","upstream_url":"ws://h/x"}]}`, "a/b"},
		"duplicate id":    {`{"version":1,"chargers":[{"id":"A","upstream_url":"ws://h/A"},{"id":"A","upstream_url":"ws://h/A"}]}`, "duplicate"},
		"no upstream":     {`{"version":1,"chargers":[{"id":"A"}]}`, "upstream_url"},
		"http upstream":   {`{"version":1,"chargers":[{"id":"A","upstream_url":"http://h/A"}]}`, "ws:// or wss://"},
		"no host":         {`{"version":1,"chargers":[{"id":"A","upstream_url":"ws:///A"}]}`, "host"},
		"creds in url":    {`{"version":1,"chargers":[{"id":"A","upstream_url":"ws://u:p@h/A"}]}`, "upstream_password"},
		"half tls":        {`{"version":1,"tls_cert_file":"/c.pem","chargers":[]}`, "tls_key_file"},
		"user without pw": {`{"version":1,"chargers":[{"id":"A","upstream_url":"ws://h/A","upstream_username":"u"}]}`, "upstream_password"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(write(t, tc.body))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

// Errors must never echo a password back, they end up in logs.
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	t.Parallel()

	_, err := Load(write(t, `{"version":1,"chargers":[{"id":"A","password":"hunter2","upstream_password":"tiger","upstream_url":"http://h/A"}]}`))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, secret := range []string{"hunter2", "tiger"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
}

func TestChargerIDs(t *testing.T) {
	t.Parallel()

	c, err := Load(write(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.ChargerIDs(), ","); got != "CP1,CP2" {
		t.Errorf("ChargerIDs = %s", got)
	}
}
