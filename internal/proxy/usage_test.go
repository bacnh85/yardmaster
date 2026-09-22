package proxy

import (
	"encoding/json"
	"testing"
)

// feed SSE lines into a tee and return the parsed usage
func teeUsage(t *testing.T, wire string, lines ...string) Usage {
	t.Helper()
	tee := NewUsageTee(wire)
	for _, l := range lines {
		tee.Write([]byte(l + "\n"))
	}
	return tee.usage
}

func TestUsageOpenAICacheShapes(t *testing.T) {
	cases := []struct {
		name           string
		usage          map[string]any
		wantIn, wantCR int
	}{
		{"openai+deepseek nested", map[string]any{
			"prompt_tokens": 100, "completion_tokens": 7,
			"prompt_tokens_details": map[string]any{"cached_tokens": 40}}, 60, 40},
		{"deepseek legacy flat", map[string]any{
			"prompt_tokens": 100, "completion_tokens": 7,
			"prompt_cache_hit_tokens": 40, "prompt_cache_miss_tokens": 60}, 60, 40},
		{"house/translate top-level", map[string]any{
			"prompt_tokens": 100, "completion_tokens": 7, "cache_read_tokens": 40}, 60, 40},
		{"no cache fields", map[string]any{
			"prompt_tokens": 100, "completion_tokens": 7}, 100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := `{"id":"c","object":"chat.completion.chunk","model":"m","choices":[],"usage":` + jsonMust(tc.usage) + `}`
			u := teeUsage(t, "openai", "data: "+b, "data: [DONE]")
			if u.In != tc.wantIn || u.CacheR != tc.wantCR || u.Out != 7 {
				t.Fatalf("got in=%d cacheR=%d out=%d, want in=%d cacheR=%d out=7", u.In, u.CacheR, u.Out, tc.wantIn, tc.wantCR)
			}
		})
	}
}

func TestUsageResponsesCacheExclusive(t *testing.T) {
	u := teeUsage(t, "responses", `event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":4,"input_tokens_details":{"cached_tokens":40}}}}`)
	if u.In != 60 || u.CacheR != 40 || u.Out != 4 {
		t.Fatalf("got in=%d cacheR=%d out=%d, want 60/40/4", u.In, u.CacheR, u.Out)
	}
}

func TestUsageAnthropicUnchanged(t *testing.T) {
	// anthropic input_tokens already excludes cache reads — no subtraction
	u := teeUsage(t, "anthropic",
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"cache_read_input_tokens":30}}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":4}}`)
	if u.In != 50 || u.CacheR != 30 || u.Out != 4 {
		t.Fatalf("got in=%d cacheR=%d out=%d, want 50/30/4", u.In, u.CacheR, u.Out)
	}
}

func TestParseUsageJSONOpenAI(t *testing.T) {
	body := `{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}`
	u := ParseUsageJSON("openai", []byte(body))
	if u.In != 60 || u.CacheR != 40 || u.Out != 7 {
		t.Fatalf("got in=%d cacheR=%d out=%d, want 60/40/7", u.In, u.CacheR, u.Out)
	}
}

func jsonMust(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A misreporting upstream (cached > prompt) must not leave tok_in
// cache-INCLUSIVE — the store invariant is tok_in excludes cache_read.
func TestUsageOpenAIMisreportedCachedClamped(t *testing.T) {
	u := ParseUsageJSON("openai", []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":300}}}`))
	if u.In != 0 || u.CacheR != 300 {
		t.Fatalf("got in=%d cacheR=%d, want 0/300 (clamped, not 100 inclusive)", u.In, u.CacheR)
	}
	u2 := teeUsage(t, "responses",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":50,"output_tokens":4,"input_tokens_details":{"cached_tokens":80}}}}`)
	if u2.In != 0 || u2.CacheR != 80 {
		t.Fatalf("got in=%d cacheR=%d, want 0/80 (clamped, not 50 inclusive)", u2.In, u2.CacheR)
	}
}

// Z.ai shape: cache fields arrive only in message_delta, never message_start.
func TestUsageAnthropicZaiStreamCacheInDelta(t *testing.T) {
	u := teeUsage(t, "anthropic",
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"input_tokens":27,"output_tokens":8,"cache_read_input_tokens":2688}}`)
	if u.In != 27 || u.CacheR != 2688 || u.Out != 8 {
		t.Fatalf("got in=%d cacheR=%d out=%d, want 27/2688/8", u.In, u.CacheR, u.Out)
	}
}
