package proxy

import (
	"encoding/json"
	"strings"
)

// Usage captures token accounting from the stream or response body.
type Usage struct {
	In       int
	Out      int
	CacheR   int
	CacheW   int
	Estimate bool
	estBytes int
}

func (u *Usage) AddBytes(n int) { u.estBytes += n }

// Estimate returns a chars/4 fallback when the upstream never reported usage.
func (u Usage) Estimated() Usage {
	if u.In > 0 || u.Out > 0 {
		return u
	}
	return Usage{In: u.estBytes / 4, Out: 0, Estimate: true}
}

// UsageTee incrementally parses SSE lines out of a raw byte stream to extract
// usage without buffering or blocking the stream.
type UsageTee struct {
	wire  string
	line  []byte
	usage Usage
}

func NewUsageTee(wire string) *UsageTee {
	return &UsageTee{wire: wire}
}

func (t *UsageTee) Write(p []byte) {
	t.usage.AddBytes(len(p))
	for len(p) > 0 {
		i := indexByte(p, '\n')
		if i < 0 {
			t.line = append(t.line, p...)
			return
		}
		t.line = append(t.line, p[:i]...)
		t.consumeLine()
		t.line = t.line[:0]
		p = p[i+1:]
	}
}

func (t *UsageTee) consumeLine() {
	line := strings.TrimRight(string(t.line), "\r")
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	// cheap prefilter: only JSON-decode lines that can carry usage
	if !strings.Contains(data, "usage") && !strings.Contains(data, "message_start") {
		return
	}
	var m map[string]any
	if json.Unmarshal([]byte(data), &m) != nil {
		return
	}
	t.parse(m)
}

func (t *UsageTee) parse(m map[string]any) {
	if t.wire == "anthropic" {
		switch m["type"] {
		case "message_start":
			msg, _ := m["message"].(map[string]any)
			if u, ok := msg["usage"].(map[string]any); ok {
				t.usage.In = num(u["input_tokens"])
				t.usage.CacheR = num(u["cache_read_input_tokens"])
				t.usage.CacheW = num(u["cache_creation_input_tokens"])
			}
		case "message_delta":
			if u, ok := m["usage"].(map[string]any); ok {
				if v := num(u["output_tokens"]); v > 0 {
					t.usage.Out = v
				}
				if v := num(u["input_tokens"]); v > 0 {
					t.usage.In = v
				}
			}
		}
		return
	}
	// openai chunk
	if u, ok := m["usage"].(map[string]any); ok && u != nil {
		if v := num(u["prompt_tokens"]); v > 0 {
			t.usage.In = v
		}
		if v := num(u["completion_tokens"]); v > 0 {
			t.usage.Out = v
		}
		t.usage.CacheR = num(u["cache_read_tokens"])
		t.usage.CacheW = num(u["cache_write_tokens"])
	}
}

// Usage returns the parsed usage (with estimate fallback) — call after the
// stream ends.
func (t *UsageTee) Usage() Usage {
	u := t.usage
	if u.In == 0 && u.Out == 0 {
		e := u.Estimated()
		u.In, u.Estimate = e.In, true
	}
	return u
}

// ParseUsageJSON extracts usage from a full non-stream response body.
func ParseUsageJSON(wire string, body []byte) Usage {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return Usage{Estimate: true}
	}
	t := &UsageTee{wire: wire}
	t.parse(m)
	u := t.usage
	if u.In == 0 && u.Out == 0 {
		e := Usage{estBytes: len(body)}.Estimated()
		u = Usage{In: e.In, Estimate: true}
	}
	return u
}

func num(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	}
	return 0
}

func indexByte(p []byte, c byte) int {
	for i, b := range p {
		if b == c {
			return i
		}
	}
	return -1
}
