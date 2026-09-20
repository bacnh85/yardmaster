package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// evm round-trips through JSON so the shapes match what the proxy sees.
func evm(t *testing.T, v any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestResponsesReqToChat(t *testing.T) {
	req := map[string]any{
		"model":             "gpt-5.5",
		"max_output_tokens": 1024.0,
		"temperature":       0.7,
		"stream":            true,
		"store":             false,
		"previous_response_id": "resp_old",
		"instructions":      "be terse",
		"reasoning":         map[string]any{"effort": "high"},
		"input": []any{
			evm(t, map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": "hi"}}}),
			evm(t, map[string]any{"type": "function_call", "call_id": "call_1",
				"name": "read", "arguments": `{"p":"a.md"}`}),
			evm(t, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "file body"}),
		},
		"tools": []any{
			evm(t, map[string]any{"type": "function", "name": "read",
				"description": "read a file", "parameters": map[string]any{"type": "object"}}),
		},
		"tool_choice": evm(t, map[string]any{"type": "function", "name": "read"}),
	}
	out := ResponsesReqToChat(req)
	if out["model"] != "gpt-5.5" || asInt(out["max_tokens"]) != 1024 || out["stream"] != true {
		t.Fatalf("bad scalars: %v", out)
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort lost: %v", out)
	}
	if _, has := out["store"]; has {
		t.Fatal("store must be dropped")
	}
	if _, has := out["previous_response_id"]; has {
		t.Fatal("previous_response_id must be dropped (stateless router)")
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages (system+3 items), got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" ||
		msgs[0].(map[string]any)["content"] != "be terse" {
		t.Fatalf("instructions not mapped to system: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "hi" {
		t.Fatalf("user text lost: %v", msgs[1])
	}
	tc := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "call_1" || asMap(tc["function"])["name"] != "read" {
		t.Fatalf("function_call not mapped: %v", tc)
	}
	if msgs[3].(map[string]any)["role"] != "tool" ||
		msgs[3].(map[string]any)["tool_call_id"] != "call_1" {
		t.Fatalf("function_call_output not mapped: %v", msgs[3])
	}
	tools := out["tools"].([]any)
	if asMap(asMap(tools[0])["function"])["name"] != "read" {
		t.Fatalf("tools not rewrapped: %v", tools)
	}
	if asMap(asMap(out["tool_choice"])["function"])["name"] != "read" {
		t.Fatalf("tool_choice not mapped: %v", out["tool_choice"])
	}

	// plain string input
	out2 := ResponsesReqToChat(map[string]any{"model": "m", "input": "hello"})
	msgs2 := out2["messages"].([]any)
	if len(msgs2) != 1 || msgs2[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("string input: %v", msgs2)
	}

	// pi/codex omit "type" on input message items — must still map
	out3 := ResponsesReqToChat(map[string]any{"model": "m", "input": []any{
		evm(t, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "hi"}}}),
	}})
	msgs3 := out3["messages"].([]any)
	if len(msgs3) != 1 || msgs3[0].(map[string]any)["content"] != "hi" {
		t.Fatalf("typeless message item: %v", msgs3)
	}
}

func TestChatRespToResponses(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "m",
		"choices": []any{evm(t, map[string]any{"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{"role": "assistant", "content": "hi there",
				"tool_calls": []any{map[string]any{"id": "call_9", "type": "function",
					"function": map[string]any{"name": "edit", "arguments": `{"p":1}`}}},
			}})},
		"usage": evm(t, map[string]any{"prompt_tokens": 10, "completion_tokens": 4,
			"cache_read_tokens": 6}),
	}
	out := ChatRespToResponses(chat)
	if out["object"] != "response" || out["status"] != "completed" || out["id"] != "chatcmpl-1" {
		t.Fatalf("bad envelope: %v", out)
	}
	output := out["output"].([]any)
	// message item first, then function_call
	msgItem := output[0].(map[string]any)
	if msgItem["type"] != "message" {
		t.Fatalf("want message item first: %v", output)
	}
	part := asSlice(msgItem["content"])[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hi there" {
		t.Fatalf("text lost: %v", part)
	}
	fc := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" ||
		fc["name"] != "edit" || fc["arguments"] != `{"p":1}` {
		t.Fatalf("function_call lost: %v", fc)
	}
	u := out["usage"].(map[string]any)
	if asInt(u["input_tokens"]) != 10 || asInt(u["output_tokens"]) != 4 ||
		asInt(asMap(u["input_tokens_details"])["cached_tokens"]) != 6 {
		t.Fatalf("usage not mapped: %v", u)
	}

	// reasoning_content becomes a reasoning item; length finish → incomplete
	chat2 := map[string]any{
		"id": "c2", "model": "m",
		"choices": []any{evm(t, map[string]any{"index": 0, "finish_reason": "length",
			"message": map[string]any{"role": "assistant", "content": "",
				"reasoning_content": "thinking"}})},
	}
	out2 := ChatRespToResponses(chat2)
	if out2["status"] != "incomplete" {
		t.Fatalf("length finish should be incomplete: %v", out2["status"])
	}
	if r := out2["output"].([]any)[0].(map[string]any); r["type"] != "reasoning" {
		t.Fatalf("reasoning item missing: %v", out2["output"])
	}
}

func TestChat2RespStream(t *testing.T) {
	tr := NewChat2RespStream("m")
	chunk := func(delta, finish any, usage map[string]any) map[string]any {
		return evm(t, map[string]any{
			"id": "chunk1", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
			"usage":   usage,
		})
	}
	var evs []Event
	evs = append(evs, tr.Chunk(chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))...)
	if len(evs) != 1 || evs[0].Name != "response.created" {
		t.Fatalf("first event should be response.created: %v", evs)
	}
	evs = append(evs, tr.Chunk(chunk(map[string]any{"reasoning_content": "th"}, nil, nil))...)
	evs = append(evs, tr.Chunk(chunk(map[string]any{"content": "hi"}, nil, nil))...)
	evs = append(evs, tr.Chunk(chunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "edit", "arguments": ""}},
	}}, nil, nil))...)
	evs = append(evs, tr.Chunk(chunk(map[string]any{"tool_calls": []any{
		map[string]any{"index": 0, "function": map[string]any{"arguments": `{"p":1}`}},
	}}, nil, nil))...)
	evs = append(evs, tr.Chunk(chunk(map[string]any{}, "tool_calls",
		map[string]any{"prompt_tokens": 7, "completion_tokens": 3}))...)
	evs = append(evs, tr.Finish()...)

	names := make([]string, 0, len(evs))
	for _, e := range evs {
		names = append(names, e.Name)
	}
	for _, want := range []string{"response.created", "response.reasoning_text.delta",
		"response.output_text.delta", "response.output_item.added",
		"response.function_call_arguments.delta"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing event %q; got %v", want, names)
		}
	}
	if n := names[len(names)-1]; n != "response.completed" {
		t.Fatalf("Finish must emit response.completed last, got %v", names)
	}
	completed := evs[len(evs)-1].Data["response"].(map[string]any)
	if completed["status"] != "completed" {
		t.Fatalf("bad status: %v", completed)
	}
	// assembled output: message with full text + function_call with full args
	var msgItem, fcItem map[string]any
	for _, item := range completed["output"].([]any) {
		im := item.(map[string]any)
		switch im["type"] {
		case "message":
			msgItem = im
		case "function_call":
			fcItem = im
		}
	}
	if msgItem == nil || asSlice(msgItem["content"])[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("assembled text missing: %v", completed["output"])
	}
	if fcItem == nil || fcItem["arguments"] != `{"p":1}` || fcItem["name"] != "edit" ||
		fcItem["call_id"] != "call_1" {
		t.Fatalf("assembled function_call wrong: %v", fcItem)
	}
	u := completed["usage"].(map[string]any)
	if asInt(u["input_tokens"]) != 7 || asInt(u["output_tokens"]) != 3 {
		t.Fatalf("usage not carried on completed: %v", u)
	}
	if tr.Finish() != nil {
		t.Fatal("Finish must be idempotent")
	}
}

// Round-trip property: the chat chunks Resp2ChatStream produces must be
// reassembled by Chat2RespStream into the original text/tool/usage.
func TestResponsesStreamRoundTrip(t *testing.T) {
	r2c := NewResp2ChatStream("m")
	ev := func(v any) map[string]any { return evm(t, v) }
	var chatChunks []map[string]any
	appendChunk := func(chunks []map[string]any) { chatChunks = append(chatChunks, chunks...) }
	appendChunk(r2c.Event("response.reasoning_text.delta", ev(map[string]any{"delta": "think"})))
	appendChunk(r2c.Event("response.output_text.delta", ev(map[string]any{"delta": "hello "})))
	appendChunk(r2c.Event("response.output_text.delta", ev(map[string]any{"delta": "world"})))
	appendChunk(r2c.Event("response.output_item.added", ev(map[string]any{"item": map[string]any{
		"id": "fc_9", "type": "function_call", "call_id": "call_1", "name": "edit"}})))
	appendChunk(r2c.Event("response.function_call_arguments.delta",
		ev(map[string]any{"item_id": "fc_9", "delta": `{"p":1}`})))
	appendChunk(r2c.Event("response.completed", ev(map[string]any{"response": map[string]any{
		"id": "resp1", "status": "completed",
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 2}}})))
	chatChunks = append(chatChunks, r2c.Done()...)

	c2r := NewChat2RespStream("m")
	var completed map[string]any
	var texts strings.Builder
	for _, ch := range chatChunks {
		for _, e := range c2r.Chunk(ch) {
			switch e.Name {
			case "response.output_text.delta":
				texts.WriteString(e.Data["delta"].(string))
			case "response.completed":
				completed = e.Data["response"].(map[string]any)
			}
		}
	}
	for _, e := range c2r.Finish() {
		if e.Name == "response.completed" {
			completed = e.Data["response"].(map[string]any)
		}
	}
	if texts.String() != "hello world" {
		t.Fatalf("round-trip text: %q", texts.String())
	}
	if completed == nil {
		t.Fatal("no response.completed emitted")
	}
	var args strings.Builder
	var name string
	for _, item := range completed["output"].([]any) {
		if im := item.(map[string]any); im["type"] == "function_call" {
			args.WriteString(im["arguments"].(string))
			name = im["name"].(string)
		}
	}
	if name != "edit" || args.String() != `{"p":1}` {
		t.Fatalf("round-trip tool call: %s %s", name, args.String())
	}
	if asInt(completed["usage"].(map[string]any)["output_tokens"]) != 2 {
		t.Fatalf("round-trip usage: %v", completed["usage"])
	}
}
