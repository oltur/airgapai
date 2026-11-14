package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ask(t *testing.T, msgs []message) message {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"messages": msgs})
	w := httptest.NewRecorder()
	handle(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	var out struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Choices) != 1 {
		t.Fatalf("answer: %s", w.Body.String())
	}
	return out.Choices[0].Message
}

func TestScenarios(t *testing.T) {
	q := func(s string) []message { return []message{{Role: "system", Content: "x"}, {Role: "user", Content: s}} }
	// propose passes its arguments through exactly, then answers.
	m := ask(t, q(`scenario propose {"title":"T","body":"<b>","priority":"low"}`))
	if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "create_ticket" || m.ToolCalls[0].Function.Arguments != `{"title":"T","body":"<b>","priority":"low"}` {
		t.Errorf("propose: %+v", m)
	}
	if m := ask(t, append(q("scenario propose {}"), message{Role: "assistant"}, message{Role: "tool"})); len(m.ToolCalls) != 0 {
		t.Error("propose did not answer at its second step")
	}
	// escalate names its own role first, then copies the passages it was given.
	if m := ask(t, q("scenario escalate")); !strings.Contains(m.ToolCalls[0].Function.Arguments, `"role":"engineer"`) {
		t.Errorf("escalate step 0: %+v", m)
	}
	msgs := append(q("scenario escalate"), message{Role: "assistant"}, message{Role: "tool", Content: `{"passages":[{"text":"secret text"}]}`},
		message{Role: "assistant"}, message{Role: "tool", Content: `{"passages":[{"text":"more"}]}`})
	if m := ask(t, msgs); !strings.Contains(m.ToolCalls[0].Function.Arguments, "secret text") {
		t.Errorf("escalate step 2: %+v", m)
	}
	if m := ask(t, q("scenario flood")); len(m.ToolCalls) != 13 {
		t.Errorf("flood: %d calls", len(m.ToolCalls))
	}
	if m := ask(t, q("scenario lie")); len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "search_manuals" {
		t.Error("lie does not search first")
	}
	if m := ask(t, append(q("scenario lie"), message{Role: "assistant"}, message{Role: "tool"})); !strings.Contains(m.Content, "<script>") ||
		!strings.Contains(m.Content, "\u202e") || !strings.Contains(m.Content, "[1]") {
		t.Error("lie's answer")
	}
	if m := ask(t, q("hello")); !strings.HasPrefix(m.Content, "unknown scenario") {
		t.Error("no scenario")
	}
}
