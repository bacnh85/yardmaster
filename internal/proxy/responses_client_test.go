package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/store"
)

// Responses-wire client (POST /v1/responses) against each upstream wire,
// streaming and non-streaming. Asserts native-wire output reaches the client
// and usage lands in the store.

func respUpstream(t *testing.T, wire, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch wire {
		case "openai":
			if r.URL.Path != "/chat/completions" {
				t.Errorf("openai upstream path: %s", r.URL.Path)
			}
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["model"] != "deepseek-flash" || req["reasoning_effort"] != "high" {
				t.Errorf("normalized chat req: model=%v effort=%v", req["model"], req["reasoning_effort"])
			}
			if mode == "stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				for _, c := range []string{
					`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
					`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"hola"},"finish_reason":null}]}`,
					`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`,
				} {
					fmt.Fprintf(w, "data: %s\n\n", c)
					f.Flush()
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			} else {
				json.NewEncoder(w).Encode(map[string]any{
					"id": "cmpl-1", "object": "chat.completion", "model": "m",
					"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
						"message": map[string]any{"role": "assistant", "content": "hola-full"}}},
					"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 2},
				})
			}
		case "anthropic":
			if r.URL.Path != "/v1/messages" {
				t.Errorf("anthropic upstream path: %s", r.URL.Path)
			}
			if mode == "stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				ev := func(name, data string) {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
					f.Flush()
				}
				ev("message_start", `{"type":"message_start","message":{"id":"am","role":"assistant","usage":{"input_tokens":50,"cache_read_input_tokens":30}}}`)
				ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hola"}}`)
				ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`)
				ev("message_stop", `{"type":"message_stop"}`)
			} else {
				json.NewEncoder(w).Encode(map[string]any{
					"id": "msg_1", "type": "message", "role": "assistant", "model": "m",
					"content": []any{map[string]any{"type": "text", "text": "hola-full"}},
					"usage":   map[string]any{"input_tokens": 50, "output_tokens": 4},
				})
			}
		case "responses":
			if r.URL.Path != "/responses" {
				t.Errorf("responses upstream path: %s", r.URL.Path)
			}
			// byte-exact passthrough: the body must be a native Responses request
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if _, ok := req["input"]; !ok {
				t.Errorf("responses upstream got non-native body: %v", req)
			}
			if mode == "stream" {
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				ev := func(name, data string) {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data)
					f.Flush()
				}
				ev("response.created", `{"type":"response.created","response":{"id":"r1","object":"response","status":"in_progress"}}`)
				ev("response.output_text.delta", `{"type":"response.output_text.delta","delta":"hola"}`)
				ev("response.completed", `{"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":2}}}`)
			} else {
				json.NewEncoder(w).Encode(map[string]any{
					"id": "r1", "object": "response", "model": "m", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "hola-full"}}}},
					"usage": map[string]any{"input_tokens": 11, "output_tokens": 2},
				})
			}
		}
	}))
}

func TestResponsesClientAllWires(t *testing.T) {
	cases := []struct {
		wire     string
		stream   bool
		wantSub  string // substring the client must see
		wantIn   int
		wantOut  int
	}{
		{"openai", true, `output_text.delta`, 11, 2},
		{"openai", false, "hola-full", 11, 2},
		{"anthropic", true, `output_text.delta`, 80, 4},
		{"anthropic", false, "hola-full", 50, 4},
		{"responses", true, `output_text.delta`, 11, 2},
		{"responses", false, "hola-full", 11, 2},
	}
	for _, tc := range cases {
		t.Run(tc.wire+"-"+mode(tc.stream), func(t *testing.T) {
			up := respUpstream(t, tc.wire, mode(tc.stream))
			defer up.Close()
			cfg := &config.Config{Providers: []*config.Provider{
				{Name: "p-" + tc.wire, BaseURL: up.URL, Wire: tc.wire,
					Models: []string{"ocg/gpt-x"}, ModelMap: map[string]string{"ocg/gpt-x": "deepseek-flash"},
					Auth: config.AuthConf{Keys: []string{"k"}}}}}
			p := NewProxy(provider.New(cfg), mustStore(t), nil)
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/responses", p.ServeResponses)
			ts := httptest.NewServer(mux)
			defer ts.Close()

			body := `{"model":"ocg/gpt-x","stream":` + fmt.Sprint(tc.stream) +
				`,,"reasoning":{"effort":"high"},"input":"hi"}`
			body = strings.Replace(body, ",,", ",", 1)
			resp, err := http.Post(ts.URL+"/v1/responses", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)

			if tc.stream {
				if !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
					t.Fatalf("content-type: %s", resp.Header.Get("Content-Type"))
				}
				joined := string(raw)
				for _, want := range []string{tc.wantSub, `"response.completed"`} {
					if !strings.Contains(joined, want) {
						t.Fatalf("missing %q in %s", want, joined)
					}
				}
			} else {
				var m map[string]any
				if json.Unmarshal(raw, &m) != nil || m["object"] != "response" {
					t.Fatalf("non-stream body: %s", raw)
				}
				if !strings.Contains(string(raw), tc.wantSub) {
					t.Fatalf("missing %q in %s", tc.wantSub, raw)
				}
			}
			// store submit is async — poll for the record
			var rec []store.Row
			for deadline := time.Now().Add(5 * time.Second); ; {
				rows, _ := p.Store.Recent(10)
				if len(rows) > 0 {
					rec = rows
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no usage record")
				}
				time.Sleep(5 * time.Millisecond)
			}
			r := rec[0]
			if r.TokIn != tc.wantIn || r.TokOut != tc.wantOut {
				t.Fatalf("usage: got in=%d out=%d, want %d/%d", r.TokIn, r.TokOut, tc.wantIn, tc.wantOut)
			}
		})
	}
}

func mode(stream bool) string {
	if stream {
		return "stream"
	}
	return "full"
}

func mustStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
