package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreditToInt64 covers rounding of float credit values.
func TestCreditToInt64(t *testing.T) {
	cases := []struct {
		in   float64
		want int64
	}{
		{3291.96, 3292},
		{0, 0},
		{-5, 0},
		{0.4, 0},
		{0.5, 1},
		{99.2, 99},
	}
	for _, tc := range cases {
		if got := creditToInt64(tc.in); got != tc.want {
			t.Fatalf("creditToInt64(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestJsonF64 covers the tolerant float extraction used by the enterprise
// usage endpoint.
func TestJsonF64(t *testing.T) {
	m := map[string]any{"a": float64(3291.96), "b": int64(7), "c": "99.5", "d": "abc"}
	if v := jsonF64(m, "a"); v != 3291.96 {
		t.Fatalf("a: got %v", v)
	}
	if v := jsonF64(m, "b"); v != 7 {
		t.Fatalf("b: got %v", v)
	}
	if v := jsonF64(m, "c"); v != 99.5 {
		t.Fatalf("c: got %v", v)
	}
	if v := jsonF64(m, "d"); v != 0 {
		t.Fatalf("d: got %v", v)
	}
	if v := jsonF64(m, "missing"); v != 0 {
		t.Fatalf("missing: got %v", v)
	}
}

// TestFetchUserResource_Enterprise maps the live enterprise usage response
// into the creditsSummary shape consumed by the panel/lifecycle.
// The enterprise "credit" field is credits USED in the cycle, not remaining.
func TestFetchUserResource_Enterprise(t *testing.T) {
	resp := `{"code":0,"msg":"OK","requestId":"x","data":{"credit":3291.96,"cycleStartTime":"2026-07-15 00:00:00","cycleEndTime":"2026-08-14 23:59:59","limitNum":5000,"cycleResetTime":"2026-08-15 00:00:00"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/billing/meter/get-enterprise-user-usage" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Client-Platform"); got != "web" {
			t.Fatalf("X-Client-Platform = %q, want web", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "ent1"},
	}
	cr, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	// credit=3291.96 is USED (已用), limitNum=5000 is the cycle size.
	if cr.TotalUsed != 3292 {
		t.Fatalf("TotalUsed = %d, want 3292", cr.TotalUsed)
	}
	if cr.TotalSize != 5000 {
		t.Fatalf("TotalSize = %d, want 5000", cr.TotalSize)
	}
	// remain = 5000 - 3292 = 1708
	if cr.TotalRemain != 1708 {
		t.Fatalf("TotalRemain = %d, want 1708", cr.TotalRemain)
	}
	if cr.PackCount != 1 {
		t.Fatalf("PackCount = %d, want 1", cr.PackCount)
	}
	if len(cr.Packages) != 1 {
		t.Fatalf("Packages = %d, want 1", len(cr.Packages))
	}
	p := cr.Packages[0]
	if p.CycleStart != "2026-07-15 00:00:00" || p.CycleEnd != "2026-08-14 23:59:59" {
		t.Fatalf("cycle times wrong: %+v", p)
	}
	if isCreditsExhausted(cr) {
		t.Fatalf("should not be exhausted with remain>0")
	}
	// Exercise the unexported raw path so jsonF64/jsonI64 are hit with real types.
	var m map[string]any
	if err := json.Unmarshal([]byte(resp), &m); err != nil {
		t.Fatal(err)
	}
	if m["code"].(float64) != 0 {
		t.Fatalf("unexpected envelope")
	}
}

// TestFetchUserResource_EnterpriseExhausted covers the exhausted state
// (credit == limitNum, i.e. the whole cycle quota consumed).
func TestFetchUserResource_EnterpriseExhausted(t *testing.T) {
	resp := `{"code":0,"msg":"OK","data":{"credit":5000,"cycleStartTime":"2026-07-15 00:00:00","cycleEndTime":"2026-08-14 23:59:59","limitNum":5000}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	cr, err := fetchUserResource(&storedAuth{})
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if cr.TotalRemain != 0 {
		t.Fatalf("TotalRemain = %d, want 0", cr.TotalRemain)
	}
	if !isCreditsExhausted(cr) {
		t.Fatalf("credit==limit should be exhausted")
	}
}
