package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatReqToResponses(t *testing.T) {
	chat := map[string]any{
		"model":       "gpt-5.5",
		"max_tokens":  float64(1024),
		"temperature": 0.7,
		"stream":      true,
		"messages": []any{
			map[string]any{"role": "system", "content": "be terse"},
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "tool_calls": []any{
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
	out := ChatReqToResponses(chat)
	if out["model"] != "gpt-5.5" || out["instructions"] != "be terse" {
		t.Fatalf("bad model/instructions: %v %v", out["model"], out["instructions"])
	}
	if out["max_output_tokens"] != 1024 || out["store"] != false || out["stream"] != true {
		t.Fatalf("bad scalars: %v", out)
	}
	input := out["input"].([]any)
	if len(input) != 3 { // user, assistant function_call, tool output (assistant had no text)
		t.Fatalf("want 3 input items, got %d: %v", len(input), input)
	}
	u0 := input[0].(map[string]any)
	if u0["role"] != "user" || !strings.Contains(jsonStr(u0["content"]), "input_text") {
		t.Fatalf("bad user item: %v", u0)
	}
	f := input[1].(map[string]any)
	if f["type"] != "function_call" || f["call_id"] != "call_1" || f["name"] != "read" {
		t.Fatalf("bad function_call item: %v", f)
	}
	fo := input[2].(map[string]any)
	if fo["type"] != "function_call_output" || fo["call_id"] != "call_1" || fo["output"] != "file body" {
		t.Fatalf("bad tool output item: %v", fo)
	}
	tool := out["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "read" || tool["description"] != "read a file" || tool["parameters"] == nil {
		t.Fatalf("bad flattened tool: %v", tool)
	}
	tc := out["tool_choice"].(map[string]any)
	if tc["name"] != "read" || tc["type"] != "function" {
		t.Fatalf("bad tool_choice: %v", tc)
	}
}

func TestResponsesRespToChat(t *testing.T) {
	resp := map[string]any{
		"id": "resp_1", "object": "response", "model": "gpt-5.5", "status": "completed",
		"output": []any{
			map[string]any{"type": "reasoning"},
			map[string]any{"type": "message", "content": []any{
				map[string]any{"type": "output_text", "text": "hello "},
				map[string]any{"type": "output_text", "text": "world"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_9", "name": "ls", "arguments": "{}"},
		},
		"usage": map[string]any{
			"input_tokens": 10, "output_tokens": 5,
			"input_tokens_details": map[string]any{"cached_tokens": 4},
		},
	}
	out := ResponsesRespToChat(resp)
	if out["object"] != "chat.completion" {
		t.Fatalf("bad object: %v", out["object"])
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "hello world" {
		t.Fatalf("bad content: %v", msg["content"])
	}
	tcs := msg["tool_calls"].([]any)
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "call_9" || tc0["function"].(map[string]any)["name"] != "ls" {
		t.Fatalf("bad tool_calls: %v", tcs)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("want tool_calls finish, got %v", choice["finish_reason"])
	}
	u := out["usage"].(map[string]any)
	if u["prompt_tokens"] != 10 || u["completion_tokens"] != 5 || u["cache_read_tokens"] != 4 {
		t.Fatalf("bad usage: %v", u)
	}
}

func TestResp2ChatStream(t *testing.T) {
	tr := NewResp2ChatStream("gpt-5.5")
	feed := func(name string, data map[string]any) []map[string]any {
		return tr.Event(name, data)
	}
	ev := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}

	// reasoning + text deltas
	if got := feed("response.reasoning_summary_text.delta", ev(map[string]any{"delta": "th"})); len(got) != 1 ||
		got[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["reasoning_content"] != "th" {
		t.Fatalf("bad reasoning chunk: %v", got)
	}
	if got := feed("response.output_text.delta", ev(map[string]any{"delta": "hi"})); len(got) != 1 {
		t.Fatalf("bad text chunk: %v", got)
	}

	// tool call: added (id+name), then argument deltas
	added := feed("response.output_item.added", ev(map[string]any{"item": map[string]any{
		"type": "function_call", "call_id": "c1", "name": "edit"}}))
	if len(added) != 1 {
		t.Fatalf("want 1 chunk for tool add, got %v", added)
	}
	tcm := added[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tcm["index"] != 0 || tcm["id"] != "c1" || tcm["function"].(map[string]any)["name"] != "edit" {
		t.Fatalf("bad tool add chunk: %v", tcm)
	}
	argDelta := feed("response.function_call_arguments.delta", ev(map[string]any{"item_id": "c1", "delta": "{\"x"}))
	ad := argDelta[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if ad["index"] != 0 || ad["function"].(map[string]any)["arguments"] != "{\"x" {
		t.Fatalf("bad arg delta chunk: %v", ad)
	}
	// unknown item_id ignored
	if got := feed("response.function_call_arguments.delta", ev(map[string]any{"item_id": "zz", "delta": "y"})); len(got) != 0 {
		t.Fatalf("unknown item_id should be ignored: %v", got)
	}

	// completed → usage-carrying terminal chunk
	done := feed("response.completed", ev(map[string]any{"response": map[string]any{
		"usage": map[string]any{"input_tokens": 7, "output_tokens": 3,
			"input_tokens_details": map[string]any{"cached_tokens": 2}}}}))
	if len(done) != 1 {
		t.Fatalf("want terminal chunk, got %v", done)
	}
	last := done[0]["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Fatalf("bad finish: %v", last["finish_reason"])
	}
	if u := done[0]["usage"].(map[string]any); u["prompt_tokens"] != 7 || u["cache_read_tokens"] != 2 {
		t.Fatalf("bad terminal usage: %v", done[0]["usage"])
	}
	in, out, cacheR := tr.Usage()
	if in != 7 || out != 3 || cacheR != 2 {
		t.Fatalf("bad Usage(): %d %d %d", in, out, cacheR)
	}
	// Done() after completed is a no-op
	if got := tr.Done(); len(got) != 0 {
		t.Fatalf("Done() after completed should be empty: %v", got)
	}
}

// Regression: OpenAI Responses streams correlate argument deltas by item_id
// ("fc_…") while chat tool_call ids use call_id ("call_…") — both must work.
func TestResp2ChatStreamDistinctItemAndCallIDs(t *testing.T) {
	tr := NewResp2ChatStream("gpt-5.5")
	ev := func(v any) map[string]any {
		b, _ := json.Marshal(v)
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	added := tr.Event("response.output_item.added", ev(map[string]any{"item": map[string]any{
		"id": "fc_9", "type": "function_call", "call_id": "call_1", "name": "edit"}}))
	if len(added) != 1 {
		t.Fatalf("tool add chunk: %v", added)
	}
	head := added[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if head["id"] != "call_1" || head["index"] != 0 {
		t.Fatalf("bad add chunk: %v", head)
	}
	// delta correlated by item_id (the real backend convention)
	got := tr.Event("response.function_call_arguments.delta", ev(map[string]any{"item_id": "fc_9", "delta": `{"p":1}`}))
	if len(got) != 1 {
		t.Fatalf("argument delta by item_id lost: %v", got)
	}
	tc := got[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if tc["index"] != 0 || tc["function"].(map[string]any)["arguments"] != `{"p":1}` {
		t.Fatalf("bad argument delta chunk: %v", tc)
	}
	// delta correlated by call_id must also work
	if got := tr.Event("response.function_call_arguments.delta", ev(map[string]any{"item_id": "call_1", "delta": "}"})); len(got) != 1 {
		t.Fatalf("argument delta by call_id lost: %v", got)
	}
	// a second tool call gets its own index despite the two lookup keys
	added2 := tr.Event("response.output_item.added", ev(map[string]any{"item": map[string]any{
		"id": "fc_10", "type": "function_call", "call_id": "call_2", "name": "ls"}}))
	h2 := added2[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if h2["index"] != 1 {
		t.Fatalf("second tool call index: %v", h2)
	}
}
