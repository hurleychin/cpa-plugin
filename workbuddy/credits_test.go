package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// newCheckinTestServer serves canned check-in status responses. When body is
// empty every check-in path returns 502 (simulated upstream outage); hits
// counts check-in endpoint calls when non-nil.
func newCheckinTestServer(body string, hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status", "/v2/billing/meter/checkin-status":
			if hits != nil {
				*hits++
			}
			w.Header().Set("Content-Type", "application/json")
			if body == "" {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("upstream blip"))
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestIsCreditsExhausted(t *testing.T) {
	cases := []struct {
		name string
		cr   *creditsSummary
		want bool
	}{
		{"nil", nil, false},
		{"remain>0", &creditsSummary{TotalRemain: 10}, false},
		{"remain0 used>0", &creditsSummary{TotalRemain: 0, TotalUsed: 5}, true},
		{"remain0 size>0", &creditsSummary{TotalRemain: 0, TotalSize: 100}, true},
		{"remain0 packages", &creditsSummary{TotalRemain: 0, Packages: []packageSummary{{Name: "x"}}}, true},
		{"remain0 no data", &creditsSummary{TotalRemain: 0, TotalUsed: 0}, false},
	}
	for _, tc := range cases {
		if got := isCreditsExhausted(tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestResolveCreditsCheckin_FreshForPersonalCN verifies the panel 已签到
// fix: a personal CN card gets a freshly fetched snapshot even when the
// cache holds none (e.g. pruned after the morning tick).
func TestResolveCreditsCheckin_FreshForPersonalCN(t *testing.T) {
	srv := newCheckinTestServer(`{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":true,"streak_days":3,"checkin_dates":["2026-09-24"],"activity_name":"高校新生攻略"}}`, nil)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	authID := "test-personal-cn"
	accountCache.Delete(authID)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}
	ci := resolveCreditsCheckin(authID, sa)
	if ci == nil || !ci.TodayCheckedIn {
		t.Fatalf("personal CN should get fresh checked-in snapshot, got %+v", ci)
	}
}

// TestResolveCreditsCheckin_FallsBackToCache ensures an upstream error keeps
// the last-known-good cached snapshot instead of blanking the panel.
func TestResolveCreditsCheckin_FallsBackToCache(t *testing.T) {
	srv := newCheckinTestServer("", nil)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	authID := "test-personal-cn-fallback"
	cached := &checkinSummary{Active: true, TodayCheckedIn: true, StreakDays: 2}
	accountCache.Store(authID, &accountCacheEntry{checkin: cached})
	defer accountCache.Delete(authID)
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1"},
	}
	if ci := resolveCreditsCheckin(authID, sa); ci != cached {
		t.Fatalf("should fall back to cached snapshot, got %+v", ci)
	}
}

// TestResolveCreditsCheckin_NoFetchForEnterprise ensures enterprise/Global
// accounts never hit the check-in endpoints from the credits path.
func TestResolveCreditsCheckin_NoFetchForEnterprise(t *testing.T) {
	var hits int
	srv := newCheckinTestServer("", &hits)
	defer srv.Close()
	restore := setBillingBase(srv.URL)
	defer restore()

	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", EnterpriseID: "ent1"},
	}
	if ci := resolveCreditsCheckin("test-ent", sa); ci != nil {
		t.Fatalf("enterprise should resolve nil checkin, got %+v", ci)
	}
	if hits != 0 {
		t.Fatalf("enterprise must not hit check-in endpoints, got %d hits", hits)
	}
}
