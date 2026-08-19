package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Trace configuration (plugins.configs.workbuddy.trace_enabled / trace_dir in
// CPA config). Trace is independent of CPA's own request-log: the plugin writes
// its own JSONL file, so CPA debug/request-log need not be enabled for it to
// work.
var (
	traceEnabledMu sync.RWMutex
	traceEnabled   bool
	traceDirMu     sync.RWMutex
	traceDir       string
)

// traceDefaultDir is used when trace_dir is empty.
const traceDefaultDir = "/root/cliproxyapi/logs/workbuddy-trace"

// applyTraceConfig parses trace_enabled / trace_dir from the plugin config YAML.
// Called from configure(); does not hold any other lock.
func applyTraceConfig(lines []string) {
	nextEnabled := false
	nextDir := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "trace_enabled:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "trace_enabled:")), "\"'")
			nextEnabled = v == "true" || v == "1" || v == "yes" || v == "on"
		}
		if strings.HasPrefix(line, "trace_dir:") {
			v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "trace_dir:")), "\"'")
			if v != "" {
				nextDir = v
			}
		}
	}
	traceEnabledMu.Lock()
	traceEnabled = nextEnabled
	traceEnabledMu.Unlock()
	traceDirMu.Lock()
	if nextDir != "" {
		traceDir = nextDir
	} else {
		traceDir = traceDefaultDir
	}
	traceDirMu.Unlock()
}

// traceOn reports whether session tracing is enabled.
func traceOn() bool {
	traceEnabledMu.RLock()
	defer traceEnabledMu.RUnlock()
	return traceEnabled
}

// traceLine is one JSONL record describing how a chat request was correlated
// to an upstream session, plus the outbound headers actually sent.
type traceLine struct {
	Timestamp     string              `json:"ts"`
	Model         string              `json:"model"`
	SessionHeader string              `json:"session_header"`
	BodySession   string              `json:"body_session"`
	Purpose       string              `json:"purpose"`
	ConvID        string              `json:"conv_id"`
	ParentSpan    string              `json:"parent_span"`
	CodebuddyReq  bool                `json:"codebuddy_req"`
	Headers       map[string][]string `json:"headers,omitempty"`
}

// writeSessionTrace appends one trace record for a chat request. It is a no-op
// when tracing is disabled. The file is written plugin-side so it works without
// CPA request-log or debug enabled.
func writeSessionTrace(line traceLine) {
	if !traceOn() {
		return
	}
	traceDirMu.RLock()
	dir := traceDir
	traceDirMu.RUnlock()
	path := dir + "/session-trace.jsonl"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	b, err := json.Marshal(line)
	if err != nil || len(b) == 0 {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

// sessionIDFromBody extracts a session identifier from the request body for
// clients that carry their session there instead of in a header (OpenAI
// session_id, Responses prompt_cache_key, conversation_id). Mirrors the host's
// own extractors so the derived identity agrees with session affinity.
func sessionIDFromBody(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return ""
	}
	for _, key := range []string{"session_id", "sessionId", "prompt_cache_key"} {
		if v, ok := obj[key].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	if v, ok := obj["conversation_id"].(string); ok {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	if c, ok := obj["conversation"].(map[string]any); ok {
		if v, ok := c["id"].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// filterTraceHeaders snapshots the outbound headers for tracing. All headers
// are kept so nothing is lost while debugging; only credential-bearing values
// (Authorization, Cookie, and any header whose name signals a key/token/secret)
// are redacted, keeping the key visible but masking the value.
func filterTraceHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		if isSecretHeader(k) {
			out[k] = []string{"<redacted>"}
			continue
		}
		out[k] = vs
	}
	return out
}

// isSecretHeader reports whether a header name carries a credential whose value
// must never be written to the trace file.
func isSecretHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
		"X-Api-Key", "Api-Key":
		return true
	}
	l := strings.ToLower(name)
	for _, marker := range []string{"token", "secret", "apikey", "api-key", "credential", "password"} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

// allSessionCandidates returns the session identifier available from headers
// and body in host-compatible priority order (first non-empty wins).
func allSessionCandidates(headers http.Header, payload []byte) string {
	for _, header := range []string{
		"X-Claude-Code-Session-Id",
		"Session-Id",
		"Session_id",
		"X-Session-ID",
		"X-Session-Affinity",
		"X-Client-Request-Id",
	} {
		if sid := strings.TrimSpace(headers.Get(header)); sid != "" {
			return sid
		}
	}
	if sid := sessionIDFromBody(payload); sid != "" {
		return sid
	}
	return ""
}
