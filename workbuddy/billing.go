// billing.go owns the upstream billing API surface: the enterprise usage
// query (get-enterprise-user-usage), payment type, and the shared JSON
// helpers used to tolerate the upstream's loosely-typed response shapes,
// plus the region helpers that decide CN vs Global endpoint.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// isGlobalDomain reports whether the domain belongs to the international
// (www.workbuddy.ai) WorkBuddy service.  The CN service uses
// www.codebuddy.cn; Global uses www.workbuddy.ai.
func isGlobalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// accountRegion returns "cn" or "global" based on the auth's domain field.
// Empty domain (legacy auth files) defaults to "cn" for backward compat.
func accountRegion(sa *storedAuth) string {
	if sa != nil && isGlobalDomain(sa.Auth.Domain) {
		return "global"
	}
	return "cn"
}

// setBillingBase temporarily overrides billingBase for tests; returns a
// restore func.
func setBillingBase(s string) func() {
	old := billingBase
	billingBase = s
	return func() { billingBase = old }
}

// setBillingBaseGlobal temporarily overrides billingBaseGlobal for tests.
func setBillingBaseGlobal(s string) func() {
	old := billingBaseGlobal
	billingBaseGlobal = s
	return func() { billingBaseGlobal = old }
}

// billingBaseFor returns the billing API base URL for the given auth's domain.
// CN accounts → https://www.codebuddy.cn; Global → https://www.workbuddy.ai.
// Falls back to the test-overridable billingBase for CN/nil.
func billingBaseFor(sa *storedAuth) string {
	if sa != nil && isGlobalDomain(sa.Auth.Domain) {
		return billingBaseGlobal
	}
	return billingBase
}

// -----------------------------------------------------------------------------
// Billing / usage API calls
// -----------------------------------------------------------------------------

func billingHeaders(req *http.Request, sa *storedAuth) {
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", sa.Account.EnterpriseID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
	// The enterprise usage endpoints identify the client as the web console
	// (same origin/referer the codebuddy.cn billing console sends).
	req.Header.Set("X-Client-Platform", "web")
	origin := originRefererFor(sa)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
}

func billingCall(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	data, err := billingCallOnce(sa, path, body)
	for _, d := range billingRetryDelays {
		if err == nil || !isTransientBillingErr(err) {
			break
		}
		time.Sleep(d)
		data, err = billingCallOnce(sa, path, body)
	}
	return data, err
}

// isTransientBillingErr reports whether err came from an upstream 5xx or a
// transport failure (both retryable). 4xx and business-code errors are not.
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "http 5") || strings.HasPrefix(msg, "http=5") || strings.Contains(msg, "status 5")
}

func billingCallOnce(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader([]byte("{}"))
	}
	base := billingBaseFor(sa)
	req, err := http.NewRequest(http.MethodPost, base+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	// Route via host.http.do so request-log captures the call (v0.8.1 compliance:
	// was sharedHTTPClient().Do — bypassed host transport policy + logging).
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	// Upstream 5xx is transient — classify it so billingCall can retry,
	// and keep a redacted response body snippet for diagnosis (A-42).
	if resp.StatusCode >= 500 {
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// parse failed usually means upstream returned a non-JSON error page
		// (e.g. APISIX 401 HTML for session-dead). Include a redacted snippet
		// so the panel / logs can surface the real cause instead of a bare
		// "parse failed" (P0-2 UX: was impossible to distinguish session dead
		// from a malformed response).
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, snippet)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	// Enterprise usage endpoint (tencent CodeBuddy billing console "web" client).
	// POST body is empty; the response carries the enterprise quota snapshot:
	//   data.credit         — credits USED in the cycle (已用积分)
	//   data.limitNum       — cycle quota (total)
	//   data.cycleStartTime / cycleEndTime
	//   data.cycleResetTime
	data, err := billingCall(sa, "/billing/meter/get-enterprise-user-usage", nil)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	used := creditToInt64(jsonF64(m, "credit"))
	if used < 0 {
		used = 0
	}
	size := jsonI64(m, "limitNum")
	if size < 0 {
		size = 0
	}
	remain := size - used
	if remain < 0 {
		remain = 0
	}
	sum := &creditsSummary{
		TotalRemain: remain,
		TotalUsed:   used,
		TotalSize:   size,
		PackCount:   1,
		Packages: []packageSummary{{
			Name:       "企业版",
			Remain:     remain,
			Used:       used,
			Size:       size,
			CycleStart: jsonStr(m, "cycleStartTime"),
			CycleEnd:   jsonStr(m, "cycleEndTime"),
		}},
	}
	return sum, nil
}

func fetchPaymentType(sa *storedAuth) string {
	data, err := billingCall(sa, "/v2/billing/meter/get-payment-type", nil)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	if s, ok := m["paymentType"].(string); ok {
		return s
	}
	return ""
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a cycle total
	// (used>0, size>0, or a package present). Pure zero with no data = unknown.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}

func jsonBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case float64:
				return t != 0
			case string:
				return t == "true" || t == "1"
			}
		}
	}
	return false
}

func jsonI64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case string:
				var n int64
				fmt.Sscanf(t, "%d", &n)
				return n
			}
		}
	}
	return 0
}

func jsonStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}

// jsonF64 reads a JSON number as float64, tolerating int/float/string shapes.
func jsonF64(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case int64:
				return float64(t)
			case json.Number:
				f, _ := t.Float64()
				return f
			case string:
				var f float64
				fmt.Sscanf(t, "%g", &f)
				return f
			}
		}
	}
	return 0
}

// creditToInt64 converts a float credit value to an int64, rounding to the
// nearest integer (credit can carry decimals like 3291.96).
func creditToInt64(credit float64) int64 {
	if credit <= 0 {
		return 0
	}
	return int64(credit + 0.5)
}
