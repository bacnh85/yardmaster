package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/translate"
)

// ProbeResult is the playground response for one one-shot request.
type ProbeResult struct {
	Text    string  `json:"text"`
	TokIn   int     `json:"tok_in"`
	TokOut  int     `json:"tok_out"`
	CostUSD float64 `json:"cost_usd"`
	TTFTms  float64 `json:"ttft_ms"`
	DurMs   float64 `json:"dur_ms"`
}

// Probe sends a one-shot non-streaming chat request to one provider (first
// static key) and returns the assistant text + usage. OpenAI chat wire is the
// hub: buildUpstream translates it to the provider's wire.
func (p *Proxy) Probe(ctx context.Context, name, model, prompt string, maxTokens int) (ProbeResult, error) {
	var pv *config.Provider
	for _, x := range p.Reg.Config().Providers {
		if x.Name == name {
			pv = x
			break
		}
	}
	if pv == nil {
		return ProbeResult{}, fmt.Errorf("no provider %q", name)
	}
	if pv.Auth.Type == "oauth" || len(pv.Auth.Keys) == 0 {
		return ProbeResult{}, fmt.Errorf("provider %q has no static API key", name)
	}
	if maxTokens <= 0 || maxTokens > 4096 {
		maxTokens = 256
	}
	upModel := provider.UpstreamModel(pv, model)
	req := map[string]any{
		"model": upModel, "stream": false, "max_tokens": maxTokens,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}
	tgt := &provider.Target{Provider: pv, APIKey: pv.Auth.Keys[0], AuthType: "static"}
	httpReq, err := p.buildUpstream(ctx, tgt, WireOpenAI, upModel, req, nil, true, "")
	if err != nil {
		return ProbeResult{}, err
	}
	start := time.Now()
	resp, err := p.do(tgt, httpReq)
	if err != nil {
		return ProbeResult{}, err
	}
	defer resp.Body.Close()
	ttft := time.Since(start) // headers received = first byte for non-streaming
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ProbeResult{}, err
	}
	if resp.StatusCode >= 400 {
		snippet := string(body)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return ProbeResult{}, fmt.Errorf("http %d: %s", resp.StatusCode, snippet)
	}
	text, usage := probeParse(pv.Wire, body)
	out := ProbeResult{
		Text: text, TokIn: usage.In, TokOut: usage.Out,
		TTFTms: float64(ttft.Milliseconds()), DurMs: float64(time.Since(start).Milliseconds()),
	}
	if p.Cost != nil {
		c := p.Cost(model)
		out.CostUSD = float64(usage.In)/1e6*c.Input + float64(usage.Out)/1e6*c.Output
	}
	return out, nil
}

// probeParse extracts assistant text + usage from a non-streaming upstream
// body in the provider's wire shape.
func probeParse(wire string, body []byte) (string, Usage) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", Usage{}
	}
	switch wire {
	case WireAnthropic:
		var sb strings.Builder
		for _, c := range asAnySlice(m["content"]) {
			if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
				if s, ok := cm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String(), ParseUsageJSON(wire, body)
	case WireResponses:
		chat := translate.ResponsesRespToChat(m)
		b, _ := json.Marshal(chat)
		return probeOpenAIText(chat), ParseUsageJSON(WireOpenAI, b)
	default:
		return probeOpenAIText(m), ParseUsageJSON(wire, body)
	}
}

func probeOpenAIText(m map[string]any) string {
	choices, _ := m["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	switch t := msg["content"].(type) {
	case string:
		return t
	case []any: // content parts
		var sb strings.Builder
		for _, p := range t {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					sb.WriteString(s)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func asAnySlice(v any) []any {
	s, _ := v.([]any)
	return s
}
