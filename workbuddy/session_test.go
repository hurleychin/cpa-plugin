package main

import (
	"net/http"
	"testing"
)

func TestNewChatSessionNoSession(t *testing.T) {
	h := http.Header{}
	sess := newChatSession(h, nil)
	if sess.purpose != "conversation_topic" {
		t.Fatalf("purpose = %q, want conversation_topic", sess.purpose)
	}
	if sess.id != "" {
		t.Fatalf("id = %q, want empty", sess.id)
	}
	if sess.parentSpanID != "" {
		t.Fatalf("parentSpanID = %q, want empty", sess.parentSpanID)
	}
	if sess.codebuddyReq {
		t.Fatal("codebuddyReq = true, want false")
	}
}

func TestNewChatSessionWithSession(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "sess-abc-123")
	sess := newChatSession(h, nil)
	if sess.purpose != "conversation" {
		t.Fatalf("purpose = %q, want conversation", sess.purpose)
	}
	if sess.id == "" {
		t.Fatal("id empty, want derived UUID")
	}
	if !sess.codebuddyReq {
		t.Fatal("codebuddyReq = false, want true")
	}
	if sess.parentSpanID == "" {
		t.Fatal("parentSpanID empty, want derived span")
	}
}

func TestNewChatSessionStable(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-ID", "stable-session-1")
	a := newChatSession(h, nil)
	b := newChatSession(h, nil)
	if a.id != b.id {
		t.Fatalf("conversation id not stable: %q vs %q", a.id, b.id)
	}
	if a.parentSpanID != b.parentSpanID {
		t.Fatalf("parent span not stable: %q vs %q", a.parentSpanID, b.parentSpanID)
	}
}

func TestNewChatSessionDifferentSessions(t *testing.T) {
	h1 := http.Header{}
	h1.Set("X-Claude-Code-Session-Id", "sess-A")
	h2 := http.Header{}
	h2.Set("X-Claude-Code-Session-Id", "sess-B")
	if newChatSession(h1, nil).id == newChatSession(h2, nil).id {
		t.Fatal("different sessions must map to different conversation ids")
	}
}

func TestSha256UUIDShape(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-ID", "shape-test")
	id := newChatSession(h, nil).id
	// Expect the canonical 8-4-4-4-12 UUID shape with v4/variant bits.
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("conversation id %q not UUID-shaped", id)
	}
	if id[14] != '4' {
		t.Fatalf("conversation id %q missing version-4 nibble", id)
	}
	if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Fatalf("conversation id %q missing variant nibble", id)
	}
}

func TestBackendHeadersAccept(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	backendHeaders(req, &storedAuth{}, newChatSession(http.Header{}, nil))
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}

func TestSessionIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"session_id", `{"session_id":"body-sess-1"}`, "body-sess-1"},
		{"sessionId", `{"sessionId":"body-sess-2"}`, "body-sess-2"},
		{"prompt_cache_key", `{"prompt_cache_key":"cache-key-3"}`, "cache-key-3"},
		{"conversation_id", `{"conversation_id":"conv-4"}`, "conv-4"},
		{"conversation.id", `{"conversation":{"id":"conv-5"}}`, "conv-5"},
		{"empty", `{}`, ""},
		{"invalid json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sessionIDFromBody([]byte(c.body)); got != c.want {
				t.Fatalf("sessionIDFromBody = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAllSessionCandidatesPriority(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "claude-sess")
	h.Set("Session-Id", "codex-sess")
	// Header wins over body regardless of order.
	if got := allSessionCandidates(h, []byte(`{"session_id":"body-sess"}`)); got != "claude-sess" {
		t.Fatalf("priority = %q, want claude-sess", got)
	}
	// No headers, body only.
	if got := allSessionCandidates(http.Header{}, []byte(`{"session_id":"body-sess"}`)); got != "body-sess" {
		t.Fatalf("body fallback = %q, want body-sess", got)
	}
	// Codex underscore variant recognized.
	codex := http.Header{}
	codex.Set("Session_id", "codex-underscore")
	if got := allSessionCandidates(codex, nil); got != "codex-underscore" {
		t.Fatalf("Session_id = %q, want codex-underscore", got)
	}
}

func TestFilterTraceHeadersRedactsSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret-token")
	h.Set("X-Enterprise-Token", "tok-abc")
	h.Set("B3", "abc-123-1-456")
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("X-User-Id", "u-1")
	out := filterTraceHeaders(h)
	if out["Authorization"][0] == "Bearer secret-token" {
		t.Fatal("Authorization value leaked into trace")
	}
	if out["Authorization"][0] != "<redacted>" {
		t.Fatalf("Authorization = %v, want <redacted>", out["Authorization"])
	}
	if out["X-Enterprise-Token"][0] != "<redacted>" {
		t.Fatalf("X-Enterprise-Token = %v, want <redacted>", out["X-Enterprise-Token"])
	}
	if out["B3"][0] != "abc-123-1-456" {
		t.Fatalf("B3 = %v, want full value kept", out["B3"])
	}
	if out["X-Agent-Purpose"][0] != "conversation" {
		t.Fatalf("X-Agent-Purpose = %v", out["X-Agent-Purpose"])
	}
	if out["X-User-Id"][0] != "u-1" {
		t.Fatalf("X-User-Id = %v, want non-secret header kept in full", out["X-User-Id"])
	}
}
