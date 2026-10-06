package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The login flow mirrors zcode-api's CliOAuthClient: init → authorize_url with
// the desktop interstitial param, poll → pending/ready. These tests stub the
// upstream zcode.z.ai endpoints and pin the admin-surface contract.
func zaiLoginStub(t *testing.T, initBody, pollBody string, pollStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("init missing Bearer poll token")
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("init content-type = %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(initBody))
	})
	mux.HandleFunc("/api/v1/oauth/cli/poll/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(pollStatus)
		w.Write([]byte(pollBody))
	})
	return httptest.NewServer(mux)
}

func TestZaiLoginStartAndPollReady(t *testing.T) {
	stub := zaiLoginStub(t,
		`{"code":0,"data":{"flow_id":"flow-1","authorize_url":"https://chat.z.ai/oauth/authorize?x=1","expires_at":9999999999,"poll_interval_sec":2}}`,
		`{"code":0,"data":{"status":"ready","token":"plan-jwt","zai":{"access_token":"coding-key"}}}`, 200)
	defer stub.Close()
	restore := replaceZaiLoginOrigin(stub.URL)
	defer restore()

	// start
	req := httptest.NewRequest("POST", "/admin/api/zai/login", nil)
	rec := httptest.NewRecorder()
	(&Server{}).handleZaiLoginStart(rec, req)
	if rec.Code != 200 {
		t.Fatalf("start: HTTP %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"flow_id":"flow-1"`) ||
		!strings.Contains(body, "redirect_uri="+url.QueryEscape("https://zcode.z.ai/app/oauth/login?redirect=zcode%3A%2F%2Foauth%2Fcallback&app_version=3.14.0")) {
		t.Fatalf("start body missing flow_id/interstitial replace: %s", body)
	}

	// poll: ready consumes the flow
	req = httptest.NewRequest("GET", "/admin/api/zai/login/poll?flow_id=flow-1", nil)
	rec = httptest.NewRecorder()
	(&Server{}).handleZaiLoginPoll(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"status":"ready"`) || !strings.Contains(rec.Body.String(), `"jwt":"plan-jwt"`) {
		t.Fatalf("poll ready: HTTP %d body=%s", rec.Code, rec.Body.String())
	}
	// flow consumed → 404 on next poll
	rec = httptest.NewRecorder()
	(&Server{}).handleZaiLoginPoll(rec, req)
	if rec.Code != 404 {
		t.Fatalf("consumed flow should 404, got %d", rec.Code)
	}
}

func TestZaiLoginPollPendingAndFailed(t *testing.T) {
	stub := zaiLoginStub(t,
		`{"code":0,"data":{"flow_id":"flow-2","authorize_url":"https://x/y","expires_at":9999999999,"poll_interval_sec":1}}`,
		`{"code":0,"data":{"status":"pending"}}`, 200)
	defer stub.Close()
	restore := replaceZaiLoginOrigin(stub.URL)
	defer restore()

	req := httptest.NewRequest("POST", "/admin/api/zai/login", nil)
	rec := httptest.NewRecorder()
	(&Server{}).handleZaiLoginStart(rec, req)
	if rec.Code != 200 {
		t.Fatalf("start: HTTP %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	(&Server{}).handleZaiLoginPoll(rec, httptest.NewRequest("GET", "/admin/api/zai/login/poll?flow_id=flow-2", nil))
	if !strings.Contains(rec.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending poll: %s", rec.Body.String())
	}

	// upstream fatal envelope → failed status (not a 5xx)
	stub2 := zaiLoginStub(t, ``, `{"code":7,"msg":"denied"}`, 200)
	defer stub2.Close()
	restore2 := replaceZaiLoginOrigin(stub2.URL)
	defer restore2()
	rec = httptest.NewRecorder()
	(&Server{}).handleZaiLoginPoll(rec, httptest.NewRequest("GET", "/admin/api/zai/login/poll?flow_id=flow-2", nil))
	if !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("failed poll: %s", rec.Body.String())
	}
}

func TestZaiLoginUnknownFlow(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handleZaiLoginPoll(rec, httptest.NewRequest("GET", "/admin/api/zai/login/poll?flow_id=nope", nil))
	if rec.Code != 404 {
		t.Fatalf("unknown flow should 404, got %d", rec.Code)
	}
}
