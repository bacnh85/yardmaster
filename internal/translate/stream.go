package translate

import (
	"time"
)

// Event is an anthropic SSE event (name may be empty for openai-style data-only lines).
type Event struct {
	Name string
	Data map[string]any
}

// OAI2AnthStream transforms OpenAI chat-completion chunks into Anthropic SSE events.
type OAI2AnthStream struct {
	model     string
	started   bool
	openBlock int          // index of currently open block, -1 = none
	blockIdx  int          // next block index (monotonic)
	blockKind byte         // 't' text, 'k' thinking, 'u' tool
	toolOpen  map[int]bool // openai tool index -> open
	usageIn   int
	usageOut  int
	finish    string
	id        string
}

func NewOAI2AnthStream(model string) *OAI2AnthStream {
	return &OAI2AnthStream{model: model, toolOpen: map[int]bool{}, openBlock: -1}
}

func (t *OAI2AnthStream) start() []Event {
	t.started = true
	t.id = "chatcmpl-ar-" + time.Now().UTC().Format("20060102150405.000000000")
	return []Event{{
		Name: "message_start",
		Data: map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": t.id, "type": "message", "role": "assistant", "model": t.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": t.usageIn, "output_tokens": 0},
			},
		},
	}}
}

func (t *OAI2AnthStream) blockStart(btype string) Event {
	idx := t.blockIdx
	t.blockIdx++
	t.openBlock = idx
	ev := Event{Name: "content_block_start", Data: map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": btype},
	}}
	switch btype {
	case "text":
		ev.Data["content_block"] = map[string]any{"type": "text", "text": ""}
		t.blockKind = 't'
	case "thinking":
		ev.Data["content_block"] = map[string]any{"type": "thinking", "thinking": ""}
		t.blockKind = 'k'
	case "tool_use":
		ev.Data["content_block"] = map[string]any{"type": "tool_use", "id": "", "name": "", "input": map[string]any{}}
		t.blockKind = 'u'
	}
	return ev
}

func (t *OAI2AnthStream) blockDelta(delta map[string]any) Event {
	return Event{Name: "content_block_delta", Data: map[string]any{
		"type": "content_block_delta", "index": t.openBlock, "delta": delta,
	}}
}

func (t *OAI2AnthStream) blockStop(index int) Event {
	return Event{Name: "content_block_stop", Data: map[string]any{
		"type": "content_block_stop", "index": index,
	}}
}

func (t *OAI2AnthStream) closeOpen() (out []Event) {
	if t.openBlock >= 0 {
		out = append(out, t.blockStop(t.openBlock))
		t.openBlock = -1
	}
	return
}

// Chunk consumes one openai chunk (parsed data line). Returns anthropic events.
func (t *OAI2AnthStream) Chunk(chunk map[string]any) []Event {
	var out []Event
	if !t.started {
		if u := asMap(chunk["usage"]); u != nil {
			t.usageIn = asInt(u["prompt_tokens"])
		}
		out = append(out, t.start()...)
	}
	if u := asMap(chunk["usage"]); u != nil {
		t.usageIn = asInt(u["prompt_tokens"])
		t.usageOut = asInt(u["completion_tokens"])
	}
	choices := asSlice(chunk["choices"])
	if len(choices) == 0 {
		return out
	}
	ch := asMap(choices[0])
	if ch == nil {
		return out
	}
	if fr := asString(ch["finish_reason"]); fr != "" {
		t.finish = fr
	}
	delta := asMap(ch["delta"])
	if delta == nil {
		return out
	}
	if rc := asString(delta["reasoning_content"]); rc != "" {
		if t.openBlock < 0 || t.blockKind != 'k' {
			out = append(out, t.closeOpen()...)
			out = append(out, t.blockStart("thinking"))
		}
		out = append(out, t.blockDelta(map[string]any{"type": "thinking_delta", "thinking": rc}))
	}
	if c := delta["content"]; c != nil {
		if s := asString(c); s != "" {
			if t.openBlock < 0 || t.blockKind != 't' {
				out = append(out, t.closeOpen()...)
				out = append(out, t.blockStart("text"))
			}
			out = append(out, t.blockDelta(map[string]any{"type": "text_delta", "text": s}))
		}
	}
	for _, tc := range asSlice(delta["tool_calls"]) {
		tcm := asMap(tc)
		if tcm == nil {
			continue
		}
		idx := asInt(tcm["index"])
		fn := asMap(tcm["function"])
		if _, open := t.toolOpen[idx]; !open {
			out = append(out, t.closeOpen()...)
			t.toolOpen[idx] = true
			ev := t.blockStart("tool_use")
			cb := ev.Data["content_block"].(map[string]any)
			cb["id"] = asString(tcm["id"])
			if fn != nil {
				cb["name"] = fn["name"]
			}
			out = append(out, ev)
		}
		if fn != nil {
			if args := asString(fn["arguments"]); args != "" {
				out = append(out, t.blockDelta(map[string]any{
					"type": "input_json_delta", "partial_json": args}))
			}
		}
	}
	return out
}

// Finish emits closing events after the [DONE] marker.
func (t *OAI2AnthStream) Finish() []Event {
	var out []Event
	if !t.started {
		out = append(out, t.start()...)
	}
	out = append(out, t.closeOpen()...)
	stop := "end_turn"
	if t.finish != "" {
		stop = StopOAIToAnth(t.finish)
	}
	out = append(out, Event{Name: "message_delta", Data: map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": t.usageOut, "input_tokens": t.usageIn},
	}})
	out = append(out, Event{Name: "message_stop", Data: map[string]any{"type": "message_stop"}})
	return out
}

// ---- Anthropic SSE -> OpenAI chunks ----

// Anth2OAIStream transforms Anthropic SSE events into OpenAI chat-completion chunks.
type Anth2OAIStream struct {
	model    string
	id       string
	started  bool
	usageIn  int
	usageOut int
	cacheR   int
	cacheW   int
	finish   string
	toolMap  map[int]int // anthropic block index -> openai tool_calls index
	nextTool int
	hasChunk bool
}

func NewAnth2OAIStream(model string) *Anth2OAIStream {
	return &Anth2OAIStream{model: model, toolMap: map[int]int{}}
}

func (t *Anth2OAIStream) chunk(delta map[string]any, finish any) map[string]any {
	t.hasChunk = true
	return map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model":   t.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

// Event consumes one anthropic SSE event. Returns openai chunks to forward.
// The last returned chunk on the message_stop event is the trailing usage chunk.
func (t *Anth2OAIStream) Event(name string, data map[string]any) []map[string]any {
	var out []map[string]any
	switch name {
	case "message_start":
		if !t.started {
			t.started = true
			msg := asMap(data["message"])
			t.id = asString(msg["id"])
			if t.id == "" {
				t.id = "chatcmpl-ar-" + time.Now().UTC().Format("20060102150405.000000000")
			}
			if u := asMap(msg["usage"]); u != nil {
				t.usageIn = asInt(u["input_tokens"])
				t.cacheR = asInt(u["cache_read_input_tokens"])
				t.cacheW = asInt(u["cache_creation_input_tokens"])
			}
			out = append(out, t.chunk(map[string]any{"role": "assistant", "content": ""}, nil))
		}
	case "content_block_start":
		cb := asMap(data["content_block"])
		if cb != nil && asString(cb["type"]) == "tool_use" {
			blockIdx := asInt(data["index"])
			ti := t.nextTool
			t.nextTool++
			t.toolMap[blockIdx] = ti
			out = append(out, t.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": ti, "id": cb["id"], "type": "function",
					"function": map[string]any{"name": cb["name"], "arguments": ""},
				}},
			}, nil))
		}
	case "content_block_delta":
		d := asMap(data["delta"])
		if d == nil {
			return out
		}
		switch asString(d["type"]) {
		case "text_delta":
			out = append(out, t.chunk(map[string]any{"content": d["text"]}, nil))
		case "thinking_delta":
			out = append(out, t.chunk(map[string]any{"reasoning_content": d["thinking"]}, nil))
		case "input_json_delta":
			blockIdx := asInt(data["index"])
			ti, ok := t.toolMap[blockIdx]
			if !ok {
				ti = t.nextTool
				t.nextTool++
				t.toolMap[blockIdx] = ti
			}
			out = append(out, t.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": ti, "function": map[string]any{"arguments": d["partial_json"]},
				}},
			}, nil))
		}
	case "message_delta":
		if dm := asMap(data["delta"]); dm != nil {
			t.finish = asString(dm["stop_reason"])
		}
		if u := asMap(data["usage"]); u != nil {
			t.usageOut = asInt(u["output_tokens"])
			if v := asInt(u["input_tokens"]); v > 0 {
				t.usageIn = v
			}
			// Z.ai (unlike Anthropic) reports usage only here, not in
			// message_start — including the cache fields
			if v := asInt(u["cache_read_input_tokens"]); v > 0 {
				t.cacheR = v
			}
			if v := asInt(u["cache_creation_input_tokens"]); v > 0 {
				t.cacheW = v
			}
		}
		out = append(out, t.chunk(map[string]any{}, StopAnthToOAI(t.finish)))
	case "message_stop":
		out = append(out, t.UsageChunk())
	case "error":
		ed := data["error"]
		if ed == nil {
			ed = data
		}
		out = append(out, map[string]any{"error": ed, "object": "chat.completion.chunk"})
	}
	return out
}

func (t *Anth2OAIStream) UsageChunk() map[string]any {
	prompt := t.usageIn + t.cacheR + t.cacheW
	return map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "model": t.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": prompt, "completion_tokens": t.usageOut,
			"total_tokens":      prompt + t.usageOut,
			"cache_read_tokens": t.cacheR, "cache_write_tokens": t.cacheW,
			// openai-standard dialect — pi reads only this shape
			"prompt_tokens_details": map[string]any{"cached_tokens": t.cacheR, "cache_write_tokens": t.cacheW},
		},
	}
}

// Usage returns the internally accumulated usage (for the router's stats,
// independent of what was forwarded).
func (t *Anth2OAIStream) Usage() (in, out, cacheR, cacheW int) {
	return t.usageIn, t.usageOut, t.cacheR, t.cacheW
}

// SawStart reports whether a message_start was seen (else the stream died early).
func (t *Anth2OAIStream) SawStart() bool { return t.started }
