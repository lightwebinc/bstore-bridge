package bridge

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bstore/provider"
	"github.com/lightwebinc/bstore/storerec"
)

func goodConfig(t *testing.T) *Config {
	k, _ := ec.NewPrivateKey()
	return &Config{
		Provider: provider.Config{Data: t.TempDir(), PublicURL: "https://bridge.example.com/front", Node: "https://node.example.com",
			Settle: "rpc:https://node.example.com/rpc", Headers: "https://headers.example.com", Hosts: []string{"https://overlay.example.com"},
			Buckets: []provider.BucketConfig{{Bucket: "example_photos_hmqzvtkwrx"}}},
		Incumbent: Incumbent{URL: "https://uhrp.example.com", Identity: hex.EncodeToString(k.PubKey().Compressed()),
			Lookup: []string{"https://overlay.example.com"}},
	}
}

func TestConfigChecks(t *testing.T) {
	if err := goodConfig(t).Check(); err != nil {
		t.Fatal(err)
	}
	c := goodConfig(t)
	_ = c.Check()
	if c.Provider.Backend != "bridge" || c.Incumbent.AdWaitSeconds == 0 || c.StatementCheckSeconds == 0 {
		t.Fatalf("defaults: %+v", c)
	}
	bad := map[string]func(c *Config){
		"native backend":      func(c *Config) { c.Provider.Backend = "native" },
		"http incumbent":      func(c *Config) { c.Incumbent.URL = "http://uhrp.example.com" },
		"trailing slash":      func(c *Config) { c.Incumbent.URL = "https://uhrp.example.com/" },
		"query":               func(c *Config) { c.Incumbent.URL = "https://uhrp.example.com?x=1" },
		"user":                func(c *Config) { c.Incumbent.URL = "https://u:p@uhrp.example.com" },
		"chirp path":          func(c *Config) { c.Incumbent.URL = "https://uhrp.example.com/chirp/v1" },
		"bad identity":        func(c *Config) { c.Incumbent.Identity = "02" + strings.Repeat("ff", 32) },
		"upper-case identity": func(c *Config) { c.Incumbent.Identity = strings.ToUpper(c.Incumbent.Identity) },
		"anyone key":          func(c *Config) { c.Incumbent.Identity = hex.EncodeToString(storerec.AnyoneKey) },
		"no lookup":           func(c *Config) { c.Incumbent.Lookup = nil },
		"lookup scheme":       func(c *Config) { c.Incumbent.Lookup = []string{"file:///etc/passwd"} },
		"missing ca":          func(c *Config) { c.Incumbent.CA = "/nonexistent/ca.pem" },
		"same url":            func(c *Config) { c.Provider.PublicURL = c.Incumbent.URL },
		"export":              func(c *Config) { c.Provider.Export = "/srv/export" },
	}
	for name, f := range bad {
		c := goodConfig(t)
		f(c)
		if err := c.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadConfigRefusesUnknownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(`{"provider":{},"incumbent":{},"surprise":1}`), 0o600)
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("unknown key: %v", err)
	}
}
