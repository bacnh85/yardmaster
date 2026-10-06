package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The start-plan credits plane: data.balances[] with snake_case fields (camel
// aliases accepted upstream too — bareNum tolerates both via RawMessage).
func TestFetchZaiStartQuota(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer plan-jwt" {
			t.Errorf("Authorization = %q, want Bearer plan-jwt", auth)
		}
		w.Write([]byte(`{"code":0,"data":{"balances":[
			{"show_name":"Weekend package","remaining_units":70000000,"total_units":100000000,"used_units":30000000,"expires_at":1767225600000},
			{"show_name":"empty row","remaining_units":0,"total_units":0,"used_units":0}
		]}}`))
	}))
	defer stub.Close()

	acct, err := fetchZaiStartQuota(context.Background(), stub.URL, "plan-jwt")
	if err != nil {
		t.Fatal(err)
	}
	if acct.Monthly == nil {
		t.Fatal("credits window missing")
	}
	w := acct.Monthly
	if w.Used != 30000000 || w.Cap != 100000000 {
		t.Fatalf("credits = %v/%v, want 30000000/100000000", w.Used, w.Cap)
	}
	if w.ResetAt != 1767225600000 {
		t.Fatalf("expires_at = %v", w.ResetAt)
	}
	if w.Exceeded {
		t.Fatal("not exceeded")
	}
	if w.Unit != "units" {
		t.Fatalf("grant units must carry unit=units, got %q", w.Unit)
	}

	// exhausted bucket + percentage unit
	stub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{"balances":[{"remaining_units":0,"total_units":500,"used_units":500,"unit_type":"percentage"}]}}`))
	}))
	defer stub2.Close()
	acct2, err := fetchZaiStartQuota(context.Background(), stub2.URL, "plan-jwt")
	if err != nil {
		t.Fatal(err)
	}
	if !acct2.Monthly.Exceeded || acct2.Monthly.Unit != "pct" {
		t.Fatalf("exhausted pct bucket: %+v", acct2.Monthly)
	}

	// upstream error surfaces as account error
	stub3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":3012,"msg":"re-login"}`))
	}))
	defer stub3.Close()
	_, err = fetchZaiStartQuota(context.Background(), stub3.URL, "plan-jwt")
	if err == nil {
		t.Fatal("401 must surface as error")
	}
}

func TestBareNum(t *testing.T) {
	for raw, want := range map[string]float64{
		`100`:    100,
		`"2500"`: 2500,
		`0`:      0,
		`1.5e7`:  15000000,
		`"abc"`:  0,
		`null`:   0,
	} {
		if got := bareNum([]byte(raw)); got != want {
			t.Errorf("bareNum(%s) = %v, want %v", raw, got, want)
		}
	}
}
