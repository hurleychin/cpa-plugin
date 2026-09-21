package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAccountRouting_PersonalVsEnterprise verifies the personal-edition merge
// routing contract: EnterpriseID accounts use the enterprise usage API only,
// personal accounts use the personal resource-package API.
func TestAccountRouting_PersonalVsEnterprise(t *testing.T) {
	if !isEnterpriseAccount(&storedAuth{Account: storedAccount{EnterpriseID: "ent1"}}) {
		t.Fatal("EnterpriseID account should be enterprise")
	}
	if isEnterpriseAccount(&storedAuth{Account: storedAccount{UID: "u1"}}) {
		t.Fatal("UID-only account should be personal")
	}
	if isEnterpriseAccount(&storedAuth{}) {
		t.Fatal("empty auth should be personal")
	}
	// Check-in eligibility: personal CN only.
	if !shouldFetchCheckin(&storedAuth{
		Auth:    storedTokens{Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}) {
		t.Fatal("personal CN should fetch checkin")
	}
	if shouldFetchCheckin(&storedAuth{
		Auth:    storedTokens{Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "ent1"},
	}) {
		t.Fatal("enterprise CN should not fetch checkin")
	}
	if shouldFetchCheckin(&storedAuth{
		Auth:    storedTokens{Domain: "www.workbuddy.ai"},
		Account: storedAccount{UID: "u1"},
	}) {
		t.Fatal("personal Global should not fetch checkin")
	}
}

// TestFetchUserResource_PersonalRoutes verifies a personal account hits the
// personal get-user-resource API (not the enterprise endpoint) and aggregates
// packages.
func TestFetchUserResource_PersonalRoutes(t *testing.T) {
	personalResp := `{"code":0,"msg":"OK","data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":100,"Accounts":[{"PackageName":"CodeBuddy个人体验版","CapacityRemain":60,"CapacityUsed":40,"CapacitySize":100,"CycleCapacityRemain":60,"CycleCapacityUsed":40,"CycleCapacitySize":100,"CycleStartTime":"2026-07-15 00:00:00","CycleEndTime":"2026-08-14 23:59:59"}]}}}}`
	var sawEnterprise, sawPersonal bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/billing/meter/get-enterprise-user-usage":
			sawEnterprise = true
		case "/v2/billing/meter/get-user-resource":
			sawPersonal = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(personalResp))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()
	restoreG := setBillingBaseGlobal(srv.URL)
	defer restoreG()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}
	cr, err := fetchUserResource(sa)
	if err != nil {
		t.Fatalf("fetchUserResource: %v", err)
	}
	if sawEnterprise {
		t.Fatal("personal account must not hit enterprise endpoint")
	}
	if !sawPersonal {
		t.Fatal("personal account should hit get-user-resource")
	}
	if cr.TotalRemain != 60 || cr.TotalUsed != 40 || cr.TotalSize != 100 {
		t.Fatalf("unexpected aggregate: %+v", cr)
	}
	if cr.PackCount != 1 {
		t.Fatalf("PackCount = %d, want 1", cr.PackCount)
	}
}

// TestHasTrialPack_Markers verifies trial detection without matching bare
// CN free-tier names.
func TestHasTrialPack_Markers(t *testing.T) {
	trial := &creditsSummary{Packages: []packageSummary{{Name: "CodeBuddy One-time Free 2-Week Pro Plan Trial"}}}
	if !hasTrialPack(trial) {
		t.Fatal("trial pack should be detected")
	}
	cnFree := &creditsSummary{Packages: []packageSummary{{Name: "CodeBuddy个人体验版"}}}
	if hasTrialPack(cnFree) {
		t.Fatal("CN 体验版 must not count as trial")
	}
	if hasTrialPack(nil) {
		t.Fatal("nil must not count as trial")
	}
}

// TestCheckinSchedule_Window verifies the personal daily random window:
// once per day inside 09:00~10:00 local, deterministic per-day target.
func TestCheckinSchedule_Window(t *testing.T) {
	if !checkinAutoEnabled() {
		t.Fatal("checkin_auto should default true")
	}
	loc := time.Local
	// Window boundaries.
	if !inPersonalCheckinWindow(time.Date(2026, 8, 10, 9, 30, 0, 0, loc)) {
		t.Fatal("09:30 should be in checkin window")
	}
	if inPersonalCheckinWindow(time.Date(2026, 8, 10, 8, 59, 0, 0, loc)) {
		t.Fatal("08:59 should not be in checkin window")
	}
	if inPersonalCheckinWindow(time.Date(2026, 8, 10, 10, 0, 0, 0, loc)) {
		t.Fatal("10:00 should not be in checkin window")
	}
	// Target lies inside the window and is stable for the same day.
	day := time.Date(2026, 8, 10, 0, 0, 0, 0, loc)
	t1, t2 := personalCheckinTarget(day), personalCheckinTarget(day)
	if !t1.Equal(t2) {
		t.Fatal("daily target should be deterministic per day")
	}
	start, end := personalCheckinWindow(day)
	if t1.Before(start) || !t1.Before(end) {
		t.Fatalf("target %v outside window [%v, %v)", t1, start, end)
	}
	// Different days (very likely) differ.
	if personalCheckinTarget(day).Equal(personalCheckinTarget(day.Add(24*time.Hour))) &&
		personalCheckinTarget(day).Equal(personalCheckinTarget(day.Add(48*time.Hour))) {
		t.Fatal("targets should vary across days")
	}
	// Before today's target → wake at target.
	now := start.Add(-time.Hour)
	if next := nextPersonalCheckinTime(now); !next.Equal(personalCheckinTarget(now)) {
		t.Fatalf("pre-window next = %v, want target %v", next, personalCheckinTarget(now))
	}
	// After tick recorded → tomorrow's target.
	markPersonalCheckinTick(now)
	defer func() {
		lastCheckinTickDayMu.Lock()
		lastCheckinTickDay = ""
		lastCheckinTickDayMu.Unlock()
	}()
	if next := nextPersonalCheckinTime(now); !next.Equal(personalCheckinTarget(now.Add(24 * time.Hour))) {
		t.Fatalf("post-tick next = %v, want tomorrow target", next)
	}
}
