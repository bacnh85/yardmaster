package config

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skip_when_exhausted round-trips from YAML and defaults to false (unset
// fields keep the current route-everything behavior). Validation-free by
// design: a bool has no invalid state.
func TestSkipWhenExhaustedConfig(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
providers:
  - name: a
    base_url: https://a.example/v1
    wire: openai
    skip_when_exhausted: true
    auth: {type: static, keys: [k]}
  - name: b
    base_url: https://b.example/v1
    wire: openai
    auth: {type: static, keys: [k]}
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers[0].SkipWhenExhausted {
		t.Fatal("provider a: skip_when_exhausted not parsed from yaml")
	}
	if cfg.Providers[1].SkipWhenExhausted {
		t.Fatal("provider b: skip_when_exhausted must default to false")
	}
}

// proxy_url: per-provider outbound proxy for upstream dispatch. Empty =
// direct; otherwise it must parse and carry an http/https/socks5 scheme —
// the set http.Transport dials natively. Rejected at load so a typo never
// surfaces as a mid-dispatch transport error.
func TestProxyURLConfig(t *testing.T) {
	var cfg Config
	err := yaml.Unmarshal([]byte(`
providers:
  - name: a
    base_url: https://a.example/v1
    proxy_url: socks5://127.0.0.1:1080
    wire: openai
    auth: {type: static, keys: [k]}
  - name: b
    base_url: https://b.example/v1
    proxy_url: http://proxy.example:8080
    wire: openai
    auth: {type: static, keys: [k]}
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatal("proxy_url not parsed from yaml")
	}
	for _, bad := range []string{
		"ftp://proxy.example:1080", // parses, unsupported scheme
		"127.0.0.1:1080",           // no scheme (not even parseable as URL)
		"file:///etc/hosts",        // scheme outside the dial set
	} {
		var c Config
		err := yaml.Unmarshal([]byte(fmt.Sprintf(`
providers:
  - name: a
    base_url: https://a.example/v1
    proxy_url: %q
    wire: openai
    auth: {type: static, keys: [k]}
`, bad)), &c)
		if err != nil {
			t.Fatal(err)
		}
		c.Defaults()
		err = c.Validate()
		if err == nil || !strings.Contains(err.Error(), "proxy_url") {
			t.Errorf("proxy_url %q: want rejection naming proxy_url, got %v", bad, err)
		}
	}
}
