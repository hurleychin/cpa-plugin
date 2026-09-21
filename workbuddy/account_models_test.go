package main

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func promoLoc() *time.Location { return time.FixedZone("CST", 8*3600) }

func TestParseHM(t *testing.T) {
	if m, ok := parseHM("23:00"); !ok || m != 1380 {
		t.Fatalf("23:00 = %d,%v", m, ok)
	}
	if m, ok := parseHM("7:50"); !ok || m != 470 {
		t.Fatalf("7:50 = %d,%v", m, ok)
	}
	if _, ok := parseHM("24:00"); ok {
		t.Fatal("24:00 should be invalid")
	}
	if _, ok := parseHM("xx"); ok {
		t.Fatal("xx should be invalid")
	}
}

func nightPromo() v3Promo {
	return v3Promo{
		ID: "glm-52-night-discount-202607", Enabled: true,
		Schedule: v3Schedule{
			Timezone: "Asia/Shanghai",
			Daily: []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			}{{Start: "23:00", End: "7:50"}},
		},
	}
}

func TestPromoActive_OvernightWindow(t *testing.T) {
	p := nightPromo()
	loc := promoLoc()
	if !promoActive(p, time.Date(2026, 9, 21, 2, 0, 0, 0, loc)) {
		t.Fatal("02:00 should be active in 23:00-7:50 window")
	}
	if !promoActive(p, time.Date(2026, 9, 21, 23, 30, 0, 0, loc)) {
		t.Fatal("23:30 should be active in 23:00-7:50 window")
	}
	if promoActive(p, time.Date(2026, 9, 21, 12, 0, 0, 0, loc)) {
		t.Fatal("12:00 should not be active in 23:00-7:50 window")
	}
	p.Enabled = false
	if promoActive(p, time.Date(2026, 9, 21, 2, 0, 0, 0, loc)) {
		t.Fatal("disabled promo should not be active")
	}
}

func TestPromoActive_ValidRange(t *testing.T) {
	p := nightPromo()
	p.Schedule.ValidFrom = "2026-07-06T00:00:00+08:00"
	p.Schedule.ValidUntil = "2026-10-01T00:00:00+08:00"
	loc := promoLoc()
	if !promoActive(p, time.Date(2026, 9, 21, 2, 0, 0, 0, loc)) {
		t.Fatal("should be active inside valid range")
	}
	if promoActive(p, time.Date(2026, 10, 5, 2, 0, 0, 0, loc)) {
		t.Fatal("should not be active past validUntil")
	}
}

func TestPromoPeriod(t *testing.T) {
	p := nightPromo()
	if got := promoPeriod(p); got != "每日23:00–7:50" {
		t.Fatalf("period = %q", got)
	}
	p.Schedule.ValidFrom = "2026-07-06T00:00:00+08:00"
	p.Schedule.ValidUntil = "2026-10-01T00:00:00+08:00"
	if got := promoPeriod(p); got != "每日23:00–7:50（7月6日–9月30日）" {
		t.Fatalf("period = %q", got)
	}
}

func TestHandleAccountModels_RequiresAuthIndex(t *testing.T) {
	out := handleAccountModels(pluginapi.ManagementRequest{})
	if out["error"] == nil {
		t.Fatal("missing auth_index should error")
	}
}
