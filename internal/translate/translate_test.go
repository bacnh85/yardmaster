package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m
}

// zai's GLM-5.x always reasons and its server default is effort=max — a client
// that sends no reasoning_effort (thinking off) must land on adaptive+low.
func TestAdaptiveDefaultEffortLow(t *testing.T) {
	req := mustJSON(t, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	out := OpenAIReqToAnthropic(req, Options{AdaptiveThinking: true})
	if th, ok := out["thinking"].(map[string]any); !ok || th["type"] != "adaptive" {
		t.Fatalf("adaptive thinking expected: %v", out["thinking"])
	}
	if oc, ok := out["output_config"].(map[string]any); !ok || oc["effort"] != "low" {
		t.Fatalf("default effort must be low, got: %v", out["output_config"])
	}
	// without adaptive_thinking, no effort from the client stays effort-less
	out2 := OpenAIReqToAnthropic(req, Options{})
	if _, ok := out2["thinking"]; ok {
		t.Fatalf("no thinking expected without adaptive_thinking: %v", out2["thinking"])
	}
	// explicit effort keeps the mapping
	req3 := mustJSON(t, `{"model":"glm-5.3","reasoning_effort":"max","messages":[{"role":"user","content":"hi"}]}`)
	out3 := OpenAIReqToAnthropic(req3, Options{AdaptiveThinking: true})
	if oc := out3["output_config"].(map[string]any); oc["effort"] != "max" {
		t.Fatalf("explicit max effort: %v", out3["output_config"])
	}
}

// Anthropic requires 1024 ≤ budget_tokens < max_tokens: small max_tokens must
// clamp the budget (never emit <1024) or drop thinking when there is no room.
func TestBudgetTokensFitsMaxTokens(t *testing.T) {
	// default max_tokens 8192: big effort budget capped under it
	out := OpenAIReqToAnthropic(mustJSON(t, `{"model":"m","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`), Options{})
	if b := out["thinking"].(map[string]any)["budget_tokens"]; b != 7168 {
		t.Fatalf("budget capped under 8192: %v", b)
	}
	// small max_tokens: budget clamps to the 1024 floor (1024 < 2048 ✓)
	out2 := OpenAIReqToAnthropic(mustJSON(t, `{"model":"m","reasoning_effort":"high","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`), Options{})
	if b := out2["thinking"].(map[string]any)["budget_tokens"]; b != 1024 {
		t.Fatalf("budget floor: %v", b)
	}
	// max_tokens ≤ 1024: no valid budget exists — thinking dropped, still valid
	out3 := OpenAIReqToAnthropic(mustJSON(t, `{"model":"m","reasoning_effort":"high","max_tokens":512,"messages":[{"role":"user","content":"hi"}]}`), Options{})
	if _, ok := out3["thinking"]; ok {
		t.Fatalf("thinking must be dropped when max_tokens leaves no room: %v", out3["thinking"])
	}
}

// InjectCacheControlAnthropic must mark an anthropic body that lacks markers
// and leave one that already carries them untouched.
func TestInjectCacheControlAnthropic(t *testing.T) {
	req := mustJSON(t, `{"model":"glm-5.3","system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	InjectCacheControlAnthropic(req)
	sys := req["system"].([]any)
	if _, ok := sys[0].(map[string]any)["cache_control"]; !ok {
		t.Fatal("system block not marked")
	}
	last := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Fatal("last user block not marked")
	}
	// existing markers respected
	req2 := mustJSON(t, `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`)
	InjectCacheControlAnthropic(req2)
	if len(req2["system"].([]any)[0].(map[string]any)) != 3 {
		t.Fatal("existing marker replaced")
	}
}

func TestOpenAIReqToAnthropic(t *testing.T) {
	req := mustJSON(t, `{
		"model": "claude-x", "stream": true, "max_tokens": 4096, "temperature": 0.7,
		"reasoning_effort": "high",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": "", "tool_calls": [
				{"id": "call_1", "type": "function",
				 "function": {"name": "read", "arguments": "{\"path\":\"a.go\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "file body"}
		],
		"tools": [{"type": "function", "function": {
			"name": "read", "description": "read a file",
			"parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}],
		"tool_choice": "auto"
	}`)
	out := OpenAIReqToAnthropic(req, Options{InjectCacheControl: true, AdaptiveThinking: true})

	if out["model"] != "claude-x" || out["stream"] != true || out["max_tokens"] != 4096 {
		t.Fatalf("bad scalars: %v", out)
	}
	if out["temperature"] != 0.7 {
		t.Fatalf("temperature lost")
	}
	if th, ok := out["thinking"].(map[string]any); !ok || th["type"] != "adaptive" {
		t.Fatalf("adaptive thinking missing: %v", out["thinking"])
	}
	if oc, ok := out["output_config"].(map[string]any); !ok || oc["effort"] != "high" {
		t.Fatalf("effort missing: %v", out["output_config"])
	}
	if out["system"] != "be terse" {
		t.Fatalf("system: %v", out["system"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (user, assistant+tool_use, user+tool_result), got %d", len(msgs))
	} // assistant message has tool_use
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant blocks: %v", blocks)
	}
	tu := blocks[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "read" {
		t.Fatalf("tool_use: %v", tu)
	}
	if inp := tu["input"].(map[string]any); inp["path"] != "a.go" {
		t.Fatalf("tool input: %v", inp)
	}
	// tool result becomes user message with tool_result
	trMsg := msgs[2].(map[string]any)
	tr := trMsg["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" || tr["content"] != "file body" {
		t.Fatalf("tool_result: %v", tr)
	}
	// tools converted
	tools := out["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["name"] != "read" || t0["input_schema"] == nil {
		t.Fatalf("tools: %v", t0)
	}
	if tc := out["tool_choice"].(map[string]any); tc["type"] != "auto" {
		t.Fatalf("tool_choice: %v", tc)
	}
}

func TestCacheControlInjection(t *testing.T) {
	req := mustJSON(t, `{
		"model": "glm", "max_tokens": 100,
		"messages": [
			{"role": "system", "content": "s"},
			{"role": "user", "content": "u1"},
			{"role": "assistant", "content": "a1"},
			{"role": "user", "content": "u2"}
		]
	}`)
	out := OpenAIReqToAnthropic(req, Options{InjectCacheControl: true})
	sys := out["system"].(string)
	if sys != "s" {
		t.Fatalf("system should stay string: %v", sys)
	}
	msgs := out["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	lb := last["content"].([]any)[0].(map[string]any)
	if lb["cache_control"] == nil {
		t.Fatalf("last user block not marked: %v", lb)
	}
	// system stayed a string (single text) — marker only applies to blocks/tools
}

// Same-wire anthropic passthrough (InjectCacheControlAnthropic): string system
// is normalized to one text block and gets the ephemeral marker; blocks-form
// system keeps the existing mark-last behavior.
func TestInjectCacheControlAnthropicStringSystem(t *testing.T) {
	out := mustJSON(t, `{"system":"be terse","messages":[{"role":"user","content":[{"type":"text","text":"u"}]}]}`)
	InjectCacheControlAnthropic(out)
	sys, ok := out["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("string system not normalized to single block: %v", out["system"])
	}
	sb := sys[0].(map[string]any)
	if sb["text"] != "be terse" || sb["cache_control"] == nil {
		t.Fatalf("system block wrong: %v", sb)
	}
	// blocks-form system unchanged (mark-last-block)
	out2 := mustJSON(t, `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"messages":[{"role":"user","content":[{"type":"text","text":"u"}]}]}`)
	InjectCacheControlAnthropic(out2)
	sys2 := out2["system"].([]any)
	last := sys2[len(sys2)-1].(map[string]any)
	if last["cache_control"] == nil || sys2[0].(map[string]any)["cache_control"] != nil {
		t.Fatalf("blocks-form system marking wrong: %v", sys2)
	}
}

func TestAnthropicReqToOpenAI(t *testing.T) {
	req := mustJSON(t, `{
		"model": "deepseek-flash", "max_tokens": 2048, "stream": false,
		"system": "sys prompt",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "list files"},
				{"type": "tool_result", "tool_use_id": "t1", "content": "a.go\nb.go"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "done"},
				{"type": "tool_use", "id": "t2", "name": "write", "input": {"path": "x"}}
			]}
		],
		"tools": [{"name": "write", "description": "w", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "tool", "name": "write"}
	}`)
	out := AnthropicReqToOpenAI(req, Options{})
	if out["max_tokens"] != 2048 {
		t.Fatalf("max_tokens")
	}
	msgs := out["messages"].([]any)
	// expected: system, tool(t1) [tool results follow assistant directly on openai wire],
	// user "list files", assistant(done+tool_calls)
	if len(msgs) != 4 {
		for i, m := range msgs {
			t.Logf("msg %d: %v", i, m)
		}
		t.Fatalf("want 4 messages, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["content"] != "sys prompt" {
		t.Fatalf("system msg: %v", msgs[0])
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "t1" {
		t.Fatalf("tool msg: %v", tool)
	}
	if msgs[2].(map[string]any)["content"] != "list files" {
		t.Fatalf("user msg: %v", msgs[2])
	}
	asst := msgs[3].(map[string]any)
	if asst["content"] != "done" {
		t.Fatalf("assistant content: %v", asst)
	}
	tcs := asst["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "t2" {
		t.Fatalf("tool_calls: %v", tc0)
	}
	fn := tc0["function"].(map[string]any)
	if fn["name"] != "write" || fn["arguments"] != `{"path":"x"}` {
		t.Fatalf("function: %v", fn)
	}
	tools := out["tools"].([]any)
	tf := tools[0].(map[string]any)["function"].(map[string]any)
	if tf["name"] != "write" || tf["parameters"] == nil {
		t.Fatalf("tools out: %v", tools)
	}
	if tc := out["tool_choice"].(map[string]any); tc["type"] != "function" {
		t.Fatalf("tool_choice out: %v", tc)
	}
}

func TestResponsesNonStream(t *testing.T) {
	am := mustJSON(t, `{
		"id": "m1", "type": "message", "role": "assistant", "model": "glm",
		"content": [
			{"type": "thinking", "thinking": "hmm"},
			{"type": "text", "text": "hello"},
			{"type": "tool_use", "id": "t9", "name": "bash", "input": {"cmd": "ls"}}
		],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 100, "output_tokens": 20,
			"cache_read_input_tokens": 50, "cache_creation_input_tokens": 5}
	}`)
	om := AnthropicRespToOpenAI(am)
	if om["object"] != "chat.completion" {
		t.Fatalf("object: %v", om["object"])
	}
	ch := om["choices"].([]any)[0].(map[string]any)
	msg := ch["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("content: %v", msg["content"])
	}
	if msg["reasoning_content"] != "hmm" {
		t.Fatalf("reasoning: %v", msg["reasoning_content"])
	}
	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish: %v", ch["finish_reason"])
	}
	u := om["usage"].(map[string]any)
	if u["prompt_tokens"] != 155 { // 100 + 50 + 5
		t.Fatalf("prompt_tokens: %v", u["prompt_tokens"])
	}
	d, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok || d["cached_tokens"] != 50 {
		t.Fatalf("prompt_tokens_details: %v", u)
	}
	if u["completion_tokens"] != 20 {
		t.Fatalf("completion_tokens: %v", u["completion_tokens"])
	}

	// reverse
	om2 := mustJSON(t, `{
		"id": "c1", "object": "chat.completion", "model": "m",
		"choices": [{"index": 0, "finish_reason": "length",
			"message": {"role": "assistant", "content": "text", "reasoning_content": "r"}}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3}
	}`)
	am2 := OpenAIRespToAnthropic(om2)
	if am2["stop_reason"] != "max_tokens" {
		t.Fatalf("stop_reason: %v", am2["stop_reason"])
	}
	content := am2["content"].([]any)
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("thinking block first: %v", content)
	}
	if am2["usage"].(map[string]any)["input_tokens"] != 10 {
		t.Fatalf("usage: %v", am2["usage"])
	}
}

func TestErrorEnvelopes(t *testing.T) {
	up := []byte(`{"error":{"message":"rate limited","code":1302}}`)
	o := ErrToOpenAI(up, 429)
	if !strings.Contains(string(o), "rate limited") {
		t.Fatalf("openai passthrough: %s", o)
	}
	a := ErrToAnthropic(up, 429)
	if !strings.Contains(string(a), `"type":"error"`) {
		t.Fatalf("anthropic wrap: %s", a)
	}
	plain := []byte("gateway exploded")
	if !strings.Contains(string(ErrToOpenAI(plain, 502)), "gateway exploded") {
		t.Fatal("plain wrap openai")
	}
	if !strings.Contains(string(ErrToAnthropic(plain, 502)), "gateway exploded") {
		t.Fatal("plain wrap anthropic")
	}
}

// Regression: empty/missing choices must not panic (Azure content-filter shape).
func TestOpenAIRespToAnthropicEmptyChoices(t *testing.T) {
	for _, fixture := range []string{
		`{"id":"x","object":"chat.completion","model":"m","choices":[]}`,
		`{"id":"x","object":"chat.completion","model":"m"}`,
	} {
		am := OpenAIRespToAnthropic(mustJSON(t, fixture))
		if am["type"] != "message" {
			t.Fatalf("bad envelope: %v", am)
		}
		content := am["content"].([]any)
		if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
			t.Fatalf("expected empty text block: %v", content)
		}
	}
}

// A user turn [tool_result, image, text] must emit the tool message FIRST
// (adjacent to the assistant tool_calls turn), then the image, then text.
func TestAnthropicReqToOpenAI_ToolResultImageOrder(t *testing.T) {
	req := mustJSON(t, `{
		"model": "m", "max_tokens": 100,
		"messages": [
			{"role": "user", "content": "screenshot the db"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "shot", "input": {}}]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "t1", "content": "shot ok"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}},
				{"type": "text", "text": "describe it"}
			]}
		]
	}`)
	out := AnthropicReqToOpenAI(req, Options{})
	msgs := out["messages"].([]any)
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = asString(asMap(m)["role"])
	}
	want := []string{"user", "assistant", "tool", "user", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles: %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles: %v, want %v", roles, want)
		}
	}
	// image message carries the image_url block
	img := asMap(msgs[3])["content"].([]any)[0].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("msgs[3] content: %v", img)
	}
	// tool message directly follows the assistant tool_calls turn
	if asMap(msgs[2])["tool_call_id"] != "t1" {
		t.Fatalf("msgs[2]: %v", msgs[2])
	}
}

// translated completions always carry a numeric `created` (openai SDK shape)
func TestAnthropicRespToOpenAI_CreatedNumeric(t *testing.T) {
	am := mustJSON(t, `{"id":"m1","type":"message","role":"assistant","model":"glm",
		"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":1,"output_tokens":1}}`)
	om := AnthropicRespToOpenAI(am)
	b, _ := json.Marshal(om)
	var rt struct {
		Created int64 `json:"created"`
	}
	if json.Unmarshal(b, &rt) != nil || rt.Created <= 0 {
		t.Fatalf("created must be a positive integer, got %s", b)
	}
}
