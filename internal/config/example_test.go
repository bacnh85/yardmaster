package config

import "testing"

// The shipped example is the copy-paste starting point: it must always load
// and validate (a route referencing a commented-out provider would 500 boot).
func TestExampleConfigLoadsAndValidates(t *testing.T) {
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
	cfg.Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("example config does not validate: %v", err)
	}
}
