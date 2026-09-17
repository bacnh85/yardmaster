package translate

import (
	"strings"
)

// The Responses wire (OpenAI /v1/responses, spoken by the Codex backend at
// chatgpt.com/backend-api/codex) expressed as OpenAI chat-completions
// translations: request mapping, non-stream response mapping, and a SSE
// event → chat-chunk stream translator. Chat wire is the hub; anthropic
// clients compose through the existing chat translators.

// ---- request: chat -> responses ----

// ChatReqToResponses converts an OpenAI chat-completions request body to a
// Responses-API request body.
func ChatReqToResponses(req map[string]any) map[string]any {
	out := map[string]any{
		"model": req["model"],
		"store": false, // never persist to the provider's side
	}
	for _, k := range []string{"temperature", "top_p", "stream", "parallel_tool_calls", "user"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if mt := asFloat(req["max_tokens"]); mt > 0 {
		out["max_output_tokens"] = asInt(mt)
	}
	var instr []string
	input := make([]any, 0, 16)
	for _, m := range asSlice(req["messages"]) {
		msg := asMap(m)
		switch asString(msg["role"]) {
		case "system", "developer":
			if t := openAIContentToText(msg["content"]); t != "" {
				instr = append(instr, t)
			}
		case "user":
			input = append(input, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": openAIContentToText(msg["content"])}},
			})
		case "assistant":
			for _, tc := range asSlice(msg["tool_calls"]) {
				tcm := asMap(tc)
				fn := asMap(tcm["function"])
				input = append(input, map[string]any{
					"type": "function_call", "call_id": asString(tcm["id"]),
					"name": asString(fn["name"]), "arguments": asString(fn["arguments"]),
				})
			}
			if t := openAIContentToText(msg["content"]); t != "" {
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": t}},
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": asString(msg["tool_call_id"]),
				"output": toolResultText(msg["content"]),
			})
		}
	}
	if len(instr) > 0 {
		out["instructions"] = strings.Join(instr, "\n\n")
	}
	out["input"] = input
	if tools := toolsToResponses(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := req["tool_choice"]; tc != nil {
		out["tool_choice"] = toolChoiceToResponses(tc)
	}
	return out
}

func toolsToResponses(v any) []any {
	var out []any
	for _, t := range asSlice(v) {
		tm := asMap(t)
		if asString(tm["type"]) != "function" && tm["function"] == nil {
			continue // only function tools are supported on this path
		}
		fn := asMap(tm["function"])
		item := map[string]any{"type": "function", "name": asString(fn["name"])}
		if d := asString(fn["description"]); d != "" {
			item["description"] = d
		}
		if fn["parameters"] != nil {
			item["parameters"] = fn["parameters"]
		}
		if fn["strict"] != nil {
			item["strict"] = fn["strict"]
		}
		out = append(out, item)
	}
	return out
}

func toolChoiceToResponses(tc any) any {
	switch v := tc.(type) {
	case string:
		return v // "auto" | "none" | "required" pass through
	case map[string]any:
		if fn := asMap(v["function"]); fn != nil {
			return map[string]any{"type": "function", "name": asString(fn["name"])}
		}
	}
	return "auto"
}

// ---- non-stream response: responses -> chat ----

// ResponsesRespToChat converts a full Responses-API response to an OpenAI
// chat-completion body.
func ResponsesRespToChat(resp map[string]any) map[string]any {
	var sb strings.Builder
	var toolCalls []any
	for _, item := range asSlice(resp["output"]) {
		im := asMap(item)
		switch asString(im["type"]) {
		case "message":
			for _, part := range asSlice(im["content"]) {
				pm := asMap(part)
				if asString(pm["type"]) == "output_text" {
					sb.WriteString(asString(pm["text"]))
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, map[string]any{
				"id": asString(im["call_id"]), "type": "function",
				"function": map[string]any{"name": asString(im["name"]), "arguments": asString(im["arguments"])},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if sb.Len() > 0 {
		msg["content"] = sb.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := "stop"
	switch asString(resp["status"]) {
	case "incomplete":
		finish = "length"
	default:
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	out := map[string]any{
		"id": asString(resp["id"]), "object": "chat.completion", "model": asString(resp["model"]),
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if u := responsesUsageToChat(asMap(resp["usage"])); u != nil {
		out["usage"] = u
	}
	return out
}

// responsesUsageToChat maps Responses usage into the openai-style usage map
// our accounting already parses (cache_read_tokens is our house extension).
func responsesUsageToChat(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	in, out := asInt(u["input_tokens"]), asInt(u["output_tokens"])
	m := map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out}
	if d := asMap(u["input_tokens_details"]); d != nil {
		m["cache_read_tokens"] = asInt(d["cached_tokens"])
	}
	return m
}

// ---- stream: responses SSE -> openai chat chunks ----

// Resp2ChatStream converts Responses-API SSE events into openai chat chunks.
// Feed Event(name, data) per SSE event; it returns zero or more chat chunks.
type Resp2ChatStream struct {
	model    string
	started  bool
	finished bool
	nextIdx  int
	toolIdx  map[string]int // item id OR call_id -> openai tool_call index
	usage    map[string]any
	in       int
	out      int
	cacheR   int
}

func NewResp2ChatStream(model string) *Resp2ChatStream {
	return &Resp2ChatStream{model: model, toolIdx: map[string]int{}}
}

// Usage returns captured token counts.
func (t *Resp2ChatStream) Usage() (in, out, cacheR int) { return t.in, t.out, t.cacheR }

func (t *Resp2ChatStream) chunk(delta map[string]any, finish any) map[string]any {
	if !t.started {
		t.started = true
	}
	return map[string]any{
		"id": "ar-resp", "object": "chat.completion.chunk", "model": t.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

// Done emits the terminal chunks once the response stream has ended
// (after a completed/incomplete/failed event).
func (t *Resp2ChatStream) Done() []map[string]any {
	if !t.started || t.finished {
		return nil
	}
	t.finished = true
	final := t.chunk(map[string]any{}, "stop")
	if t.usage != nil {
		final["usage"] = t.usage
	}
	return []map[string]any{final}
}

// Event converts one Responses SSE event. name is the SSE event: field (may
// be empty; data.type is used as fallback).
func (t *Resp2ChatStream) Event(name string, data map[string]any) []map[string]any {
	if name == "" {
		name = asString(data["type"])
	}
	switch name {
	case "response.output_text.delta":
		return []map[string]any{t.chunk(map[string]any{"content": asString(data["delta"])}, nil)}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		// house extension (deepseek/glm-style): reasoning exposed separately
		return []map[string]any{t.chunk(map[string]any{"reasoning_content": asString(data["delta"])}, nil)}
	case "response.output_item.added":
		item := asMap(data["item"])
		if asString(item["type"]) != "function_call" {
			return nil
		}
		// index under BOTH ids: argument deltas correlate by item_id ("fc_…"),
		// while chat tool_call ids use call_id ("call_…")
		idx := t.nextIdx
		t.nextIdx++
		for _, k := range []string{asString(item["id"]), asString(item["call_id"])} {
			if k != "" {
				t.toolIdx[k] = idx
			}
		}
		return []map[string]any{t.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "id": asString(item["call_id"]), "type": "function",
			"function": map[string]any{"name": asString(item["name"]), "arguments": ""},
		}}}, nil)}
	case "response.function_call_arguments.delta":
		callID := asString(data["item_id"])
		idx, ok := t.toolIdx[callID]
		if !ok {
			return nil
		}
		return []map[string]any{t.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": idx, "function": map[string]any{"arguments": asString(data["delta"])},
		}}}, nil)}
	case "response.completed", "response.incomplete", "response.failed":
		if resp := asMap(data["response"]); resp != nil {
			t.usage = responsesUsageToChat(asMap(resp["usage"]))
			if t.usage != nil {
				t.in = t.usage["prompt_tokens"].(int)
				t.out = t.usage["completion_tokens"].(int)
				t.cacheR, _ = t.usage["cache_read_tokens"].(int)
			}
		}
		return t.Done()
	}
	return nil
}
