// Package translate converts between OpenAI chat-completions and Anthropic
// messages wire formats: requests, non-stream responses, and SSE events.
// Maps are used instead of structs so unknown fields pass through instead of
// being silently dropped by schema drift.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Options control provider-specific translation behavior.
type Options struct {
	InjectCacheControl bool // add ephemeral markers when targeting anthropic wire
	AdaptiveThinking   bool // zai-style: thinking adaptive + output_config.effort
}

// ---- helpers ----

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
func asString(v any) string {
	s, _ := v.(string)
	return s
}
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}
func asInt(v any) int {
	return int(asFloat(v))
}
func jsonStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}
func hasCacheControl(blocks []any) bool {
	for _, b := range blocks {
		if m := asMap(b); m != nil {
			if _, ok := m["cache_control"]; ok {
				return true
			}
		}
	}
	return false
}

// ---- stop reason mapping ----

var anthropicToOpenAIStop = map[string]string{
	"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length",
	"tool_use": "tool_calls", "refusal": "content_filter",
}

var openAIToAnthropicStop = map[string]string{
	"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
	"content_filter": "refusal", "function_call": "tool_use",
}

func StopAnthToOAI(s string) string {
	if v, ok := anthropicToOpenAIStop[s]; ok {
		return v
	}
	if s == "" {
		return ""
	}
	return "stop"
}

func StopOAIToAnth(s string) string {
	if v, ok := openAIToAnthropicStop[s]; ok {
		return v
	}
	if s == "" {
		return ""
	}
	return "end_turn"
}

// ---- OpenAI request -> Anthropic request ----

// OpenAIReqToAnthropic converts an OpenAI chat-completions request body to an
// Anthropic messages request body.
func OpenAIReqToAnthropic(req map[string]any, opts Options) map[string]any {
	out := map[string]any{
		"model":  req["model"],
		"stream": req["stream"] == true,
	}
	if mt := asInt(req["max_tokens"]); mt > 0 {
		out["max_tokens"] = mt
	} else if mt := asInt(req["max_completion_tokens"]); mt > 0 {
		out["max_tokens"] = mt
	} else {
		out["max_tokens"] = 8192
	}
	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	switch stop := req["stop"].(type) {
	case string:
		out["stop_sequences"] = []any{stop}
	case []any:
		if len(stop) > 0 {
			out["stop_sequences"] = stop
		}
	}

	// reasoning effort
	if effort := asString(req["reasoning_effort"]); effort != "" {
		if opts.AdaptiveThinking {
			out["thinking"] = map[string]any{"type": "adaptive"}
			out["output_config"] = map[string]any{"effort": mapEffort(effort)}
		} else {
			out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": effortBudget(effort)}
		}
	}

	// system
	var sysTexts []string
	var sysBlocks []any
	for _, m := range asSlice(req["messages"]) {
		mm := asMap(m)
		if mm == nil || asString(mm["role"]) != "system" {
			continue
		}
		texts, blocks := openAIContentToTextBlocks(mm["content"])
		sysTexts = append(sysTexts, texts...)
		sysBlocks = append(sysBlocks, blocks...)
	}
	if len(sysBlocks) > 0 {
		out["system"] = sysBlocks
	} else if len(sysTexts) > 0 {
		out["system"] = strings.Join(sysTexts, "\n\n")
	}

	// tools
	if tools := openAIToolsToAnthropic(asSlice(req["tools"])); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := toolChoiceToAnthropic(req["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}

	// messages (skip system; merge consecutive same-role)
	var msgs []any
	appendMsg := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 {
			last := asMap(msgs[n-1])
			if asString(last["role"]) == role {
				last["content"] = append(asSlice(last["content"]), blocks...)
				return
			}
		}
		msgs = append(msgs, map[string]any{"role": role, "content": blocks})
	}
	for _, m := range asSlice(req["messages"]) {
		mm := asMap(m)
		if mm == nil {
			continue
		}
		switch asString(mm["role"]) {
		case "system":
			continue
		case "user":
			_, blocks := openAIContentToUserBlocks(mm["content"])
			appendMsg("user", blocks)
		case "assistant":
			blocks := openAIAssistantBlocks(mm)
			appendMsg("assistant", blocks)
		case "tool":
			tr := map[string]any{
				"type":        "tool_result",
				"tool_use_id": asString(mm["tool_call_id"]),
				"content":     openAIContentToText(mm["content"]),
			}
			appendMsg("user", []any{tr})
		}
	}
	out["messages"] = msgs

	// inject ephemeral cache markers (the Z.ai / Claude subscription multiplier)
	if opts.InjectCacheControl {
		injectCacheControl(out)
	}
	return out
}

func mapEffort(e string) string {
	if e == "max" {
		return "max"
	}
	if e == "minimal" || e == "low" {
		return "low"
	}
	return "high"
}

func effortBudget(e string) int {
	switch e {
	case "minimal", "low":
		return 4096
	case "high", "max":
		return 32768
	default:
		return 16384
	}
}

// openAIContentToText converts openai message content to plain text.
func openAIContentToText(content any) string {
	texts, _ := openAIContentToTextBlocks(content)
	return strings.Join(texts, "\n")
}
func openAIContentToTextBlocks(content any) (texts []string, blocks []any) {
	switch c := content.(type) {
	case string:
		if c != "" {
			texts = append(texts, c)
		}
	case []any:
		for _, part := range c {
			if pm := asMap(part); pm != nil && asString(pm["type"]) == "text" {
				if t := asString(pm["text"]); t != "" {
					texts = append(texts, t)
				}
			}
		}
	}
	return
}

// openAIContentToUserBlocks converts openai user content to anthropic blocks (text + image).
func openAIContentToUserBlocks(content any) (texts []string, blocks []any) {
	switch c := content.(type) {
	case string:
		if c != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": c})
		}
	case []any:
		for _, part := range c {
			pm := asMap(part)
			if pm == nil {
				continue
			}
			switch asString(pm["type"]) {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": asString(pm["text"])})
			case "image_url":
				iu := asMap(pm["image_url"])
				url := asString(iu["url"])
				if strings.HasPrefix(url, "data:") {
					media, data, ok := parseDataURL(url)
					if ok {
						blocks = append(blocks, map[string]any{"type": "image",
							"source": map[string]any{"type": "base64", "media_type": media, "data": data}})
					}
				} else if url != "" {
					blocks = append(blocks, map[string]any{"type": "image",
						"source": map[string]any{"type": "url", "url": url}})
				}
			}
		}
	}
	return
}

func parseDataURL(url string) (media, data string, ok bool) {
	// data:<media>;base64,<data>
	rest := strings.TrimPrefix(url, "data:")
	i := strings.Index(rest, ";base64,")
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(";base64,"):], true
}

func openAIAssistantBlocks(m map[string]any) []any {
	var blocks []any
	if t := asString(m["content"]); t != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": t})
	} else if arr := asSlice(m["content"]); arr != nil {
		_, tb := openAIContentToUserBlocks(arr)
		blocks = append(blocks, tb...)
	}
	for _, tc := range asSlice(m["tool_calls"]) {
		tcm := asMap(tc)
		fn := asMap(tcm["function"])
		var input any
		raw := asString(fn["arguments"])
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			input = map[string]any{}
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": asString(tcm["id"]),
			"name": asString(fn["name"]), "input": input,
		})
	}
	return blocks
}

func openAIToolsToAnthropic(tools []any) []any {
	var out []any
	for _, t := range tools {
		tm := asMap(t)
		if tm == nil {
			continue
		}
		fn := asMap(tm["function"])
		if fn == nil { // flat {name, description, parameters} style
			fn = tm
		}
		schema := fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"name":         asString(fn["name"]),
			"description":  asString(fn["description"]),
			"input_schema": schema,
		})
	}
	return out
}

func toolChoiceToAnthropic(tc any) map[string]any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "required":
			return map[string]any{"type": "any"}
		case "auto":
			return map[string]any{"type": "auto"}
		default: // "none" or ""
			return nil
		}
	case map[string]any:
		if fn := asMap(v["function"]); fn != nil {
			return map[string]any{"type": "tool", "name": asString(fn["name"])}
		}
	}
	return nil
}

func injectCacheControl(out map[string]any) {
	mark := func(blocks []any) {
		if len(blocks) == 0 || hasCacheControl(blocks) {
			return
		}
		if b := asMap(blocks[len(blocks)-1]); b != nil {
			b["cache_control"] = map[string]any{"type": "ephemeral"}
		}
	}
	if sys, ok := out["system"].([]any); ok {
		mark(sys)
	}
	if tools, ok := out["tools"].([]any); ok {
		mark(tools)
	}
	if msgs, ok := out["messages"].([]any); ok && len(msgs) > 0 {
		last := asMap(msgs[len(msgs)-1])
		if last != nil && asString(last["role"]) == "user" {
			mark(asSlice(last["content"]))
		}
	}
}

// ---- Anthropic request -> OpenAI request ----

func AnthropicReqToOpenAI(req map[string]any, _ Options) map[string]any {
	out := map[string]any{
		"model":  req["model"],
		"stream": req["stream"] == true,
	}
	if mt := asInt(req["max_tokens"]); mt > 0 {
		out["max_tokens"] = mt
	}
	if v, ok := req["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"]; ok {
		out["top_p"] = v
	}
	if ss := asSlice(req["stop_sequences"]); len(ss) > 0 {
		out["stop"] = ss
	}

	// thinking/effort -> reasoning_effort
	if th := asMap(req["thinking"]); th != nil {
		if asString(th["type"]) == "adaptive" {
			if oc := asMap(req["output_config"]); oc != nil {
				if e := asString(oc["effort"]); e != "" {
					out["reasoning_effort"] = mapEffortReverse(e)
				}
			}
		} else if b := asInt(th["budget_tokens"]); b > 0 {
			out["reasoning_effort"] = budgetEffort(b)
		}
	}

	// system
	var sysParts []string
	switch sys := req["system"].(type) {
	case string:
		if sys != "" {
			sysParts = append(sysParts, sys)
		}
	case []any:
		for _, b := range sys {
			if bm := asMap(b); bm != nil && asString(bm["type"]) == "text" {
				sysParts = append(sysParts, asString(bm["text"]))
			}
		}
	}
	msgs := []any{}
	if len(sysParts) > 0 {
		msgs = append(msgs, map[string]any{"role": "system", "content": strings.Join(sysParts, "\n\n")})
	}

	for _, m := range asSlice(req["messages"]) {
		mm := asMap(m)
		if mm == nil {
			continue
		}
		role := asString(mm["role"])
		switch c := mm["content"].(type) {
		case string:
			if c != "" {
				msgs = append(msgs, map[string]any{"role": role, "content": c})
			}
		case []any:
			var text string
			var toolCalls []any
			var pendingToolResults []any
			for _, b := range c {
				bm := asMap(b)
				if bm == nil {
					continue
				}
				switch asString(bm["type"]) {
				case "text":
					text += asString(bm["text"])
				case "thinking":
					// reasoning history: fold into text marker-free; most openai upstreams ignore it
				case "image":
					if src := asMap(bm["source"]); src != nil {
						url := asString(src["url"])
						if asString(src["type"]) == "base64" {
							url = fmt.Sprintf("data:%s;base64,%s", asString(src["media_type"]), asString(src["data"]))
						}
						msgs = append(msgs, map[string]any{"role": role, "content": []any{
							map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}}})
					}
				case "tool_use":
					toolCalls = append(toolCalls, map[string]any{
						"id": bm["id"], "type": "function",
						"function": map[string]any{"name": bm["name"], "arguments": jsonStr(bm["input"])},
					})
				case "tool_result":
					pendingToolResults = append(pendingToolResults, map[string]any{
						"role":         "tool",
						"tool_call_id": bm["tool_use_id"],
						"content":      toolResultText(bm["content"]),
					})
				}
			}
			// flush tool results first (they respond to the previous assistant turn)
			msgs = append(msgs, pendingToolResults...)
			if role == "assistant" && (text != "" || len(toolCalls) > 0) {
				am := map[string]any{"role": "assistant", "content": text}
				if len(toolCalls) > 0 {
					am["tool_calls"] = toolCalls
				}
				msgs = append(msgs, am)
			} else if role == "user" && text != "" {
				msgs = append(msgs, map[string]any{"role": "user", "content": text})
			}
		}
	}
	out["messages"] = msgs

	// tools
	if tools := asSlice(req["tools"]); len(tools) > 0 {
		var ot []any
		for _, t := range tools {
			tm := asMap(t)
			if tm == nil {
				continue
			}
			ot = append(ot, map[string]any{"type": "function", "function": map[string]any{
				"name": tm["name"], "description": tm["description"], "parameters": tm["input_schema"],
			}})
		}
		out["tools"] = ot
	}
	if tc := asMap(req["tool_choice"]); tc != nil {
		switch asString(tc["type"]) {
		case "any":
			out["tool_choice"] = "required"
		case "auto":
			out["tool_choice"] = "auto"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc["name"]}}
		}
	}
	return out
}

func mapEffortReverse(e string) string {
	if e == "low" {
		return "low"
	}
	if e == "max" {
		return "high"
	}
	return "medium"
}

func budgetEffort(b int) string {
	switch {
	case b <= 8192:
		return "low"
	case b >= 24576:
		return "high"
	default:
		return "medium"
	}
}

func toolResultText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, b := range c {
			if bm := asMap(b); bm != nil && asString(bm["type"]) == "text" {
				parts = append(parts, asString(bm["text"]))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ---- non-stream responses ----

// AnthropicRespToOpenAI converts a full anthropic messages response to openai chat completion.
func AnthropicRespToOpenAI(am map[string]any) map[string]any {
	content := ""
	var reasoning string
	var toolCalls []any
	for _, b := range asSlice(am["content"]) {
		bm := asMap(b)
		if bm == nil {
			continue
		}
		switch asString(bm["type"]) {
		case "text":
			content += asString(bm["text"])
		case "thinking":
			reasoning += asString(bm["thinking"])
		case "tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id": bm["id"], "type": "function",
				"function": map[string]any{"name": bm["name"], "arguments": jsonStr(bm["input"])},
			})
		}
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if reasoning != "" {
		msg["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	u := asMap(am["usage"])
	inTok := asInt(u["input_tokens"])
	prompt := inTok
	usage := map[string]any{
		"prompt_tokens":     inTok,
		"completion_tokens": asInt(u["output_tokens"]),
		"total_tokens":      inTok + asInt(u["output_tokens"]),
	}
	if cr := asInt(u["cache_read_input_tokens"]); cr > 0 {
		usage["cache_read_tokens"] = cr
		prompt += cr
	}
	if cw := asInt(u["cache_creation_input_tokens"]); cw > 0 {
		usage["cache_write_tokens"] = cw
		prompt += cw
	}
	usage["prompt_tokens"] = prompt
	return map[string]any{
		"id": am["id"], "object": "chat.completion", "created": am["created_at"],
		"model": am["model"],
		"choices": []any{map[string]any{
			"index": 0, "message": msg,
			"finish_reason": StopAnthToOAI(asString(am["stop_reason"])),
		}},
		"usage": usage,
	}
}

// OpenAIRespToAnthropic converts a full openai chat completion to anthropic messages response.
func OpenAIRespToAnthropic(om map[string]any) map[string]any {
	var content []any
	var reasoning string
	finish := ""
	var msg map[string]any
	if chList := asSlice(om["choices"]); len(chList) > 0 {
		ch0 := asMap(chList[0])
		if ch0 != nil {
			finish = asString(ch0["finish_reason"])
			msg = asMap(ch0["message"])
		}
	}
	if msg != nil {
		if t := asString(msg["content"]); t != "" {
			content = append(content, map[string]any{"type": "text", "text": t})
		}
		reasoning = asString(msg["reasoning_content"])
		if reasoning != "" {
			content = append([]any{map[string]any{"type": "thinking", "thinking": reasoning}}, content...)
		}
		for i, tc := range asSlice(msg["tool_calls"]) {
			tcm := asMap(tc)
			fn := asMap(tcm["function"])
			var input any
			_ = json.Unmarshal([]byte(asString(fn["arguments"])), &input)
			content = append(content, map[string]any{
				"type": "tool_use", "id": asString(tcm["id"]),
				"name": asString(fn["name"]), "input": input,
				// block index for clients that track it
				"index": i,
			})
		}
	}
	if len(content) == 0 {
		content = []any{map[string]any{"type": "text", "text": ""}}
	}
	u := asMap(om["usage"])
	input := asInt(u["prompt_tokens"])
	usage := map[string]any{"input_tokens": input, "output_tokens": asInt(u["completion_tokens"])}
	return map[string]any{
		"id": om["id"], "type": "message", "role": "assistant", "model": om["model"],
		"content":       content,
		"stop_reason":   StopOAIToAnth(finish),
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// ---- error envelopes ----

// ErrToOpenAI wraps an upstream error body into an openai error envelope.
func ErrToOpenAI(body []byte, status int) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if _, ok := m["error"]; ok {
			return body // already openai-shaped
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("upstream error (http %d)", status)
	}
	out, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg, "type": "upstream_error", "code": status,
	}})
	return out
}

// ErrToAnthropic wraps an upstream error body into an anthropic error envelope.
func ErrToAnthropic(body []byte, status int) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if asString(m["type"]) == "error" {
			return body
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("upstream error (http %d)", status)
	}
	out, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
	return out
}

// CountTokensEstimate is a rough token estimate (chars/4) for /v1/messages/count_tokens.
func CountTokensEstimate(req map[string]any) int {
	n := len(asString(req["system"]))
	for _, m := range asSlice(req["messages"]) {
		mm := asMap(m)
		if mm == nil {
			continue
		}
		n += len(asString(mm["content"]))
		if arr := asSlice(mm["content"]); arr != nil {
			b, _ := json.Marshal(arr)
			n += len(b)
		}
	}
	return n / 4
}
