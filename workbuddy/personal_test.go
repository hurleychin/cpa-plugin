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

// TestCheckinSchedule_Hours verifies the personal 09:00/21:00 tick helpers.
func TestCheckinSchedule_Hours(t *testing.T) {
	if len(checkinHours) != 2 || checkinHours[0] != 9 || checkinHours[1] != 21 {
		t.Fatalf("checkinHours = %v, want [9 21]", checkinHours)
	}
	if !checkinAutoEnabled() {
		t.Fatal("checkin_auto should default true")
	}
	// Next slot from 08:00 local should be 09:00 today.
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.Local)
	next := nextPersonalCheckinTime(now)
	if next.Hour() != 9 || !next.After(now) {
		t.Fatalf("next checkin from 08:00 = %v, want 09:00 today", next)
	}
	if !scheduledInCurrentHour(time.Date(2026, 8, 10, 9, 30, 0, 0, time.Local), checkinHours) {
		t.Fatal("09:30 should be in checkin hour")
	}
	if scheduledInCurrentHour(time.Date(2026, 8, 10, 10, 30, 0, 0, time.Local), checkinHours) {
		t.Fatal("10:30 should not be in checkin hour")
	}
}
