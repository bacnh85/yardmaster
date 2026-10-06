package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

// The start-plan gateway rejects bodies whose system field lacks the ZCode
// identity blocks (biz 3012). These tests pin the exact wire shape zcode-api
// ships: 3 official blocks first (ephemeral), client blocks after (stripped),
// currentDate reminder as leading user turn, exactly 4 cache breakpoints.
func TestInjectStartPlanSystem(t *testing.T) {
	req := map[string]any{
		"model": "glm-5.3-flash",
		"system": []any{
			map[string]any{"type": "text", "text": "You are pi, a coding agent", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"tools": []any{
			map[string]any{"name": "read", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
		},
	}
	InjectStartPlanSystem(req, "glm-5.3-flash")

	sys, _ := req["system"].([]any)
	if len(sys) != 4 { // 3 official + 1 client block
		t.Fatalf("system blocks = %d, want 4 (3 official + 1 client)", len(sys))
	}
	first, _ := sys[0].(map[string]any)
	if first["text"] != "You are ZCode, an interactive coding agent" {
		t.Fatalf("block 1 = cli_prefix, got %q", first["text"])
	}
	for i, b := range sys {
		m := b.(map[string]any)
		cc, ok := m["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			// client block must be stripped, official must carry ephemeral
			if i < 3 {
				t.Fatalf("official block %d missing ephemeral marker", i)
			}
		} else if i == 3 {
			t.Fatalf("client block 3 must have cache_control stripped")
		}
	}
	third, _ := sys[2].(map[string]any)
	txt := third["text"].(string)
	if !strings.HasPrefix(txt, "\n\n") {
		t.Fatalf("dynamic block must start with \\n\\n")
	}
	if !strings.Contains(txt, "- You are powered by the model named zai-api/glm-5.3-flash.") {
		t.Fatalf("powered-by line missing: %q", txt)
	}

	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (context prefix + user)", len(msgs))
	}
	prefix := msgs[0].(map[string]any)
	if prefix["role"] != "user" {
		t.Fatalf("context prefix must be a user turn")
	}
	pc := prefix["content"].([]any)[0].(map[string]any)
	if !strings.Contains(pc["text"].(string), "<system-reminder>") ||
		!strings.Contains(pc["text"].(string), "# currentDate\nToday's date is ") ||
		!strings.HasSuffix(pc["text"].(string), "</system-reminder>") {
		t.Fatalf("context prefix shape wrong: %q", pc["text"])
	}

	tools, _ := req["tools"].([]any)
	if cc := tools[0].(map[string]any)["cache_control"]; cc != nil {
		t.Fatalf("tool cache_control must be stripped, got %v", cc)
	}
	// last message (the original user turn) carries the 4th marker
	last := msgs[1].(map[string]any)
	lb := last["content"].([]any)[0].(map[string]any)
	if lb["cache_control"] == nil {
		t.Fatalf("last message must carry the 4th ephemeral marker")
	}
	// breakpoint budget: exactly 4 across the whole body
	n := 0
	for _, b := range sys {
		if b.(map[string]any)["cache_control"] != nil {
			n++
		}
	}
	if lb["cache_control"] != nil {
		n++
	}
	if n != 4 {
		t.Fatalf("cache breakpoints = %d, want 4", n)
	}
	_, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("body not marshalable: %v", err)
	}
}

func TestInjectStartPlanSystemStringSystem(t *testing.T) {
	req := map[string]any{
		"model":    "glm-5.3-flash",
		"system":   "plain client system",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	InjectStartPlanSystem(req, "glm-5.3-flash")
	sys, _ := req["system"].([]any)
	if len(sys) != 4 {
		t.Fatalf("system blocks = %d, want 4 (3 official + string→block)", len(sys))
	}
	if got := sys[3].(map[string]any)["text"]; got != "plain client system" {
		t.Fatalf("client system preserved after official, got %q", got)
	}
}
