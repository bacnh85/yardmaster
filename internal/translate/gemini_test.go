package translate

import (
	"strings"
	"testing"
)

func TestChatReqToGemini(t *testing.T) {
	chat := map[string]any{
		"model":       "gemini-3-pro",
		"max_tokens":  float64(1024),
		"temperature": 0.7,
		"top_p":       0.9,
		"stop":        []any{"END", "\n\n"},
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,QUJD"}},
			}},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function",
					"function": map[string]any{"name": "read", "arguments": `{"p":"a.md"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "file body"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "read a file",
				"parameters": map[string]any{"type": "object"},
			}},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	}
	out, err := ChatReqToGemini(chat)
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != nil {
		t.Fatalf("gemini body must not carry model: %v", out["model"])
	}
	if si := out["systemInstruction"].(map[string]any); !strings.Contains(jsonStr(si), "be terse") {
		t.Fatalf("bad systemInstruction: %v", si)
	}
	gen := out["generationConfig"].(map[string]any)
	if gen["maxOutputTokens"] != 1024 || gen["temperature"] != 0.7 || gen["topP"] != 0.9 {
		t.Fatalf("bad generationConfig: %v", gen)
	}
	if ss := gen["stopSequences"].([]any); len(ss) != 2 || ss[0] != "END" {
		t.Fatalf("bad stopSequences: %v", ss)
	}
	contents := out["contents"].([]any)
	if len(contents) != 3 { // user, model functionCall, tool functionResponse
		t.Fatalf("want 3 contents, got %d: %v", len(contents), contents)
	}
	u0 := contents[0].(map[string]any)
	if u0["role"] != "user" || !strings.Contains(jsonStr(u0), `"text":"what is this"`) {
		t.Fatalf("bad user content: %v", u0)
	}
	if !strings.Contains(jsonStr(u0), `"inlineData":{"data":"QUJD","mimeType":"image/png"}`) {
		t.Fatalf("bad inlineData: %v", u0)
	}
	m1 := contents[1].(map[string]any)
	if m1["role"] != "model" || !strings.Contains(jsonStr(m1), `"functionCall":{"args":{"p":"a.md"},"name":"read"}`) {
		t.Fatalf("bad functionCall content: %v", m1)
	}
	t2 := contents[2].(map[string]any)
	// tool result must resolve the function name from the assistant's tool_calls
	if t2["role"] != "user" ||
		!strings.Contains(jsonStr(t2), `"functionResponse":{"name":"read","response":{"result":"file body"}}`) {
		t.Fatalf("bad functionResponse content: %v", t2)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	decls := tool["functionDeclarations"].([]any)[0].(map[string]any)
	if decls["name"] != "read" || decls["description"] != "read a file" || decls["parameters"] == nil {
		t.Fatalf("bad functionDeclarations: %v", decls)
	}
	tc := out["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
	if tc["mode"] != "ANY" || tc["allowedFunctionNames"].([]any)[0] != "read" {
		t.Fatalf("bad toolConfig: %v", tc)
	}
}

// Remote https images have no v1beta carrier (inlineData is base64-only,
// fileData must be Google-Cloud-hosted) — dropped without error.
func TestChatReqToGeminiDropsRemoteImages(t *testing.T) {
	out, err := ChatReqToGemini(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "https://example.com/cat.png"}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	parts := out["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 1 || !strings.Contains(jsonStr(parts[0]), `"text":"look"`) {
		t.Fatalf("remote image must be dropped: %v", parts)
	}
}

func TestToolChoiceToGeminiStringModes(t *testing.T) {
	for tc, mode := range map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY"} {
		out, err := ChatReqToGemini(map[string]any{
			"messages": []any{}, "tool_choice": tc,
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		cfg := out["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)
		if cfg["mode"] != mode {
			t.Fatalf("tool_choice %q: want mode %s, got %v", tc, mode, cfg)
		}
	}
	// absent/unknown tool_choice leaves the provider default
	out, _ := ChatReqToGemini(map[string]any{"messages": []any{}, "tool_choice": "weird"})
	if _, ok := out["toolConfig"]; ok {
		t.Fatalf("unknown tool_choice must be dropped: %v", out["toolConfig"])
	}
}

func TestGeminiReqToChat(t *testing.T) {
	gem := mustJSON(t, `{
		"generationConfig": {"maxOutputTokens": 512, "temperature": 0.2, "topP": 0.8,
			"stopSequences": ["END"]},
		"systemInstruction": {"parts": [{"text": "be terse"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "what is this"},
				{"inlineData": {"mimeType": "image/png", "data": "QUJD"}}]},
			{"role": "model", "parts": [{"functionCall": {"name": "read", "args": {"p": "a.md"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "read",
				"response": {"result": "file body"}}}]}
		],
		"tools": [{"functionDeclarations": [{"name": "read", "description": "read a file",
			"parameters": {"type": "object"}}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["read"]}}
	}`)
	out, err := GeminiReqToChat(gem)
	if err != nil {
		t.Fatal(err)
	}
	if out["max_tokens"] != 512 || out["temperature"] != 0.2 || out["top_p"] != 0.8 {
		t.Fatalf("bad generation mapping: %v", out)
	}
	if s := out["stop"].([]any); len(s) != 1 || s[0] != "END" {
		t.Fatalf("bad stop: %v", s)
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 4 { // system, user(text+image), assistant(tool_calls), tool
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("bad system message: %v", msgs[0])
	}
	u1 := msgs[1].(map[string]any)
	if !strings.Contains(jsonStr(u1), "data:image/png;base64,QUJD") {
		t.Fatalf("bad image round trip: %v", u1)
	}
	a2 := msgs[2].(map[string]any)
	tcs := a2["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_0" || tc0["function"].(map[string]any)["name"] != "read" ||
		tc0["function"].(map[string]any)["arguments"] != `{"p":"a.md"}` {
		t.Fatalf("bad tool_calls: %v", tcs)
	}
	t3 := msgs[3].(map[string]any)
	// functionResponse must resolve tool_call_id via the function name
	if t3["role"] != "tool" || t3["tool_call_id"] != "call_0" ||
		t3["content"] != `{"result":"file body"}` {
		t.Fatalf("bad tool message: %v", t3)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	if tool["function"].(map[string]any)["name"] != "read" {
		t.Fatalf("bad tools: %v", tool)
	}
	if out["tool_choice"].(map[string]any)["function"].(map[string]any)["name"] != "read" {
		t.Fatalf("bad tool_choice: %v", out["tool_choice"])
	}
}

func TestToolChoiceFromGeminiModes(t *testing.T) {
	for mode, want := range map[string]any{
		"AUTO": "auto", "NONE": "none", "REQUIRED": "auto", // unknown → default auto
	} {
		out, err := GeminiReqToChat(mustJSON(t, `{
			"contents": [],
			"toolConfig": {"functionCallingConfig": {"mode": "`+mode+`"}}
		}`))
		if err != nil {
			t.Fatal(err)
		}
		if out["tool_choice"] != want {
			t.Fatalf("mode %s: want %v, got %v", mode, want, out["tool_choice"])
		}
	}
	// ANY without names → required
	out, _ := GeminiReqToChat(mustJSON(t, `{
		"contents": [],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY"}}
	}`))
	if out["tool_choice"] != "required" {
		t.Fatalf("ANY without names: got %v", out["tool_choice"])
	}
}

// gemini fixture -> chat -> gemini must preserve the shape (lossless-ish
// round trip mirroring the responses.go convention).
func TestGeminiReqRoundTrip(t *testing.T) {
	gem := mustJSON(t, `{
		"systemInstruction": {"parts": [{"text": "be terse"}]},
		"contents": [
			{"role": "user", "parts": [{"text": "hi"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "ls", "args": {}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "ls", "response": {"result": "a.md"}}}]}
		]
	}`)
	chat, err := GeminiReqToChat(gem)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ChatReqToGemini(chat)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonStr(back), `"text":"be terse"`) ||
		!strings.Contains(jsonStr(back), `"functionCall":{"args":{},"name":"ls"}`) ||
		!strings.Contains(jsonStr(back), `"functionResponse":{"name":"ls","response":{"result":"a.md"}}`) {
		t.Fatalf("round trip lost shape: %s", jsonStr(back))
	}
}

func TestGeminiRespToChat(t *testing.T) {
	gem := mustJSON(t, `{
		"responseId": "gem-1",
		"candidates": [{"content": {"role": "model", "parts": [
				{"text": "let me think", "thought": true},
				{"text": "hello"},
				{"functionCall": {"name": "ls", "args": {"p": "."}}}
			]}, "finishReason": "STOP"}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5,
			"thoughtsTokenCount": 3, "cachedContentTokenCount": 4, "totalTokenCount": 18}
	}`)
	out := GeminiRespToChat(gem, "gemini-3-pro")
	if out["object"] != "chat.completion" || out["model"] != "gemini-3-pro" || out["id"] != "gem-1" {
		t.Fatalf("bad envelope: %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("bad content: %v", msg["content"])
	}
	if msg["reasoning_content"] != "let me think" {
		t.Fatalf("bad reasoning_content: %v", msg["reasoning_content"])
	}
	tcs := msg["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_0" || tc0["function"].(map[string]any)["name"] != "ls" ||
		tc0["function"].(map[string]any)["arguments"] != `{"p":"."}` {
		t.Fatalf("bad tool_calls: %v", tcs)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("want tool_calls finish, got %v", choice["finish_reason"])
	}
	u := out["usage"].(map[string]any)
	// thoughts fold into completion_tokens; cache surfaces both ways
	if u["prompt_tokens"] != 10 || u["completion_tokens"] != 8 || u["total_tokens"] != 18 {
		t.Fatalf("bad usage: %v", u)
	}
	if u["cache_read_tokens"] != 4 || u["prompt_tokens_details"].(map[string]any)["cached_tokens"] != 4 {
		t.Fatalf("bad cache usage: %v", u)
	}
}

func TestGeminiFinishReasonMapping(t *testing.T) {
	for gemF, chatF := range map[string]string{
		"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter",
		"RECITATION": "content_filter", "OTHER": "stop", // default stop
	} {
		gem := mustJSON(t, `{"candidates": [{"finishReason": "`+gemF+`",
			"content": {"role": "model", "parts": [{"text": "x"}]}}]}`)
		out := GeminiRespToChat(gem, "m")
		f := out["choices"].([]any)[0].(map[string]any)["finish_reason"]
		if f != chatF {
			t.Fatalf("finishReason %s: want %s, got %v", gemF, chatF, f)
		}
	}
	// absent finishReason → stop
	out := GeminiRespToChat(mustJSON(t, `{"candidates": [{}]}`), "m")
	if f := out["choices"].([]any)[0].(map[string]any)["finish_reason"]; f != "stop" {
		t.Fatalf("absent finishReason: got %v", f)
	}
}

func TestChatRespToGemini(t *testing.T) {
	chat := mustJSON(t, `{
		"id": "c1", "model": "m",
		"choices": [{"index": 0, "finish_reason": "tool_calls",
			"message": {"role": "assistant", "content": "answer", "reasoning_content": "why",
				"tool_calls": [{"id": "call_7", "type": "function",
					"function": {"name": "ls", "arguments": "{\"p\":\".\"}"}}]}}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5,
			"prompt_tokens_details": {"cached_tokens": 4}}
	}`)
	out := ChatRespToGemini(chat)
	if out["responseId"] != "c1" || out["modelVersion"] != "m" {
		t.Fatalf("bad envelope: %v", out)
	}
	cand := out["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"] != "STOP" { // tool_calls report as STOP on gemini wire
		t.Fatalf("bad finishReason: %v", cand["finishReason"])
	}
	parts := cand["content"].(map[string]any)["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d: %v", len(parts), parts)
	}
	if !strings.Contains(jsonStr(parts[0]), `"thought":true`) ||
		!strings.Contains(jsonStr(parts[0]), `"text":"why"`) {
		t.Fatalf("bad thought part: %v", parts[0])
	}
	if !strings.Contains(jsonStr(parts[1]), `"text":"answer"`) {
		t.Fatalf("bad text part: %v", parts[1])
	}
	if !strings.Contains(jsonStr(parts[2]), `"functionCall":{"args":{"p":"."},"name":"ls"}`) {
		t.Fatalf("bad functionCall part: %v", parts[2])
	}
	u := out["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != 10 || u["candidatesTokenCount"] != 5 ||
		u["totalTokenCount"] != 15 || u["cachedContentTokenCount"] != 4 {
		t.Fatalf("bad usageMetadata: %v", u)
	}
}

// reasoning_content (thought) parts must survive a full response round trip.
func TestGeminiRespRoundTrip(t *testing.T) {
	gem := mustJSON(t, `{
		"responseId": "g1",
		"candidates": [{"finishReason": "MAX_TOKENS", "content": {"role": "model",
			"parts": [{"text": "hmm", "thought": true}, {"text": "hi"}]}}],
		"usageMetadata": {"promptTokenCount": 3, "candidatesTokenCount": 2, "totalTokenCount": 5}
	}`)
	chat := GeminiRespToChat(gem, "m")
	back := ChatRespToGemini(chat)
	cand := back["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"] != "MAX_TOKENS" {
		t.Fatalf("finishReason lost: %v", cand["finishReason"])
	}
	if !strings.Contains(jsonStr(cand), `"thought":true`) ||
		!strings.Contains(jsonStr(cand), `"text":"hmm"`) ||
		!strings.Contains(jsonStr(cand), `"text":"hi"`) {
		t.Fatalf("parts lost: %s", jsonStr(cand))
	}
	u := back["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != 3 || u["candidatesTokenCount"] != 2 {
		t.Fatalf("usage lost: %v", u)
	}
}

func TestGemini2ChatStream(t *testing.T) {
	tr := NewGemini2ChatStream("gemini-3-pro")
	feed := func(s string) []map[string]any { return tr.Chunk(mustJSON(t, s)) }

	// role rides the first content delta; thought → reasoning_content
	first := feed(`{"candidates": [{"content": {"role": "model", "parts": [
		{"text": "th", "thought": true}]}}]}`)
	if len(first) != 1 {
		t.Fatalf("want 1 chunk, got %v", first)
	}
	d0 := first[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if d0["role"] != "assistant" || d0["reasoning_content"] != "th" {
		t.Fatalf("bad first chunk: %v", d0)
	}
	// text delta
	got := feed(`{"candidates": [{"content": {"parts": [{"text": "hi"}]}}]}`)
	if len(got) != 1 || got[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] != "hi" {
		t.Fatalf("bad text chunk: %v", got)
	}
	// functionCall streams whole, with stable ids
	fc := feed(`{"candidates": [{"content": {"parts": [{"functionCall": {"name": "ls", "args": {"p": "."}}}]}}],
		"usageMetadata": {"promptTokenCount": 7, "candidatesTokenCount": 3, "thoughtsTokenCount": 2,
			"cachedContentTokenCount": 2, "totalTokenCount": 12}}`)
	if len(fc) != 1 {
		t.Fatalf("want 1 tool chunk, got %v", fc)
	}
	tcm := fc[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tcm["index"] != 0 || tcm["id"] != "call_0" ||
		tcm["function"].(map[string]any)["name"] != "ls" ||
		tcm["function"].(map[string]any)["arguments"] != `{"p":"."}` {
		t.Fatalf("bad tool chunk: %v", tcm)
	}
	// finishReason rides its own chunk
	fin := feed(`{"candidates": [{"finishReason": "STOP", "content": {"role": "model", "parts": []}}]}`)
	if len(fin) != 1 || fin[0]["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("bad finish chunk: %v", fin)
	}
	// EOF (no [DONE] sentinel on gemini SSE) → Done emits the usage chunk
	done := tr.Done()
	if len(done) != 1 {
		t.Fatalf("want usage chunk, got %v", done)
	}
	u := done[0]["usage"].(map[string]any)
	if u["prompt_tokens"] != 7 || u["completion_tokens"] != 5 || u["cache_read_tokens"] != 2 {
		t.Fatalf("bad usage chunk: %v", done[0]["usage"])
	}
	in, out, cacheR := tr.Usage()
	if in != 7 || out != 5 || cacheR != 2 {
		t.Fatalf("bad Usage(): %d %d %d", in, out, cacheR)
	}
	// Done is idempotent
	if got := tr.Done(); len(got) != 0 {
		t.Fatalf("Done() must be idempotent: %v", got)
	}
}

// MAX_TOKENS maps to length on the finish chunk.
func TestGemini2ChatStreamFinishLength(t *testing.T) {
	tr := NewGemini2ChatStream("m")
	chunks := tr.Chunk(mustJSON(t, `{"candidates": [{"finishReason": "MAX_TOKENS",
		"content": {"role": "model", "parts": [{"text": "x"}]}}]}`))
	if len(chunks) != 2 {
		t.Fatalf("want text + finish chunks, got %v", chunks)
	}
	last := chunks[len(chunks)-1]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "length" {
		t.Fatalf("want length, got %v", last["finish_reason"])
	}
	// the stream end only adds the usage chunk now (zero-filled when the
	// upstream sent no usageMetadata — accounting still gets a usage object)
	done := tr.Done()
	if len(done) != 1 || done[0]["usage"] == nil {
		t.Fatalf("want usage-only Done: %v", done)
	}
}

// upstream that dies without a finishReason still yields a clean stop.
func TestGemini2ChatStreamNoFinish(t *testing.T) {
	tr := NewGemini2ChatStream("m")
	tr.Chunk(mustJSON(t, `{"candidates": [{"content": {"role": "model",
		"parts": [{"text": "hi"}]}}]}`))
	done := tr.Done()
	if len(done) != 2 { // synthetic finish + usage chunk
		t.Fatalf("want 2 chunks, got %v", done)
	}
	if done[0]["choices"].([]any)[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("bad synthetic finish: %v", done[0])
	}
}

func TestChat2GemStream(t *testing.T) {
	tr := NewChat2GemStream("gemini-3-pro")
	feed := func(s string) []map[string]any { return tr.Chunk(mustJSON(t, s)) }

	// reasoning → thought part; content → plain part
	th := feed(`{"choices": [{"index": 0, "delta": {"reasoning_content": "why"}, "finish_reason": null}]}`)
	if len(th) != 1 || !strings.Contains(jsonStr(th[0]), `"thought":true`) {
		t.Fatalf("bad thought chunk: %v", th)
	}
	tx := feed(`{"choices": [{"index": 0, "delta": {"content": "hi"}, "finish_reason": null}]}`)
	if len(tx) != 1 || !strings.Contains(jsonStr(tx[0]), `"text":"hi"`) {
		t.Fatalf("bad text chunk: %v", tx)
	}
	// tool deltas accumulate — no gemini chunk mid-stream (whole parts only)
	if got := feed(`{"choices": [{"index": 0, "delta": {"tool_calls": [
		{"index": 0, "id": "call_1", "type": "function", "function": {"name": "ls", "arguments": ""}}]}}]}`); len(got) != 0 {
		t.Fatalf("tool head must buffer: %v", got)
	}
	if got := feed(`{"choices": [{"index": 0, "delta": {"tool_calls": [
		{"index": 0, "function": {"arguments": "{\"p\":1}"}}]}}]}`); len(got) != 0 {
		t.Fatalf("tool args must buffer: %v", got)
	}
	// finish_reason + usage ride the terminal chat chunk
	if got := feed(`{"choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 4, "completion_tokens": 6,
			"prompt_tokens_details": {"cached_tokens": 2}}}`); len(got) != 0 {
		t.Fatalf("finish chunk must buffer: %v", got)
	}
	final := tr.Finish()
	if len(final) != 1 {
		t.Fatalf("want final chunk, got %v", final)
	}
	f := final[0]
	if !strings.Contains(jsonStr(f), `"finishReason":"STOP"`) { // tool_calls → STOP
		t.Fatalf("bad finishReason: %s", jsonStr(f))
	}
	if !strings.Contains(jsonStr(f), `"functionCall":{"args":{"p":1},"name":"ls"}`) {
		t.Fatalf("assembled functionCall missing: %s", jsonStr(f))
	}
	u := f["usageMetadata"].(map[string]any)
	if u["promptTokenCount"] != 4 || u["candidatesTokenCount"] != 6 ||
		u["cachedContentTokenCount"] != 2 || u["totalTokenCount"] != 10 {
		t.Fatalf("bad usageMetadata: %v", u)
	}
	if f["modelVersion"] != "gemini-3-pro" {
		t.Fatalf("bad modelVersion: %v", f["modelVersion"])
	}
	// Finish is idempotent
	if got := tr.Finish(); len(got) != 0 {
		t.Fatalf("Finish() must be idempotent: %v", got)
	}
}

// length finish maps to MAX_TOKENS on the final gemini chunk.
func TestChat2GemStreamFinishLength(t *testing.T) {
	tr := NewChat2GemStream("m")
	tr.Chunk(mustJSON(t, `{"choices": [{"index": 0, "delta": {"content": "x"},
		"finish_reason": "length"}]}`))
	final := tr.Finish()
	if !strings.Contains(jsonStr(final[0]), `"finishReason":"MAX_TOKENS"`) {
		t.Fatalf("want MAX_TOKENS: %s", jsonStr(final[0]))
	}
}

func TestGeminiErrMessage(t *testing.T) {
	if msg, ok := GeminiErrMessage([]byte(`{"error": {"code": 429, "message": "rate limited",
		"status": "RESOURCE_EXHAUSTED"}}`)); !ok || msg != "rate limited" {
		t.Fatalf("bad message: %q %v", msg, ok)
	}
	if _, ok := GeminiErrMessage([]byte(`garbage`)); ok {
		t.Fatal("garbage body must not parse")
	}
	if _, ok := GeminiErrMessage([]byte(`{"error": {}}`)); ok {
		t.Fatal("empty error must not parse")
	}
}

func TestErrToGemini(t *testing.T) {
	// already gemini-shaped: passthrough verbatim
	gem := []byte(`{"error": {"code": 400, "message": "bad", "status": "INVALID_ARGUMENT"}}`)
	if string(ErrToGemini(gem, 502)) != string(gem) {
		t.Fatalf("gemini body must pass through: %s", ErrToGemini(gem, 502))
	}
	// upstream openai-shaped body whose error object lacks message → rewrapped
	out := ErrToGemini([]byte(`{"error": "nope"}`), 400)
	if !strings.Contains(string(out), `"status":"INVALID_ARGUMENT"`) ||
		!strings.Contains(string(out), "nope") {
		t.Fatalf("bad wrap: %s", out)
	}
	// plain text body
	out = ErrToGemini([]byte("gateway exploded"), 429)
	if !strings.Contains(string(out), `"status":"RESOURCE_EXHAUSTED"`) ||
		!strings.Contains(string(out), "gateway exploded") {
		t.Fatalf("bad plain wrap: %s", out)
	}
	// empty body → synthesized message
	out = ErrToGemini(nil, 503)
	if !strings.Contains(string(out), `"status":"UNAVAILABLE"`) ||
		!strings.Contains(string(out), "upstream error (http 503)") {
		t.Fatalf("bad empty wrap: %s", out)
	}
}
