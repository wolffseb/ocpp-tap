// Package config reads octap's trial-mode configuration: one JSON file on
// the data volume (ADR-0001 §4).
//
// SP1 reads it once at startup. Writing it from the wizard, hot reload, file
// mode enforcement and the admin password belong to SP4.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/wolffseb/ocpp-tap/internal/forwarder"
)

// Version is the schema version this octap reads.
const Version = 1

// DefaultListen is where chargers connect when the file names no address.
const DefaultListen = ":9000"

// Config is the parsed file. It implements forwarder.Chargers.
type Config struct {
	Version int `json:"version"`
	// Listen is the address chargers connect to.
	Listen string `json:"listen"`
	// TLSCertFile and TLSKeyFile, when both set, make the charger-facing
	// listener speak TLS (Security Profile 2).
	TLSCertFile string `json:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file"`
	// UpstreamCAFile adds PEM CA certificates trusted for wss:// upstreams,
	// on top of the system roots.
	UpstreamCAFile string    `json:"upstream_ca_file"`
	Chargers       []Charger `json:"chargers"`

	byID map[string]forwarder.Charger
}

// Charger is one charger's entry.
type Charger struct {
	ID               string `json:"id"`
	Password         string `json:"password"`
	UpstreamURL      string `json:"upstream_url"`
	UpstreamUsername string `json:"upstream_username"`
	UpstreamPassword string `json:"upstream_password"`
}

// Load reads and validates the file at path. Error messages never contain a
// password.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	c.byID = make(map[string]forwarder.Charger, len(c.Chargers))
	for _, ch := range c.Chargers {
		c.byID[ch.ID] = forwarder.Charger(ch)
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch {
	case c.Version == 0:
		return errors.New(`"version" is missing`)
	case c.Version > Version:
		return fmt.Errorf("version %d is newer than this octap understands (%d)", c.Version, Version)
	case c.Version < Version:
		return fmt.Errorf("version %d is not supported", c.Version)
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("tls_cert_file and tls_key_file must be set together")
	}
	seen := map[string]bool{}
	for i, ch := range c.Chargers {
		if ch.ID == "" {
			return fmt.Errorf("chargers[%d]: id is empty", i)
		}
		if strings.ContainsAny(ch.ID, "/?#") {
			return fmt.Errorf("chargers[%d]: id %q may not contain / ? or #", i, ch.ID)
		}
		if seen[ch.ID] {
			return fmt.Errorf("chargers[%d]: duplicate id %q", i, ch.ID)
		}
		seen[ch.ID] = true
		if err := validateUpstream(ch); err != nil {
			return fmt.Errorf("charger %q: %w", ch.ID, err)
		}
	}
	return nil
}

func validateUpstream(ch Charger) error {
	if ch.UpstreamURL == "" {
		return errors.New("upstream_url is empty")
	}
	u, err := url.Parse(ch.UpstreamURL)
	if err != nil {
		// The parse error quotes the URL, which may hold a secret.
		return errors.New("upstream_url is not a valid URL")
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return errors.New("upstream_url must start with ws:// or wss://")
	}
	if u.Host == "" {
		return errors.New("upstream_url has no host")
	}
	if u.User != nil {
		return errors.New("upstream_url must not carry credentials; use upstream_username and upstream_password")
	}
	if ch.UpstreamUsername != "" && ch.UpstreamPassword == "" {
		return errors.New("upstream_username is set but upstream_password is empty")
	}
	return nil
}

// Charger implements forwarder.Chargers.
func (c *Config) Charger(id string) (forwarder.Charger, bool) {
	ch, ok := c.byID[id]
	return ch, ok
}

// ChargerIDs lists the configured chargers in file order.
func (c *Config) ChargerIDs() []string {
	ids := make([]string, len(c.Chargers))
	for i, ch := range c.Chargers {
		ids[i] = ch.ID
	}
	return ids
}
