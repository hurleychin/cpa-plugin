package main

import (
	"net/http"
	"testing"
)

func TestNewChatSessionNoSession(t *testing.T) {
	h := http.Header{}
	sess := newChatSession(h)
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
	sess := newChatSession(h)
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
	a := newChatSession(h)
	b := newChatSession(h)
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
	if newChatSession(h1).id == newChatSession(h2).id {
		t.Fatal("different sessions must map to different conversation ids")
	}
}

func TestSha256UUIDShape(t *testing.T) {
	h := http.Header{}
	h.Set("X-Session-ID", "shape-test")
	id := newChatSession(h).id
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
	backendHeaders(req, &storedAuth{}, newChatSession(http.Header{}))
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want application/json", got)
	}
}
