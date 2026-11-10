package approval

import (
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	a, err := Canonical([]byte(`{"title": "Pump", "priority":"high", "body":"<b>&</b> 1.50"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical([]byte("{\n \"body\":\"<b>&</b> 1.50\",\"priority\" : \"high\",\"title\":\"Pump\"}"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != `{"body":"<b>&</b> 1.50","priority":"high","title":"Pump"}` {
		t.Fatalf("got %s and %s", a, b)
	}
	if n, _ := Canonical([]byte(`{"n":1.50}`)); n != `{"n":1.50}` {
		t.Errorf("numbers must stay as written: %s", n)
	}
	for _, bad := range []string{
		`{"title":"a","title":"b"}`,
		`{"x":{"y":1,"y":2}}`,
		`{"x":[{"y":1,"y":2}]}`,
		`{"title":"a"} {"title":"b"}`,
		`["not", "an", "object"]`,
		`"text"`,
		``,
	} {
		if got, err := Canonical([]byte(bad)); err == nil {
			t.Errorf("%q was accepted as %q", bad, got)
		}
	}
	if Hash(a) == Hash(a+" ") || len(Hash(a)) != 64 {
		t.Error("hash")
	}
}

func TestCreateTicketValidation(t *testing.T) {
	ok := map[string]any{"title": "Pump 3 leaks", "body": "Seen at the seal.", "priority": "high"}
	if err := CreateTicket.Validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"extra field":     func(m map[string]any) { m["assignee"] = "admin" },
		"empty title":     func(m map[string]any) { m["title"] = "  " },
		"two-line title":  func(m map[string]any) { m["title"] = "a\nb" },
		"long title":      func(m map[string]any) { m["title"] = strings.Repeat("x", 121) },
		"long body":       func(m map[string]any) { m["body"] = strings.Repeat("x", 4001) },
		"missing body":    func(m map[string]any) { delete(m, "body") },
		"bad priority":    func(m map[string]any) { m["priority"] = "urgent" },
		"number priority": func(m map[string]any) { m["priority"] = 1 },
	} {
		m := map[string]any{}
		for k, v := range ok {
			m[k] = v
		}
		change(m)
		if err := CreateTicket.Validate(m); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
