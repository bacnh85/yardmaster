package translate

import (
	"fmt"
	"strings"
	"time"
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

// ---- request: responses -> chat ----

// ResponsesReqToChat converts a Responses-API request body to an OpenAI
// chat-completions request. Whitelist mapping: unknown fields (store,
// previous_response_id, include, metadata) are dropped — the router is
// stateless. ponytail: reasoning input items are dropped (no chat carrier);
// responses-native clients keep byte-exact passthrough instead.
func ResponsesReqToChat(req map[string]any) map[string]any {
	out := map[string]any{"model": req["model"]}
	for _, k := range []string{"temperature", "top_p", "stream", "parallel_tool_calls", "stop", "user"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if mt := asFloat(req["max_output_tokens"]); mt > 0 {
		out["max_tokens"] = asInt(mt)
	}
	if r := asMap(req["reasoning"]); r != nil {
		if e := asString(r["effort"]); e != "" {
			out["reasoning_effort"] = e
		}
	}
	msgs := make([]any, 0, 16)
	if instr := asString(req["instructions"]); instr != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instr})
	}
	switch in := req["input"].(type) {
	case string:
		if in != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	default:
		for _, item := range asSlice(req["input"]) {
			im := asMap(item)
			itype := asString(im["type"])
			if itype == "" && asString(im["role"]) != "" {
				itype = "message" // type is optional on input message items
			}
			switch itype {
			case "message":
				role := asString(im["role"])
				if role == "developer" {
					role = "system"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": responsesContentToChat(im["content"])})
			case "function_call":
				msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"id": asString(im["call_id"]), "type": "function",
					"function": map[string]any{"name": asString(im["name"]), "arguments": asString(im["arguments"])},
				}}})
			case "function_call_output":
				msgs = append(msgs, map[string]any{"role": "tool",
					"tool_call_id": asString(im["call_id"]), "content": asString(im["output"])})
			}
		}
	}
	out["messages"] = msgs
	if tools := toolsFromResponses(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := toolChoiceFromResponses(req["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	return out
}

// responsesContentToChat maps responses content (string | part array) to chat
// content: text parts joined, input_image kept as chat image_url blocks.
func responsesContentToChat(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	var texts []string
	var blocks []any
	for _, part := range asSlice(v) {
		pm := asMap(part)
		switch asString(pm["type"]) {
		case "input_text", "output_text", "text", "refusal":
			if t := asString(pm["text"]); t != "" {
				texts = append(texts, t)
			}
		case "input_image":
			if url := asString(pm["image_url"]); url != "" {
				blocks = append(blocks, map[string]any{"type": "image_url",
					"image_url": map[string]any{"url": url}})
			}
		}
	}
	txt := strings.Join(texts, "")
	if len(blocks) == 0 {
		return txt
	}
	if txt != "" {
		blocks = append([]any{map[string]any{"type": "text", "text": txt}}, blocks...)
	}
	return blocks
}

func toolsFromResponses(v any) []any {
	var out []any
	for _, t := range asSlice(v) {
		tm := asMap(t)
		fn := map[string]any{"name": asString(tm["name"])}
		if d := asString(tm["description"]); d != "" {
			fn["description"] = d
		}
		if tm["parameters"] != nil {
			fn["parameters"] = tm["parameters"]
		}
		if tm["strict"] != nil {
			fn["strict"] = tm["strict"]
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func toolChoiceFromResponses(tc any) any {
	switch v := tc.(type) {
	case string:
		return v
	case map[string]any:
		if n := asString(v["name"]); n != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": n}}
		}
	}
	return nil
}

// ---- non-stream response: chat -> responses ----

// ChatRespToResponses converts an OpenAI chat-completion body to a
// Responses-API response (inverse of ResponsesRespToChat).
func ChatRespToResponses(chat map[string]any) map[string]any {
	out := map[string]any{
		"id": asString(chat["id"]), "object": "response", "model": asString(chat["model"]),
		"status": "completed",
	}
	output := make([]any, 0, 4)
	var choice map[string]any
	if cs := asSlice(chat["choices"]); len(cs) > 0 {
		choice = asMap(cs[0])
	}
	if choice != nil {
		m := asMap(choice["message"])
		if r := asString(m["reasoning_content"]); r != "" {
			output = append(output, map[string]any{"type": "reasoning",
				"content": []any{map[string]any{"type": "reasoning_text", "text": r}}})
		}
		var content []any
		if t := openAIContentToText(m["content"]); t != "" {
			content = append(content, map[string]any{"type": "output_text", "text": t})
		}
		if len(content) > 0 {
			output = append(output, map[string]any{"type": "message", "role": "assistant", "content": content})
		}
		for _, tc := range asSlice(m["tool_calls"]) {
			tcm := asMap(tc)
			fn := asMap(tcm["function"])
			output = append(output, map[string]any{
				"type": "function_call", "call_id": asString(tcm["id"]),
				"name": asString(fn["name"]), "arguments": asString(fn["arguments"]),
				"status": "completed",
			})
		}
		switch asString(choice["finish_reason"]) {
		case "length":
			out["status"] = "incomplete"
		case "tool_calls":
			// completed; function_call items carry the stop reason
		}
	}
	if len(output) == 0 {
		output = append(output, map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": ""}}})
	}
	out["output"] = output
	if u := chatUsageToResponses(asMap(chat["usage"])); u != nil {
		out["usage"] = u
	}
	return out
}

// chatUsageToResponses is the inverse of responsesUsageToChat.
func chatUsageToResponses(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	in, outp := asInt(u["prompt_tokens"]), asInt(u["completion_tokens"])
	m := map[string]any{"input_tokens": in, "output_tokens": outp, "total_tokens": in + outp}
	if cr := asInt(u["cache_read_tokens"]); cr > 0 {
		m["input_tokens_details"] = map[string]any{"cached_tokens": cr}
	}
	return m
}

// ---- stream: openai chat chunks -> responses SSE events ----

// Chat2RespStream converts openai chat-completion chunks into Responses-API
// SSE events (inverse of Resp2ChatStream). pi/codex clients correlate deltas
// by output_index and backfill tool name/arguments from response.completed —
// so the completed event carries the fully assembled output array + usage.
type Chat2RespStream struct {
	model     string
	id        string
	started   bool
	finished  bool
	nextIdx   int // responses output_index
	tools     map[int]*chat2respTool
	toolByID  map[int]int // chat tool_calls index -> responses output_index
	text      strings.Builder
	reason    strings.Builder
	msgIdx    int // -1 until the message item is announced
	reasonIdx int // -1 until the reasoning item is announced
	usage     map[string]any
	items     []any // output items in announce order (mutated in place)
	itemIdx   []int // responses output_index per item (done events need it)
}

type chat2respTool struct {
	itemID string
	callID string
	name   string
	args   strings.Builder
	item   map[string]any // live output item, mutated as args grow
}

func NewChat2RespStream(model string) *Chat2RespStream {
	return &Chat2RespStream{
		model: model, id: "resp-ar-" + time.Now().UTC().Format("20060102150405.000000000"),
		msgIdx: -1, reasonIdx: -1, tools: map[int]*chat2respTool{}, toolByID: map[int]int{},
	}
}

func (t *Chat2RespStream) ev(name string, data map[string]any) Event {
	data["type"] = name
	return Event{Name: name, Data: data}
}

// Chunk consumes one openai chat chunk; returns responses events to forward.
func (t *Chat2RespStream) Chunk(chunk map[string]any) []Event {
	var out []Event
	if !t.started {
		t.started = true
		if id := asString(chunk["id"]); id != "" {
			t.id = id
		}
		out = append(out, t.ev("response.created", map[string]any{"response": map[string]any{
			"id": t.id, "object": "response", "model": t.model, "status": "in_progress",
		}}))
	}
	if u := asMap(chunk["usage"]); u != nil {
		t.usage = chatUsageToResponses(u)
	}
	var delta map[string]any
	if cs := asSlice(chunk["choices"]); len(cs) > 0 {
		delta = asMap(asMap(cs[0])["delta"])
	}
	if d := asString(delta["content"]); d != "" {
		if t.msgIdx < 0 {
			// announce the message item BEFORE its deltas — clients correlate
			// deltas by output_index and drop orphans
			t.msgIdx = t.nextIdx
			t.nextIdx++
			msg := map[string]any{"type": "message", "id": "msg_0",
				"role": "assistant", "content": []any{}}
			t.items = append(t.items, msg)
			t.itemIdx = append(t.itemIdx, t.msgIdx)
			out = append(out, t.ev("response.output_item.added", map[string]any{
				"output_index": t.msgIdx, "item": msg}))
		}
		t.text.WriteString(d)
		out = append(out, t.ev("response.output_text.delta", map[string]any{
			"item_id": "msg_0", "output_index": t.msgIdx, "content_index": 0, "delta": d,
		}))
	}
	if d := asString(delta["reasoning_content"]); d != "" {
		if t.reasonIdx < 0 {
			t.reasonIdx = t.nextIdx
			t.nextIdx++
			rs := map[string]any{"type": "reasoning", "id": "rs_0", "content": []any{}}
			t.items = append(t.items, rs)
			t.itemIdx = append(t.itemIdx, t.reasonIdx)
			out = append(out, t.ev("response.output_item.added", map[string]any{
				"output_index": t.reasonIdx, "item": rs}))
		}
		t.reason.WriteString(d)
		out = append(out, t.ev("response.reasoning_text.delta", map[string]any{
			"item_id": "rs_0", "output_index": t.reasonIdx, "content_index": 0, "delta": d,
		}))
	}
	for _, tc := range asSlice(delta["tool_calls"]) {
		tcm := asMap(tc)
		idx := asInt(tcm["index"])
		tool, ok := t.tools[idx]
		if !ok {
			fn := asMap(tcm["function"])
			tool = &chat2respTool{
				itemID: "fc_" + asString(tcm["id"]),
				callID: asString(tcm["id"]),
				name:   asString(fn["name"]),
			}
			if tool.callID == "" {
				tool.callID = fmt.Sprintf("call_%d", idx)
				tool.itemID = "fc_" + tool.callID
			}
			tool.item = map[string]any{"type": "function_call", "id": tool.itemID,
				"call_id": tool.callID, "name": tool.name, "arguments": ""}
			t.tools[idx] = tool
			t.toolByID[idx] = t.nextIdx
			t.items = append(t.items, tool.item)
			t.itemIdx = append(t.itemIdx, t.nextIdx)
			out = append(out, t.ev("response.output_item.added", map[string]any{
				"output_index": t.nextIdx,
				"item":         tool.item,
			}))
			t.nextIdx++
		} else if n := asString(asMap(tcm["function"])["name"]); n != "" && tool.name == "" {
			tool.name = n
			tool.item["name"] = n
		}
		if d := asString(asMap(tcm["function"])["arguments"]); d != "" {
			tool.args.WriteString(d)
			tool.item["arguments"] = tool.args.String()
			out = append(out, t.ev("response.function_call_arguments.delta", map[string]any{
				"item_id": tool.itemID, "output_index": t.toolByID[idx], "delta": d,
			}))
		}
	}
	// finish_reason does NOT trigger Finish here: usage rides the final chunk
	// (often after finish); the proxy calls Finish after the stream ends.
	return out
}

// Finish emits the terminal events (idempotent). response.completed carries
// the assembled output array + usage — pi backfills tool calls from it.
func (t *Chat2RespStream) Finish() []Event {
	if !t.started || t.finished {
		return nil
	}
	t.finished = true
	var out []Event
	// fill the announced placeholders in place, then mark everything done
	for i, it := range t.items {
		im := it.(map[string]any)
		switch asString(im["type"]) {
		case "message":
			im["content"] = []any{map[string]any{"type": "output_text", "text": t.text.String()}}
		case "reasoning":
			im["content"] = []any{map[string]any{"type": "reasoning_text", "text": t.reason.String()}}
		case "function_call":
			im["status"] = "completed"
		}
		out = append(out, t.ev("response.output_item.done", map[string]any{
			"output_index": t.itemIdx[i], "item": it}))
	}
	if t.usage == nil {
		t.usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	resp := map[string]any{"id": t.id, "object": "response", "model": t.model,
		"status": "completed", "output": t.items, "usage": t.usage}
	out = append(out, t.ev("response.completed", map[string]any{"response": resp}))
	return out
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
