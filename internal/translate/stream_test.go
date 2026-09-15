package translate

import (
	"encoding/json"
	"testing"
)

func evJSON(t *testing.T, e Event) string {
	t.Helper()
	b, _ := json.Marshal(e.Data)
	return e.Name + " " + string(b)
}

func TestOAI2Anth_TextAndUsage(t *testing.T) {
	tr := NewOAI2AnthStream("glm")
	var events []string
	for _, line := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"he"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":null}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":"stop"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":34}}`,
	} {
		for _, e := range tr.Chunk(mustJSON(t, line)) {
			events = append(events, evJSON(t, e))
		}
	}
	for _, e := range tr.Finish() {
		events = append(events, evJSON(t, e))
	}
	joined := ""
	for _, e := range events {
		joined += e + "\n"
	}
	for _, want := range []string{
		"message_start",
		`content_block_start`, `"type":"text"`,
		`"text":"he"`, `"text":"llo"`, `"text":"world"`, `"text_delta"`,
		`content_block_stop`,
		`message_delta`, `"stop_reason":"end_turn"`, `"output_tokens":34`,
		"message_stop",
	} {
		if !contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	// exactly one text block opened
	if n := count(joined, `"content_block_start"`); n != 1 {
		t.Fatalf("text blocks: %d\n%s", n, joined)
	}
}

func TestOAI2Anth_ThinkingAndTools(t *testing.T) {
	tr := NewOAI2AnthStream("m")
	var out []Event
	for _, line := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"thinking..."}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"c"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ommand\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		out = append(out, tr.Chunk(mustJSON(t, line))...)
	}
	out = append(out, tr.Finish()...)

	joined := ""
	for _, e := range out {
		joined += evJSON(t, e) + "\n"
	}
	if !contains(joined, `"thinking":"thinking..."`) || !contains(joined, `"thinking_delta"`) {
		t.Fatalf("thinking delta missing:\n%s", joined)
	}
	if !contains(joined, `"name":"bash"`) || !contains(joined, `"id":"t1"`) {
		t.Fatalf("tool_use start missing:\n%s", joined)
	}
	if !contains(joined, `"partial_json":"{\"c"`) {
		t.Fatalf("partial json missing:\n%s", joined)
	}
	if !contains(joined, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop reason:\n%s", joined)
	}
	// block indices: thinking=0, text=1, tool=2
	if !contains(joined, `"index":2,"type":"content_block_start"`) {
		t.Fatalf("tool block index:\n%s", joined)
	}
}

func TestAnth2OAI_Full(t *testing.T) {
	tr := NewAnth2OAIStream("glm")
	var chunks []map[string]any
	feed := func(name, data string) {
		chunks = append(chunks, tr.Event(name, mustJSON(t, data))...)
	}
	feed("message_start", `{"type":"message_start","message":{"id":"m1","role":"assistant",
		"usage":{"input_tokens":100,"cache_read_input_tokens":40,"cache_creation_input_tokens":2}}}`)
	feed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	feed("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`)
	feed("content_block_stop", `{"type":"content_block_stop","index":0}`)
	feed("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tt","name":"run","input":{}}}`)
	feed("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`)
	feed("content_block_stop", `{"type":"content_block_stop","index":1}`)
	feed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`)
	chunks = append(chunks, tr.Event("message_stop", mustJSON(t, `{"type":"message_stop"}`))...)

	// first chunk: role
	if chunks[0]["choices"] == nil {
		t.Fatalf("no role chunk: %v", chunks[0])
	}
	// text delta present
	if !hasDeltaContent(chunks, "hi") {
		t.Fatalf("text missing")
	}
	// tool call start + args
	if !hasToolCall(chunks, "tt", "run", `{"x":1}`) {
		t.Fatalf("tool calls missing")
	}
	// finish chunk
	var finish string
	for _, c := range chunks {
		for _, chv := range toSlice(c["choices"]) {
			if fr := asString(asMap(chv)["finish_reason"]); fr != "" {
				finish = fr
			}
		}
	}
	if finish != "tool_calls" {
		t.Fatalf("finish: %s", finish)
	}
	// final usage chunk
	last := chunks[len(chunks)-1]
	u := last["usage"].(map[string]any)
	if u["prompt_tokens"] != 142 || u["completion_tokens"] != 9 { // 100+40+2
		t.Fatalf("usage chunk: %v", u)
	}
	// internal usage tracker
	in, out, cr, cw := tr.Usage()
	if in != 100 || out != 9 || cr != 40 || cw != 2 {
		t.Fatalf("tracker: %d %d %d %d", in, out, cr, cw)
	}
}

func TestAnth2OAI_ThinkingPassthrough(t *testing.T) {
	tr := NewAnth2OAIStream("m")
	var chunks []map[string]any
	chunks = append(chunks, tr.Event("message_start", mustJSON(t,
		`{"type":"message_start","message":{"id":"x","usage":{"input_tokens":1}}}`))...)
	chunks = append(chunks, tr.Event("content_block_delta", mustJSON(t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"why"}}`))...)
	found := false
	for _, c := range chunks {
		for _, chv := range toSlice(c["choices"]) {
			d := asMap(asMap(chv)["delta"])
			if d != nil && d["reasoning_content"] == "why" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("reasoning_content not mapped")
	}
}

// ---- helpers ----

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func count(s, sub string) int {
	n := 0
	for {
		i := indexOf(s, sub)
		if i < 0 {
			return n
		}
		n++
		s = s[i+len(sub):]
	}
}
func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func hasDeltaContent(chunks []map[string]any, text string) bool {
	for _, c := range chunks {
		for _, chv := range toSlice(c["choices"]) {
			d := asMap(asMap(chv)["delta"])
			if d != nil && d["content"] == text {
				return true
			}
		}
	}
	return false
}

func hasToolCall(chunks []map[string]any, id, name, args string) bool {
	sawID, sawArgs := false, false
	for _, c := range chunks {
		for _, chv := range toSlice(c["choices"]) {
			d := asMap(asMap(chv)["delta"])
			if d == nil {
				continue
			}
			for _, tcv := range toSlice(d["tool_calls"]) {
				tc := asMap(tcv)
				if tc["id"] == id {
					sawID = true
				}
				fn := asMap(tc["function"])
				if fn != nil && fn["arguments"] == args {
					sawArgs = true
				}
			}
		}
	}
	return sawID && sawArgs
}
