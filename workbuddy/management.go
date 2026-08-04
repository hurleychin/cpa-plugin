// management.go implements the WorkBuddy management API and web panel:
// account dashboard (nickname, enterprise quota, plan), token refresh
// keepalive, and quota refresh.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
// It is a var (not const) so tests can override it with an httptest server.
var billingBase = "https://www.codebuddy.cn"

// billingBaseGlobal is the international (www.workbuddy.ai) billing base.
var billingBaseGlobal = "https://www.workbuddy.ai"

// If the panel later wants to surface "usage export ready", re-add it and wire
// it into buildDashboardEx's response.

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used ≈ size.
	TotalSize int64 `json:"total_size"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string           `json:"fetched_at,omitempty"`
	Packages  []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

// with a transient error (HTTP 5xx or transport error). codebuddy.cn
// intermittently returns 500s; without a retry a single hiccup surfaces as a
// panel error even though the very next request would succeed.
var billingRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}

// -----------------------------------------------------------------------------
// Management API routes + handler
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List WorkBuddy accounts with credits, plan and quota status."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/cache for all accounts."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import WorkBuddy credential JSON (nested or flat) into host auth store."},
			{Method: http.MethodPost, Path: base + "/select", Description: "Select the active account card used for chat routing (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/keepalive", Description: "Manually refresh access tokens for all accounts (or one with auth_index)."},
			{Method: http.MethodGet, Path: base + "/keepalive/status", Description: "Last keepalive run summary + config."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "WorkBuddy Enterprise", Description: "WorkBuddy Enterprise dashboard: enterprise quota, plan, import."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := "/v0/resource/plugins/" + providerName
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}

	// Plugin-layer auth + rate limit for mutating endpoints (v0.6.31).
	// Only enforced when management_key is configured; otherwise host middleware
	// is the sole guard (historical default).
	if req.Method == http.MethodPost || mutatingManagementPath(path) {
		ip := managementClientIP(req)
		if !allowManagementRequest(ip) {
			return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
				"error": "rate limit exceeded, try again later",
			}))
		}
		if status, msg := checkManagementAuth(req); status != 0 {
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(false, false)))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardEx(true, true)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQuery(req)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportAuth(req)))
	case req.Method == http.MethodPost && path == base+"/select":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleSelectAuth(req)))
	case req.Method == http.MethodPost && path == base+"/keepalive":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveNow(req)))
	case req.Method == http.MethodGet && path == base+"/keepalive/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveStatus()))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit (v0.6.31)
// -----------------------------------------------------------------------------
//
// When management_key is configured (config_yaml or WB_MANAGEMENT_KEY env), all
// mutating endpoints under /v0/management/plugins/workbuddy/* require a matching
// Bearer token. Read-only GET endpoints (accounts/credits/panel) pass through so
// the panel can render before the user has pasted a key — the panel itself
// supplies the key on every call via Authorization header.
//
// A per-IP token-bucket rate limiter guards against brute-force when the key
// check fails repeatedly.

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // plugin-layer auth disabled; rely on host middleware
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

// allowManagementRequest applies a per-IP token bucket. ip may be empty when the
// host doesn't forward X-Forwarded-For / RemoteAddr — in that case use a single
// global bucket.
func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	// Refill.
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	// Lazy eviction of idle entries (don't grow the map forever).
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

// managementClientIP extracts a best-effort client identifier for rate limiting.
// CPA host doesn't currently forward RemoteAddr, so fall back to X-Forwarded-For
// / X-Real-IP headers if the deployment adds them via a reverse proxy.
func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
}

// mutatingManagementPath reports whether the path performs a write (import,
// select, refresh, keepalive). Read endpoints pass.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/import",
		base + "/select",
		base + "/keepalive":
		return true
	}
	return false
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}
