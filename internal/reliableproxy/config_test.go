package reliableproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigurationValidationAndUnknownFields(t *testing.T) {
	good := DefaultConfig()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Listen = "0.0.0.0:1234" }, func(c *Config) { c.Upstream = "https://untrusted.invalid/backend-api/codex" }, func(c *Config) { c.Routes = nil }, func(c *Config) { c.Routes = []RouteConfig{{ID: "same"}, {ID: "same"}} }, func(c *Config) { c.Routes = []RouteConfig{{ID: "secret/url"}} }, func(c *Config) { c.CircuitMaxSeconds = 1 }, func(c *Config) { c.MaxRequestMiB = 200 }} {
		c := DefaultConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("invalid configuration accepted", c.Listen)
		}
	}
	p := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(good)
	os.WriteFile(p, raw, 0600)
	if _, err := LoadConfig(p); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, []byte(`{"unexpected":true}`), 0600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("unknown key accepted")
	}
}
