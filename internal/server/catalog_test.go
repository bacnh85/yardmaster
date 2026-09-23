package server

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
)

// flexString/tokToMtok: OpenRouter's per-token pricing strings ("0.0000007",
// "-1" = BYO-key unmanaged) must decode without failing whole-payload JSON.
func TestTokToMtok(t *testing.T) {
	cases := []struct {
		in   flexString
		want float64
	}{
		{"0.0000007", 0.7},      // $/token → $/Mtok
		{"0", 0},                // free
		{"-1", -1},              // BYO-key → unknown-price convention
		{flexString("null"), 0}, // absent field decodes as literal null string → 0
		{"", 0},                 // empty → 0
	}
	for _, c := range cases {
		if got := tokToMtok(c.in); got != c.want {
			t.Errorf("tokToMtok(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// modelsDevProviderID maps a provider base_url to its models.dev entry —
// ollama.com hosts the "ollama-cloud" entry with full catalog metadata.
func TestModelsDevProviderID(t *testing.T) {
	cases := map[string]string{
		"https://ollama.com":            "ollama-cloud",
		"https://ollama.com/v1":         "ollama-cloud",
		"https://api.commandcode.ai":    "", // CommandCode carries its own metadata
		"https://api.deepseek.com":      "deepseek",
		"https://opencode.ai/zen/go/v1": "opencode-go",
		"https://api.example.com/v1":    "",
	}
	for in, want := range cases {
		if got := modelsDevProviderID(in); got != want {
			t.Errorf("modelsDevProviderID(%q) = %q, want %q", in, got, want)
		}
	}
}

// OpenRouter-style /models payload: live pricing strings, max_completion_tokens,
// modality/parameter capability flags, :free models, BYO-key "-1" pricing.
// A models.dev-provider payload (CommandCode shape: only id/name/context/
// supported_endpoints) must keep parsing exactly as before.
func TestUpstreamModelsOpenRouter(t *testing.T) {
	body := `{"data":[
		{"id":"anthropic/claude-sonnet-5","name":"Claude: Sonnet 5","context_length":1000000,
		 "pricing":{"prompt":"0.000002","completion":"0.00001","input_cache_read":"0.0000002"},
		 "top_provider":{"max_completion_tokens":128000},
		 "architecture":{"input_modalities":["text","image"]},
		 "supported_parameters":["reasoning","tool_choice","temperature"]},
		{"id":"inclusionai/ling-3.0-flash-sante:free","name":"Ling free","context_length":32768,
		 "pricing":{"prompt":"0","completion":"0"},"top_provider":{"max_completion_tokens":8192},
		 "architecture":{"input_modalities":["text"]},"supported_parameters":[]},
		{"id":"openrouter/auto","name":"Auto (BYOK)","context_length":2000000,
		 "pricing":{"prompt":"-1","completion":"-1"},
		 "top_provider":{"max_completion_tokens":null},
		 "architecture":{"input_modalities":["text"]},"supported_parameters":null},
		{"id":"cmd/plain","name":"Plain (no pricing fields)","context_length":8192}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	s := &Server{}
	got := s.upstreamModels(context.Background(), &config.Provider{
		BaseURL: srv.URL,
		Auth:    config.AuthConf{Keys: []string{"k"}},
	})
	if len(got) != 4 {
		t.Fatalf("got %d models, want 4", len(got))
	}
	c := got[0]
	if c.Input != 2 || c.Output != 10 || math.Abs(c.CacheRead-0.2) > 1e-9 {
		t.Errorf("claude pricing = %v/%v/%v, want 2/10/0.2", c.Input, c.Output, c.CacheRead)
	}
	if c.MaxOutput != 128000 || !c.Image || !c.Reasoning || !c.ToolCall {
		t.Errorf("claude meta wrong: maxout=%d image=%v reason=%v tool=%v", c.MaxOutput, c.Image, c.Reasoning, c.ToolCall)
	}
	if c.Family != "" {
		t.Errorf("openai-wire payload must not set family, got %q", c.Family)
	}
	f := got[1]
	if !f.Free || f.Input != 0 || f.Output != 0 {
		t.Errorf(":free model = free=%v in=%v out=%v, want true/0/0", f.Free, f.Input, f.Output)
	}
	b := got[2]
	if b.Input != -1 || b.Output != -1 || b.Free {
		t.Errorf("BYOK -1 pricing = in=%v out=%v free=%v, want -1/-1/false", b.Input, b.Output, b.Free)
	}
	if b.MaxOutput != 0 {
		t.Errorf("null max_completion_tokens must decode as 0, got %d", b.MaxOutput)
	}
	p := got[3]
	if p.Input != 0 || p.Output != 0 || p.Free {
		t.Errorf("absent pricing fields = in=%v out=%v free=%v, want 0/0/false (upstream leaves flags unset)", p.Input, p.Output, p.Free)
	}
}
