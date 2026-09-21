package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/store"
)

// The zai preset one-click create must land every coding-plan quota lever in
// the stored config: cache-control injection, adaptive thinking, dispatch
// spacing, fast-mode header/body, zcode signing, curated models.
func TestProviderFormZaiFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: \":0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: ":0",
		Keys:   []*config.Key{{Key: "ar-agent", Name: "pi", Allow: []string{"*"}}},
		Providers: []*config.Provider{{Name: "seed", BaseURL: "https://up.test", Wire: "openai",
			Auth: config.AuthConf{Type: "static", Keys: []string{"sk"}}}},
	}
	cfg.Defaults()
	reg := provider.New(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	srv := New(p, auth.NewKeyStore(cfg.Keys), st, cfgPath, "secretpw", "test")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	admin := func(method, path string, body *string) (int, []byte) {
		var rd *strings.Reader
		if body != nil {
			rd = strings.NewReader(*body)
		} else {
			rd = strings.NewReader("")
		}
		req, _ := http.NewRequest(method, ts.URL+"/admin/api/"+path, rd)
		req.SetBasicAuth("", "secretpw")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 0, 8192)
		tmp := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		return resp.StatusCode, buf
	}

	form := `{"name":"zai","wire":"anthropic","base_url":"https://api.z.ai/api/anthropic","models":["glm-5.3","glm-5.3-flash"],` +
		`"keys":["id.secret"],"prefix":"zai","dispatch_interval_ms":1000,` +
		`"adaptive_thinking":true,"inject_cache_control":true,"zcode_signing":true,` +
		`"extra_headers":{"anthropic-beta":"fast-mode-2026-02-01"},"body_overrides":{"speed":"fast"}}`
	if code, b := admin("POST", "providers", &form); code != 200 {
		t.Fatalf("create zai provider: %d %s", code, b)
	}

	// GET /admin/api/providers carries the flags (UI round-trip + prefill)
	code, b := admin("GET", "providers", nil)
	if code != 200 {
		t.Fatalf("list: %d", code)
	}
	for _, want := range []string{`"zcode_signing":true`, `"inject_cache_control":true`, `"anthropic-beta":"fast-mode-2026-02-01"`, `"speed":"fast"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("providers dump missing %s:\n%s", want, b)
		}
	}

	// PUT omitting extra_headers/body_overrides keeps the stored ones
	putBody := `{"name":"zai","wire":"anthropic","base_url":"https://api.z.ai/api/anthropic","models":["glm-5.3","glm-5.3-flash"],` +
		`"keys":["id.secret"],"dispatch_interval_ms":1000,"adaptive_thinking":true,"inject_cache_control":true,"zcode_signing":true}`
	if code, b := admin("PUT", "providers/zai", &putBody); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	_, b = admin("GET", "providers", nil)
	if !strings.Contains(string(b), `"anthropic-beta"`) || !strings.Contains(string(b), `"speed":"fast"`) {
		t.Fatalf("omitted maps must keep stored values:\n%s", b)
	}

	// PUT omitting the advanced booleans/int keeps every stored value — row
	// actions (visibility toggles, rotation) must never silently disable the
	// coding-plan tricks
	sparseBody := `{"name":"zai","wire":"anthropic","base_url":"https://api.z.ai/api/anthropic","models":["glm-5.3","glm-5.3-flash"],"keys":["id.secret"]}`
	if code, b := admin("PUT", "providers/zai", &sparseBody); code != 200 {
		t.Fatalf("sparse put: %d %s", code, b)
	}
	_, b = admin("GET", "providers", nil)
	for _, want := range []string{
		`"zcode_signing":true`, `"adaptive_thinking":true`, `"inject_cache_control":true`,
		`"dispatch_interval_ms":1000`, `"anthropic-beta"`, `"speed":"fast"`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("sparse PUT lost %s:\n%s", want, b)
		}
	}

	// on disk: zcode_signing + body_overrides persisted
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zcode_signing: true", "speed: fast", "glm-5.3-flash"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("config file missing %q:\n%s", want, raw)
		}
	}
}

// fetchZaiQuota parses both observed schemas; windows map to 5h/weekly with
// percent units; PAYG keys have no windows.
func TestFetchZaiQuota(t *testing.T) {
	orig := quotaClient
	t.Cleanup(func() { quotaClient = orig })

	cases := []struct {
		name     string
		body     string
		fiveHour bool
		weekly   bool
		pct5     float64
	}{
		{
			name:     "standard schema (max/pro): TOKENS_LIMIT 5h + TIME_LIMIT MCP monthly",
			body:     `{"data":{"level":"max","windows":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42.5,"nextResetTime":1790000000000},{"type":"TIME_LIMIT","unit":5,"number":1,"usage":{"currentValue":10,"remaining":90,"percentage":10}}]}}`,
			fiveHour: true, pct5: 42.5,
		},
		{
			name:     "live schema: limits[] spelling (lite plan, 2026-09-21)",
			body:     `{"code":200,"msg":"Operation successful","success":true,"data":{"level":"lite","limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":100,"percentage":46,"nextResetTime":1791430620983},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":57,"nextResetTime":1789971404619}]}}`,
			fiveHour: true, pct5: 57,
		},
		{
			name:     "credit-only schema (lite): CREDIT_LIMIT 5h + weekly",
			body:     `{"data":{"level":"lite","windows":[{"type":"CREDIT_LIMIT","unit":3,"number":5,"percentage":7,"nextResetTime":1790000000000},{"type":"CREDIT_LIMIT","unit":6,"number":1,"percentage":13,"nextResetTime":1790500000000}]}}`,
			fiveHour: true, weekly: true, pct5: 7,
		},
		{
			name: "PAYG key: no windows",
			body: `{"data":{"level":"","windows":[]}}`,
		},
		{
			name:     "usage.percentage fallback",
			body:     `{"data":{"level":"pro","windows":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"nextResetTime":1790000000000,"usage":{"percentage":61}}]}}`,
			fiveHour: true, pct5: 61,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				fmt.Fprint(w, tc.body)
			}))
			defer up.Close()
			quotaClient = up.Client()
			acct, err := fetchZaiQuota(context.Background(), up.URL, "id.secret")
			if err != nil {
				t.Fatal(err)
			}
			if gotAuth != "id.secret" {
				t.Fatalf("auth header: %q", gotAuth)
			}
			if tc.fiveHour {
				if acct.FiveHour == nil || acct.FiveHour.Used != tc.pct5 || acct.FiveHour.Unit != "pct" || acct.FiveHour.Cap != 100 {
					t.Fatalf("five_hour: %+v", acct.FiveHour)
				}
			} else if acct.FiveHour != nil {
				t.Fatalf("unexpected five_hour: %+v", acct.FiveHour)
			}
			if tc.weekly != (acct.Weekly != nil) {
				t.Fatalf("weekly mismatch: %+v", acct.Weekly)
			}
			if !tc.fiveHour && !tc.weekly && (acct.FiveHour != nil || acct.Weekly != nil) {
				t.Fatal("PAYG key must yield no windows")
			}
		})
	}
	_ = json.Marshal // keep import when cases change
}

// quotaSource must match both z.ai hosts; buildQuotaReport fetches the monitor
// endpoint from api.z.ai even for ultra-route providers.
func TestZaiQuotaSource(t *testing.T) {
	if got := quotaSource("https://api.z.ai/api/anthropic"); got != "zai" {
		t.Fatalf("api.z.ai: %q", got)
	}
	if got := quotaSource("https://zcode.z.ai/api/v1/ultra-zai/anthropic"); got != "zai" {
		t.Fatalf("zcode.z.ai: %q", got)
	}
	if got := quotaSource("https://open.bigmodel.cn/api/anthropic"); got != "" {
		t.Fatalf("bigmodel must not match: %q", got)
	}
}
