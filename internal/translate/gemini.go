package translate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The Gemini wire (generativelanguage.googleapis.com v1beta generateContent)
// expressed as OpenAI chat-completions translations: request mapping both
// directions, non-stream response mapping both directions, SSE stream
// translators both directions, and error envelopes. Chat wire is the hub;
// gemini-native clients and upstreams compose through these.
//
// The gemini body never carries the model — the proxy composes it into the
// URL path (/v1beta/models/<model>:generateContent[?alt=sse]) — so request
// translations emit no model field, and response translations take the
// client-facing model as a parameter (modelVersion is a provider-internal
// version string, not the requested id).

// ---- request: chat -> gemini ----

// ChatReqToGemini converts an OpenAI chat-completions request body to a
// gemini generateContent body. Lenient by package convention: unmappable
// pieces are dropped rather than failed (remote https image URLs, unknown
// roles), so error is reserved and currently always nil.
func ChatReqToGemini(req map[string]any) (map[string]any, error) {
	// gemini correlates tool results to calls by function NAME, not call id;
	// resolve tool_call_id -> name up front from every assistant turn.
	callNames := map[string]string{}
	for _, m := range asSlice(req["messages"]) {
		msg := asMap(m)
		if asString(msg["role"]) != "assistant" {
			continue
		}
		for _, tc := range asSlice(msg["tool_calls"]) {
			tcm := asMap(tc)
			fn := asMap(tcm["function"])
			if id := asString(tcm["id"]); id != "" && fn != nil {
				callNames[id] = asString(fn["name"])
			}
		}
	}

	var instr []string
	contents := make([]any, 0, 16)
	for _, m := range asSlice(req["messages"]) {
		msg := asMap(m)
		switch asString(msg["role"]) {
		case "system", "developer":
			if t := openAIContentToText(msg["content"]); t != "" {
				instr = append(instr, t)
			}
		case "user":
			contents = append(contents, map[string]any{
				"role": "user", "parts": openAIUserParts(msg["content"]),
			})
		case "assistant":
			// reasoning_content has no gemini request carrier (thought parts
			// require provider-issued thought signatures) — dropped.
			var parts []any
			if t := openAIContentToText(msg["content"]); t != "" {
				parts = append(parts, map[string]any{"text": t})
			}
			for _, tc := range asSlice(msg["tool_calls"]) {
				fn := asMap(asMap(tc)["function"])
				parts = append(parts, map[string]any{"functionCall": map[string]any{
					"name": asString(fn["name"]),
					"args": jsonArgsToMap(asString(fn["arguments"])),
				}})
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}
		case "tool":
			// functionResponse rides a user-role content; gemini rejects
			// unknown roles. Unresolvable call ids fall back to the raw id —
			// losing the result would corrupt the conversation harder.
			name := callNames[asString(msg["tool_call_id"])]
			if name == "" {
				name = asString(msg["tool_call_id"])
			}
			contents = append(contents, map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{
					"name":     name,
					"response": toolResponseObject(toolResultText(msg["content"])),
				}},
			}})
		}
	}

	out := map[string]any{"contents": contents}
	if len(instr) > 0 {
		out["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": strings.Join(instr, "\n\n")}},
		}
	}

	gen := map[string]any{}
	if mt := asFloat(req["max_tokens"]); mt > 0 {
		gen["maxOutputTokens"] = asInt(mt)
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok {
			gen[map[string]string{"temperature": "temperature", "top_p": "topP"}[k]] = v
		}
	}
	if seqs := stopToSequences(req["stop"]); len(seqs) > 0 {
		gen["stopSequences"] = seqs
	}
	if len(gen) > 0 {
		out["generationConfig"] = gen
	}

	if tools := toolsToGemini(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if cfg := toolChoiceToGemini(req["tool_choice"]); cfg != nil {
		out["toolConfig"] = cfg
	}
	// stream is a URL variant (streamGenerateContent), not a body field.
	return out, nil
}

// openAIUserParts converts openai user content (string | blocks) to gemini
// parts. Data-URL images become inlineData; remote https images are dropped
// without error — v1beta inlineData accepts only base64, and fileData URIs
// must be Google-Cloud-hosted, so arbitrary URLs have no gemini carrier.
func openAIUserParts(content any) []any {
	var parts []any
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, map[string]any{"text": c})
		}
	case []any:
		for _, part := range c {
			pm := asMap(part)
			if pm == nil {
				continue
			}
			switch asString(pm["type"]) {
			case "text":
				if t := asString(pm["text"]); t != "" {
					parts = append(parts, map[string]any{"text": t})
				}
			case "image_url":
				url := asString(asMap(pm["image_url"])["url"])
				if media, data, ok := parseDataURL(url); ok {
					parts = append(parts, map[string]any{"inlineData": map[string]any{
						"mimeType": media, "data": data}})
				}
			}
		}
	}
	return parts
}

func jsonArgsToMap(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return map[string]any{} // gemini requires args to be an object
	}
	return m
}

// toolResponseObject converts chat tool content (a string) to the gemini
// functionResponse.response object. A JSON-object payload passes through
// verbatim — gemini-native clients structured exactly that way, and it makes
// gemini→chat→gemini tool results round-trip losslessly; plain text gets the
// conventional {"result": …} envelope.
func toolResponseObject(text string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err == nil && m != nil {
		return m
	}
	return map[string]any{"result": text}
}

func stopToSequences(stop any) []any {
	switch s := stop.(type) {
	case string:
		if s != "" {
			return []any{s}
		}
	case []any:
		return s
	}
	return nil
}

func toolsToGemini(v any) []any {
	var decls []any
	for _, t := range asSlice(v) {
		tm := asMap(t)
		if asString(tm["type"]) != "function" && tm["function"] == nil {
			continue // only function tools are supported on this path
		}
		fn := asMap(tm["function"])
		decl := map[string]any{"name": asString(fn["name"])}
		if d := asString(fn["description"]); d != "" {
			decl["description"] = d
		}
		if fn["parameters"] != nil {
			// openai JSON schema passes as-is; type-case normalization is the
			// provider adapter's job, not the translator's
			decl["parameters"] = fn["parameters"]
		}
		decls = append(decls, decl)
	}
	if len(decls) == 0 {
		return nil
	}
	return []any{map[string]any{"functionDeclarations": decls}}
}

func toolChoiceToGemini(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
		case "none":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		case "required":
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		}
	case map[string]any:
		if fn := asMap(v["function"]); fn != nil {
			if name := asString(fn["name"]); name != "" {
				return map[string]any{"functionCallingConfig": map[string]any{
					"mode": "ANY", "allowedFunctionNames": []any{name}}}
			}
		}
	}
	return nil // unknown / absent: leave the provider default (AUTO)
}

// ---- request: gemini -> chat ----

// GeminiReqToChat converts a gemini generateContent request body to an OpenAI
// chat-completions request (inverse of ChatReqToGemini, for inbound
// gemini-native clients). gemini correlates responses to calls by name, chat
// by id: functionCall parts get stable ids "call_<n>" in encounter order and
// the id is recorded per function name, so a later functionResponse resolves
// back to the matching tool_call_id.
func GeminiReqToChat(req map[string]any) (map[string]any, error) {
	out := map[string]any{}
	if req["model"] != nil {
		out["model"] = req["model"] // usually injected from the URL path upstream
	}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if gen := asMap(req["generationConfig"]); gen != nil {
		if mt := asFloat(gen["maxOutputTokens"]); mt > 0 {
			out["max_tokens"] = asInt(mt)
		}
		if gen["temperature"] != nil {
			out["temperature"] = gen["temperature"]
		}
		if gen["topP"] != nil {
			out["top_p"] = gen["topP"]
		}
		if seqs := asSlice(gen["stopSequences"]); len(seqs) > 0 {
			out["stop"] = seqs
		}
	}

	msgs := make([]any, 0, 16)
	if si := req["systemInstruction"]; si != nil {
		if t := geminiInstructionText(si); t != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": t})
		}
	}
	callIDByName := map[string]string{}
	nextCall := 0
	for _, c := range asSlice(req["contents"]) {
		cm := asMap(c)
		role := asString(cm["role"])
		var text strings.Builder
		var blocks []any
		var toolCalls []any
		for _, p := range asSlice(cm["parts"]) {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			if t := asString(pm["text"]); t != "" {
				// thought parts are gemini-internal scratch; dropped on input
				text.WriteString(t)
			}
			if inl := asMap(pm["inlineData"]); inl != nil {
				blocks = append(blocks, map[string]any{"type": "image_url",
					"image_url": map[string]any{"url": fmt.Sprintf("data:%s;base64,%s",
						asString(inl["mimeType"]), asString(inl["data"]))}})
			}
			if fc := asMap(pm["functionCall"]); fc != nil {
				id := fmt.Sprintf("call_%d", nextCall)
				nextCall++
				if n := asString(fc["name"]); n != "" {
					callIDByName[n] = id
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{
						"name": asString(fc["name"]), "arguments": jsonStr(fc["args"]),
					},
				})
			}
			if fr := asMap(pm["functionResponse"]); fr != nil {
				// resolve the id via the function name; an unmatched response
				// still needs an id so the chat conversation stays well-formed
				name := asString(fr["name"])
				id := callIDByName[name]
				if id == "" {
					id = fmt.Sprintf("call_%d", nextCall)
					nextCall++
				}
				msgs = append(msgs, map[string]any{
					"role": "tool", "tool_call_id": id,
					"content": jsonStr(fr["response"]),
				})
			}
		}
		chatRole := "user"
		if role == "model" {
			chatRole = "assistant"
		}
		if text.Len() > 0 || len(blocks) > 0 {
			content := any(text.String())
			if len(blocks) > 0 {
				arr := blocks
				if text.Len() > 0 {
					arr = append([]any{map[string]any{"type": "text", "text": text.String()}}, arr...)
				}
				content = arr
			}
			msgs = append(msgs, map[string]any{"role": chatRole, "content": content})
		}
		if len(toolCalls) > 0 {
			msgs = append(msgs, map[string]any{"role": chatRole, "tool_calls": toolCalls})
		}
	}
	out["messages"] = msgs

	if tools := toolsFromGemini(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := toolChoiceFromGemini(req["toolConfig"]); tc != nil {
		out["tool_choice"] = tc
	}
	// unknown fields (candidateCount, safetySettings, …) are dropped — the
	// router is stateless and chat has no carrier for them
	return out, nil
}

// geminiInstructionText flattens systemInstruction (string | {parts}) to text.
func geminiInstructionText(v any) string {
	switch iv := v.(type) {
	case string:
		return iv
	case map[string]any:
		var sb strings.Builder
		for _, p := range asSlice(iv["parts"]) {
			if t := asString(asMap(p)["text"]); t != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n\n")
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

func toolsFromGemini(v any) []any {
	var out []any
	for _, t := range asSlice(v) {
		for _, d := range asSlice(asMap(t)["functionDeclarations"]) {
			dm := asMap(d)
			fn := map[string]any{"name": asString(dm["name"])}
			if desc := asString(dm["description"]); desc != "" {
				fn["description"] = desc
			}
			if dm["parameters"] != nil {
				fn["parameters"] = dm["parameters"]
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

func toolChoiceFromGemini(cfg any) any {
	fcc := asMap(asMap(cfg)["functionCallingConfig"])
	if fcc == nil {
		return nil
	}
	switch asString(fcc["mode"]) {
	case "NONE":
		return "none"
	case "ANY":
		if names := asSlice(fcc["allowedFunctionNames"]); len(names) > 0 {
			return map[string]any{"type": "function",
				"function": map[string]any{"name": asString(names[0])}}
		}
		return "required"
	default: // AUTO and unknown
		return "auto"
	}
}

// ---- non-stream response: gemini -> chat ----

var geminiToOpenAIStop = map[string]string{
	"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter",
	"RECITATION": "content_filter",
}

// GeminiRespToChat converts a full gemini generateContent response to an OpenAI
// chat-completion body. candidates[0] only — the hub carries a single choice.
func GeminiRespToChat(gem map[string]any, model string) map[string]any {
	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []any
	finish := "stop"
	if cands := asSlice(gem["candidates"]); len(cands) > 0 {
		cand := asMap(cands[0])
		nextCall := 0
		for _, p := range asSlice(asMap(asMap(cand)["content"])["parts"]) {
			pm := asMap(p)
			if pm == nil {
				continue
			}
			if fc := asMap(pm["functionCall"]); fc != nil {
				toolCalls = append(toolCalls, map[string]any{
					"id": fmt.Sprintf("call_%d", nextCall), "type": "function",
					"function": map[string]any{
						"name": asString(fc["name"]), "arguments": jsonStr(fc["args"]),
					},
				})
				nextCall++
				continue
			}
			if asBool(pm["thought"]) {
				reasoning.WriteString(asString(pm["text"]))
			} else {
				content.WriteString(asString(pm["text"]))
			}
		}
		finish = geminiToOpenAIStop[asString(cand["finishReason"])]
		if finish == "" {
			finish = "stop" // blocked/absent finishReason maps to a clean stop
		}
		if len(toolCalls) > 0 && finish == "stop" {
			finish = "tool_calls" // openai convention: function calls report their own stop reason
		}
	}
	msg := map[string]any{"role": "assistant"}
	if content.Len() > 0 {
		msg["content"] = content.String()
	}
	if reasoning.Len() > 0 {
		// house extension (deepseek/glm-style): reasoning exposed separately
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	choice := map[string]any{"index": 0, "message": msg, "finish_reason": finish}
	out := map[string]any{
		"id": asString(gem["responseId"]), "object": "chat.completion", "model": model,
		"choices": []any{choice},
	}
	if u := geminiUsageToChat(asMap(gem["usageMetadata"])); u != nil {
		out["usage"] = u
	}
	return out
}

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// geminiUsageToChat maps gemini usageMetadata into the openai-style usage map
// our accounting already parses. promptTokenCount is the total prompt
// (cached tokens included, same convention as openai prompt_tokens);
// thoughtsTokenCount is separate from candidatesTokenCount on the gemini wire
// but folds into completion_tokens (openai reasoning tokens are inclusive).
func geminiUsageToChat(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	in := asInt(u["promptTokenCount"])
	outT := asInt(u["candidatesTokenCount"]) + asInt(u["thoughtsTokenCount"])
	m := map[string]any{"prompt_tokens": in, "completion_tokens": outT, "total_tokens": in + outT}
	if c := asInt(u["cachedContentTokenCount"]); c > 0 {
		m["cache_read_tokens"] = c
		// openai-standard dialect — pi reads only this shape
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": c}
	}
	return m
}

// ---- non-stream response: chat -> gemini ----

var openAIToGeminiStop = map[string]string{
	"stop": "STOP", "length": "MAX_TOKENS", "content_filter": "SAFETY",
	"tool_calls": "STOP", // gemini reports function calls under STOP
}

// ChatRespToGemini converts an OpenAI chat-completion body to a gemini
// generateContent response (inverse of GeminiRespToChat, for inbound serving).
func ChatRespToGemini(chat map[string]any) map[string]any {
	out := map[string]any{"responseId": asString(chat["id"])}
	if chat["model"] != nil {
		out["modelVersion"] = chat["model"]
	}
	var parts []any
	finish := "STOP"
	if cs := asSlice(chat["choices"]); len(cs) > 0 {
		ch := asMap(cs[0])
		m := asMap(ch["message"])
		if r := asString(m["reasoning_content"]); r != "" {
			parts = append(parts, map[string]any{"text": r, "thought": true})
		}
		if t := openAIContentToText(m["content"]); t != "" {
			parts = append(parts, map[string]any{"text": t})
		}
		for _, tc := range asSlice(m["tool_calls"]) {
			fn := asMap(asMap(tc)["function"])
			parts = append(parts, map[string]any{"functionCall": map[string]any{
				"name": asString(fn["name"]), "args": jsonArgsToMap(asString(fn["arguments"])),
			}})
		}
		if s := openAIToGeminiStop[asString(ch["finish_reason"])]; s != "" {
			finish = s
		}
	}
	if parts == nil {
		parts = []any{} // gemini candidates always carry a parts array
	}
	out["candidates"] = []any{map[string]any{
		"content":      map[string]any{"role": "model", "parts": parts},
		"finishReason": finish,
	}}
	if u := chatUsageToGemini(asMap(chat["usage"])); u != nil {
		out["usageMetadata"] = u
	}
	return out
}

// chatUsageToGemini is the inverse of geminiUsageToChat. Openai chat usage
// cannot split thoughts from answer tokens, so thoughtsTokenCount stays 0.
func chatUsageToGemini(u map[string]any) map[string]any {
	if u == nil {
		return nil
	}
	in, outp := asInt(u["prompt_tokens"]), asInt(u["completion_tokens"])
	m := map[string]any{
		"promptTokenCount": in, "candidatesTokenCount": outp,
		"thoughtsTokenCount": 0, "totalTokenCount": in + outp,
	}
	cache := asInt(u["cache_read_tokens"])
	if d := asMap(u["prompt_tokens_details"]); d != nil {
		if c := asInt(d["cached_tokens"]); c > 0 {
			cache = c
		}
	}
	if cache > 0 {
		m["cachedContentTokenCount"] = cache
	}
	return m
}

// ---- stream: gemini SSE -> openai chat chunks ----

// Gemini2ChatStream converts gemini alt=sse data chunks into openai
// chat-completion chunks. Gemini SSE has no [DONE] sentinel — the stream
// simply ends at EOF — so the proxy calls Done after the reader exhausts;
// Done emits the terminal usage chunk (idempotent).
type Gemini2ChatStream struct {
	model           string
	started         bool
	finished        bool
	sawRole         bool
	finish          string
	usage           map[string]any
	nextTool        int // gemini streams functionCalls whole; one openai index per call
	in, out, cacheR int
}

func NewGemini2ChatStream(model string) *Gemini2ChatStream {
	return &Gemini2ChatStream{model: model}
}

// Usage returns captured token counts (thoughts folded into out).
func (t *Gemini2ChatStream) Usage() (in, out, cacheR int) { return t.in, t.out, t.cacheR }

func (t *Gemini2ChatStream) chunk(delta map[string]any, finish any) map[string]any {
	t.started = true
	return map[string]any{
		"id": "chatcmpl-ar-gemini", "object": "chat.completion.chunk", "model": t.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

// Chunk consumes one gemini SSE data chunk; returns openai chunks to forward.
func (t *Gemini2ChatStream) Chunk(data map[string]any) []map[string]any {
	var out []map[string]any
	if u := asMap(data["usageMetadata"]); u != nil {
		t.usage = geminiUsageToChat(u)
		if t.usage != nil {
			t.in = t.usage["prompt_tokens"].(int)
			t.out = t.usage["completion_tokens"].(int)
			t.cacheR, _ = t.usage["cache_read_tokens"].(int)
		}
	}
	var parts []any
	var finishGem string
	if cands := asSlice(data["candidates"]); len(cands) > 0 {
		cand := asMap(cands[0])
		parts = asSlice(asMap(asMap(cand)["content"])["parts"])
		finishGem = asString(cand["finishReason"])
	}
	for _, p := range parts {
		pm := asMap(p)
		if pm == nil {
			continue
		}
		if fc := asMap(pm["functionCall"]); fc != nil {
			// gemini streams each functionCall whole — one complete openai delta
			out = append(out, t.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": t.nextTool, "id": fmt.Sprintf("call_%d", t.nextTool), "type": "function",
				"function": map[string]any{
					"name": asString(fc["name"]), "arguments": jsonStr(fc["args"]),
				},
			}}}, nil))
			t.nextTool++
			continue
		}
		if txt := asString(pm["text"]); txt != "" {
			delta := map[string]any{}
			if !t.sawRole {
				delta["role"] = "assistant"
				t.sawRole = true
			}
			if asBool(pm["thought"]) {
				delta["reasoning_content"] = txt
			} else {
				delta["content"] = txt
			}
			out = append(out, t.chunk(delta, nil))
		}
	}
	if finishGem != "" {
		t.finish = geminiToOpenAIStop[finishGem]
		if t.finish == "" {
			t.finish = "stop"
		}
		out = append(out, t.chunk(map[string]any{}, t.finish))
	}
	return out
}

// Done emits the terminal chunks once the upstream stream has ended (EOF —
// gemini SSE carries no [DONE] sentinel). Emits a finish chunk first if the
// upstream ended without a finishReason, then the usage chunk.
func (t *Gemini2ChatStream) Done() []map[string]any {
	if !t.started || t.finished {
		return nil
	}
	t.finished = true
	var out []map[string]any
	if t.finish == "" {
		t.finish = "stop"
		out = append(out, t.chunk(map[string]any{}, t.finish))
	}
	if t.usage == nil {
		t.usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	final := t.chunk(map[string]any{}, nil)
	final["choices"] = []any{} // usage-only trailing chunk (openai dialect)
	if t.usage != nil {
		final["usage"] = t.usage
	}
	out = append(out, final)
	return out
}

// ---- stream: openai chat chunks -> gemini SSE data ----

// Chat2GemStream converts openai chat-completion chunks into gemini alt=sse
// data chunks for inbound gemini-native clients. Gemini chunks must carry
// whole functionCall parts with parseable args, so tool deltas are
// accumulated by tool index and flushed on Finish — chat's incremental
// argument streaming has no gemini carrier mid-stream. Gemini SSE has no
// [DONE] sentinel; Finish emits the final data chunk (finishReason +
// usageMetadata) and is idempotent.
type Chat2GemStream struct {
	model     string
	finished  bool
	finish    string
	usageIn   int
	usageOut  int
	cacheR    int
	tools     map[int]*chat2gemTool
	toolOrder []int
}

type chat2gemTool struct {
	name string
	args strings.Builder
}

func NewChat2GemStream(model string) *Chat2GemStream {
	return &Chat2GemStream{model: model, tools: map[int]*chat2gemTool{}}
}

// Chunk consumes one openai chat chunk; returns gemini data chunks to forward.
func (t *Chat2GemStream) Chunk(chunk map[string]any) []map[string]any {
	if u := asMap(chunk["usage"]); u != nil {
		t.usageIn = asInt(u["prompt_tokens"])
		t.usageOut = asInt(u["completion_tokens"])
		t.cacheR = asInt(u["cache_read_tokens"])
		if d := asMap(u["prompt_tokens_details"]); d != nil {
			if c := asInt(d["cached_tokens"]); c > 0 {
				t.cacheR = c
			}
		}
	}
	var delta map[string]any
	if cs := asSlice(chunk["choices"]); len(cs) > 0 {
		if fr := asString(asMap(cs[0])["finish_reason"]); fr != "" {
			t.finish = fr
		}
		delta = asMap(asMap(cs[0])["delta"])
	}
	if delta == nil {
		return nil
	}
	var parts []any
	if d := asString(delta["reasoning_content"]); d != "" {
		parts = append(parts, map[string]any{"text": d, "thought": true})
	}
	if d := asString(delta["content"]); d != "" {
		parts = append(parts, map[string]any{"text": d})
	}
	for _, tc := range asSlice(delta["tool_calls"]) {
		tcm := asMap(tc)
		idx := asInt(tcm["index"])
		tool, ok := t.tools[idx]
		if !ok {
			tool = &chat2gemTool{}
			t.tools[idx] = tool
			t.toolOrder = append(t.toolOrder, idx)
		}
		if fn := asMap(tcm["function"]); fn != nil {
			if n := asString(fn["name"]); n != "" {
				tool.name = n
			}
			tool.args.WriteString(asString(fn["arguments"]))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []map[string]any{map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": parts},
		}},
	}}
}

// Finish emits the final gemini data chunk once the chat stream has ended
// (after [DONE] / EOF). Carries the accumulated functionCall parts,
// finishReason, and usageMetadata. Idempotent.
func (t *Chat2GemStream) Finish() []map[string]any {
	if t.finished {
		return nil
	}
	t.finished = true
	// stable part order regardless of map iteration
	sort.Ints(t.toolOrder)
	var parts []any
	for _, idx := range t.toolOrder {
		tool := t.tools[idx]
		parts = append(parts, map[string]any{"functionCall": map[string]any{
			"name": tool.name, "args": jsonArgsToMap(tool.args.String()),
		}})
	}
	if parts == nil {
		parts = []any{}
	}
	finish := openAIToGeminiStop[t.finish]
	if finish == "" {
		finish = "STOP"
	}
	usage := map[string]any{
		"promptTokenCount": t.usageIn, "candidatesTokenCount": t.usageOut,
		"thoughtsTokenCount": 0, "totalTokenCount": t.usageIn + t.usageOut,
	}
	if t.cacheR > 0 {
		usage["cachedContentTokenCount"] = t.cacheR
	}
	return []map[string]any{map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": finish,
		}},
		"usageMetadata": usage,
		"modelVersion":  t.model,
	}}
}

// ---- errors ----

// GeminiErrMessage extracts the message from a gemini error body
// ({error:{message}}). Note the generic upstream-error path needs no new
// translator: gemini error bodies already carry an "error" object, so
// ErrToOpenAI/ErrToAnthropic pass them through verbatim. GeminiErrMessage
// exists for callers that want just the text.
func GeminiErrMessage(body []byte) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	if e := asMap(m["error"]); e != nil {
		if msg := asString(e["message"]); msg != "" {
			return msg, true
		}
	}
	return "", false
}

// ErrToGemini wraps an upstream error body into a gemini error envelope
// ({error:{code,message,status}}) for inbound gemini-native clients. A body
// already in the gemini shape (error.status carries the google.rpc code name)
// passes through verbatim; foreign-dialect envelopes (anthropic/openai also
// nest under "error") keep their message but are re-envelopeized.
func ErrToGemini(body []byte, status int) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if e := asMap(m["error"]); e != nil {
			if asString(e["status"]) != "" {
				return body // already gemini-shaped
			}
			if msg := asString(e["message"]); msg != "" {
				out, _ := json.Marshal(map[string]any{"error": map[string]any{
					"code": status, "message": msg, "status": GeminiStatusForHTTP(status),
				}})
				return out
			}
		}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("upstream error (http %d)", status)
	}
	out, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": GeminiStatusForHTTP(status),
	}})
	return out
}

// GeminiStatusForHTTP maps an http status to the google.rpc code name the
// gemini wire reports in error.status ({error:{...,status}}).
func GeminiStatusForHTTP(status int) string {
	switch status {
	case 400:
		return "INVALID_ARGUMENT"
	case 401, 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 503:
		return "UNAVAILABLE"
	}
	if status >= 500 {
		return "INTERNAL"
	}
	return "INTERNAL"
}
