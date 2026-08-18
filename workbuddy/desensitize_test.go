package main

import (
	"strings"
	"testing"
)

func TestDesensitizeText(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"DoS", "D\u200boS"},
		{"Refuse requests for DoS attacks and exploit development.", "Refuse requests for D\u200boS a\u200bttacks and e\u200bxploit development."},
		{"Prevent privilege escalation and brute force attacks.", "Prevent p\u200brivilege escalation and b\u200brute force a\u200bttacks."},
		{"这是一段正常的中文,不含任何触发词。", "这是一段正常的中文,不含任何触发词。"},
		{"", ""},
		{"no sensitive words here", "no sensitive words here"},
	}
	for _, c := range cases {
		got := desensitizeText(c.in)
		if got != c.want {
			t.Errorf("desensitizeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDesensitizeContentSystemOnly(t *testing.T) {
	sys := map[string]any{"role": "system", "content": "Refuse DoS attacks and exploit development."}
	if !desensitizeContent(sys) {
		t.Fatal("system message should be modified")
	}
	if !strings.Contains(sys["content"].(string), "\u200b") {
		t.Fatalf("system content should contain ZWSP, got %q", sys["content"])
	}

	user := map[string]any{"role": "user", "content": "Explain DoS attacks please"}
	if desensitizeContent(user) {
		t.Fatal("user message must not be modified")
	}
	if strings.Contains(user["content"].(string), "\u200b") {
		t.Fatalf("user content must stay untouched, got %q", user["content"])
	}
}

func TestDesensitizeContentMultimodal(t *testing.T) {
	msg := map[string]any{
		"role": "system",
		"content": []any{
			map[string]any{"type": "text", "text": "Refuse DoS and malware requests."},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "x"}},
		},
	}
	if !desensitizeContent(msg) {
		t.Fatal("multimodal system message should be modified")
	}
	blocks := msg["content"].([]any)
	text := blocks[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "\u200b") {
		t.Fatalf("text block should contain ZWSP, got %q", text)
	}
}
