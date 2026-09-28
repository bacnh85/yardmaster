package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
)

// gemUpstream serves one Gemini alt=sse stream, capturing path+headers+body.
type gemUp struct {
	srv  *httptest.Server
	path string
	hdr  http.Header
	body map[string]any
}

func newGemUp(t *testing.T, stream bool) *gemUp {
	t.Helper()
	u := &gemUp{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.path = r.URL.String()
		u.hdr = r.Header.Clone()
		json.NewDecoder(r.Body).Decode(&u.body)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"gem-hi"}]},"finishReason":"STOP"}],`+
				`"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5,"totalTokenCount":105}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		ev := func(data map[string]any) {
			b, _ := json.Marshal(data)
			fmt.Fprintf(w, "data: %s\n\n", b)
			f.Flush()
		}
		ev(map[string]any{"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "gem-"}}},
		}}})
		ev(map[string]any{"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": "stream"}}},
			"finishReason": "STOP",
		}}, "usageMetadata": map[string]any{"promptTokenCount": 100, "candidatesTokenCount": 5, "totalTokenCount": 105}})
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func gemCfg(url string) *config.Config {
	return &config.Config{Providers: []*config.Provider{
		{Name: "gem", BaseURL: url, Wire: "gemini", Models: []string{"g/gem-pro"},
			ModelMap: map[string]string{"g/gem-pro": "up-id"},
			Auth:     config.AuthConf{Type: "static", Keys: []string{"gk-1"}}}},
	}
}

// readAllSSE drains an SSE body into its data lines.
func readAllSSE(t *testing.T, body interface{ Read([]byte) (int, error) }) []string {
	t.Helper()
	var lines []string
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data: ") {
			lines = append(lines, strings.TrimPrefix(line, "data: "))
		}
	}
	return lines
}

// TestGeminiUpstreamStream: openai client -> gemini upstream, streaming.
// Asserts path variant + alt=sse, x-goog-api-key auth, translated body
// (contents[0].parts[0].text), model_map applied to the path, and the
// gemini SSE translated back to openai chat chunks (text + usage).
func TestGeminiUpstreamStream(t *testing.T) {
	up := newGemUp(t, true)
	p := NewProxy(provider.New(gemCfg(up.srv.URL)), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"g/gem-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if up.path != "/v1beta/models/up-id:streamGenerateContent?alt=sse" {
		t.Fatalf("upstream path: %q", up.path)
	}
	if got := up.hdr.Get("x-goog-api-key"); got != "gk-1" {
		t.Fatalf("x-goog-api-key: %q", got)
	}
	if up.hdr.Get("Authorization") != "" {
		t.Fatalf("unexpected Authorization header: %q", up.hdr.Get("Authorization"))
	}
	if txt := gemText(t, up.body); txt != "hi" {
		t.Fatalf("translated body contents[0].parts[0].text = %q, want %q", txt, "hi")
	}
	if _, hasModel := up.body["model"]; hasModel {
		t.Fatal("gemini body must not carry model (it rides the URL path)")
	}

	lines := readAllSSE(t, resp.Body)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"content":"gem-"`) || !strings.Contains(joined, `"content":"stream"`) {
		t.Fatalf("missing text deltas: %s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish chunk: %s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":100`) || !strings.Contains(joined, `"completion_tokens":5`) {
		t.Fatalf("missing usage chunk: %s", joined)
	}
}

// TestGeminiUpstreamNonStream: same round-trip without streaming.
func TestGeminiUpstreamNonStream(t *testing.T) {
	up := newGemUp(t, false)
	p := NewProxy(provider.New(gemCfg(up.srv.URL)), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeChat))
	defer ts.Close()

	resp, err := http.Post(ts.URL, "application/json",
		strings.NewReader(`{"model":"g/gem-pro","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if up.path != "/v1beta/models/up-id:generateContent" {
		t.Fatalf("upstream path: %q", up.path)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices: %v", choices)
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "gem-hi" {
		t.Fatalf("content: %v", msg["content"])
	}
	u, _ := out["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(100) || u["completion_tokens"] != float64(5) {
		t.Fatalf("usage: %v", u)
	}
}

// gemText digs contents[0].parts[0].text out of a translated gemini request.
func gemText(t *testing.T, body map[string]any) string {
	t.Helper()
	contents, _ := body["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("no contents in translated body")
	}
	parts, _ := contents[0].(map[string]any)["parts"].([]any)
	if len(parts) == 0 {
		t.Fatal("no parts in contents[0]")
	}
	txt, _ := parts[0].(map[string]any)["text"].(string)
	return txt
}

// TestGeminiClientToAnthropicUpstream: the money path — gemini-native client
// POSTs /v1beta, an anthropic upstream serves it. Non-stream + a tool call
// round-trip (gemini functionCall → chat tool_calls → anthropic tool_use →
// back through all three maps to gemini functionCall).
func TestGeminiClientToAnthropicUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		// the gemini client's user turn must arrive as an anthropic message
		msgs, _ := req["messages"].([]any)
		if len(msgs) == 0 {
			t.Error("no messages translated to anthropic")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",`+
			`"content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":3}}`)
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/claude-x:generateContent", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"weather in SF?"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	cands, _ := out["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("candidates: %v", cands)
	}
	parts, _ := cands[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("parts: %v", parts)
	}
	fc, _ := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Fatalf("functionCall name: %v", fc["name"])
	}
	args, _ := fc["args"].(map[string]any)
	if args["city"] != "SF" {
		t.Fatalf("functionCall args: %v", args)
	}
	u, _ := out["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != float64(7) || u["candidatesTokenCount"] != float64(3) {
		t.Fatalf("usageMetadata: %v", u)
	}
}

// TestGeminiClientToolRoundTripStream: gemini client, streaming tool call —
// the chat stream's tool_calls deltas must land as a whole functionCall part
// in the final gemini chunk (Chat2GemStream flushes accumulated tools on
// Finish).
func TestGeminiClientToolRoundTripStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, `event: message_start`+"\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":4}}}`+"\n\n")
		fmt.Fprint(w, `event: content_block_start`+"\n"+
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"ping"}}`+"\n\n")
		fmt.Fprint(w, `event: content_block_delta`+"\n"+
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`+"\n\n")
		fmt.Fprint(w, `event: message_delta`+"\n"+
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`+"\n\n")
		fmt.Fprint(w, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`+"\n\n")
		f.Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/claude-x:streamGenerateContent?alt=sse", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"call ping"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := readAllSSE(t, resp.Body)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"functionCall"`) || !strings.Contains(joined, `"name":"ping"`) ||
		!strings.Contains(joined, `"args":{"x":1}`) {
		t.Fatalf("whole functionCall part missing: %s", joined)
	}
	if !strings.Contains(joined, `"finishReason":"STOP"`) {
		t.Fatalf("missing finishReason: %s", joined)
	}
	if !strings.Contains(joined, `"promptTokenCount":4`) || !strings.Contains(joined, `"candidatesTokenCount":2`) {
		t.Fatalf("missing usageMetadata: %s", joined)
	}
}

// TestGeminiClientToGeminiUpstream: same-wire serving is passthrough — the
// gemini body carries no model, so the re-patch is a no-op apart from JSON
// re-encoding (same convention as responses↔responses; response SSE bytes
// ride untouched), and the upstream's gemini SSE bytes come back verbatim.
func TestGeminiClientToGeminiUpstream(t *testing.T) {
	const body = `{"contents":[{"role":"user","parts":[{"text":"same-wire"}]}],"generationConfig":{"temperature":0.5}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gem-pro:streamGenerateContent" || r.URL.RawQuery != "alt=sse" {
			t.Errorf("upstream URL: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		var got, want map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		json.Unmarshal([]byte(body), &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("passthrough body mutated: %v", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"raw\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":1,\"totalTokenCount\":10}}\n\n")
		f.Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "gem", BaseURL: up.URL, Wire: "gemini", Models: []string{"gem-pro"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"gk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/gem-pro:streamGenerateContent?alt=sse", "application/json",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := readAllSSE(t, resp.Body)
	if len(lines) != 1 || !strings.Contains(lines[0], `"text":"raw"`) ||
		!strings.Contains(lines[0], `"promptTokenCount":9`) {
		t.Fatalf("passthrough stream: %v", lines)
	}
}

// TestGeminiClientToOpenAIUpstream: gemini client, openai hub upstream — the
// hub chunk stream comes back as gemini alt=sse data (Chat2GemStream), text
// and usage intact.
func TestGeminiClientToOpenAIUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("upstream path: %s", r.URL.Path)
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "ds-flash" {
			t.Errorf("model: %v", req["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		c1, _ := json.Marshal(oaiChunk(map[string]any{"role": "assistant", "content": "hub"}, nil))
		cu, _ := json.Marshal(map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{}, "usage": map[string]any{"prompt_tokens": 6, "completion_tokens": 1}})
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", c1, cu)
		f.Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "ds", BaseURL: up.URL, Wire: "openai", Models: []string{"gem-pro"},
			ModelMap: map[string]string{"gem-pro": "ds-flash"},
			Auth:     config.AuthConf{Type: "static", Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/gem-pro:streamGenerateContent?alt=sse", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := readAllSSE(t, resp.Body)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"text":"hub"`) {
		t.Fatalf("missing text part: %s", joined)
	}
	if !strings.Contains(joined, `"promptTokenCount":6`) || !strings.Contains(joined, `"candidatesTokenCount":1`) {
		t.Fatalf("missing usageMetadata: %s", joined)
	}
}

// TestGeminiUpstreamErrorDialect: upstream >=400 surfaces as
// {error:{code,message,status}} for a gemini client (ErrToGemini path).
func TestGeminiUpstreamErrorDialect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "zai", BaseURL: up.URL, Wire: "anthropic", Models: []string{"claude-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"zk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/claude-x:generateContent", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	e, _ := out["error"].(map[string]any)
	if e == nil {
		t.Fatalf("no error envelope: %v", out)
	}
	if e["code"] != float64(429) || e["status"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("error envelope: %v", e)
	}
	if msg, _ := e["message"].(string); !strings.Contains(msg, "rate limited") {
		t.Fatalf("error message: %v", e["message"])
	}
}

// TestGeminiUnknownMethod404: only the two generateContent variants are
// accepted — a :countTokens body must 404 instead of being dispatched as a
// chat request.
func TestGeminiUnknownMethod404(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no dispatch expected for an unknown gemini method")
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "gem", BaseURL: up.URL, Wire: "gemini", Models: []string{"gem-pro"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"gk"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	for _, path := range []string{
		"/v1beta/models/gem-pro:countTokens",
		"/v1beta/models/gem-pro:embedContent",
	} {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(`{"contents":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestGeminiClientResponsesUpstreamStream: gemini client -> responses-wire
// upstream, streaming. Guards branch order in forwardTranslateStream — the
// gemini-client branch must run before the responses/gemini upstream
// passthrough or the client receives raw openai chat chunks.
func TestGeminiClientResponsesUpstreamStream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n")
		f.Flush()
	}))
	defer up.Close()

	cfg := &config.Config{Providers: []*config.Provider{
		{Name: "codex", BaseURL: up.URL, Wire: "responses", Models: []string{"gpt-x"},
			Auth: config.AuthConf{Type: "static", Keys: []string{"k"}}}}}
	p := NewProxy(provider.New(cfg), nil, nil)
	ts := httptest.NewServer(http.HandlerFunc(p.ServeGemini))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1beta/models/gpt-x:streamGenerateContent?alt=sse", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := readAllSSE(t, resp.Body)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, `"text":"hi"`) {
		t.Fatalf("missing text part: %s", joined)
	}
	if !strings.Contains(joined, `"finishReason"`) || !strings.Contains(joined, `"usageMetadata"`) ||
		!strings.Contains(joined, `"promptTokenCount":5`) {
		t.Fatalf("missing finishReason/usageMetadata: %s", joined)
	}
	// gemini SSE has no DONE sentinel; chat leakage shows up as choices/[DONE]
	if strings.Contains(joined, `"choices"`) || strings.Contains(joined, `[DONE]`) {
		t.Fatalf("openai chat chunk leaked to gemini client: %s", joined)
	}
}
