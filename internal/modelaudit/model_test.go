package modelaudit

import (
	"net/http"
	"testing"
)

func TestEvidenceVerdicts(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"matching", `{"response":{"model":"astra"}}`, "consistent"},
		{"different", `{"model":"luna"}`, "mismatch"},
		{"no evidence", `{"output":[{"text":"I am luna","model":"luna"}]}`, "unknown"},
		{"slug", `{"message":{"metadata":{"model_slug":"luna"}}}`, "mismatch"},
		{"default only", `{"message":{"metadata":{"default_model_slug":"astra"}}}`, "unknown"},
		{"invalid", `not json`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := New("astra", "astra")
			d.JSON([]byte(tc.body), "sse", "response.created", 1, 23)
			r := d.Result()
			if r.Verdict != tc.want || r.ActualVerified {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestConflictAndHints(t *testing.T) {
	d := New("astra", "astra")
	d.Headers(http.Header{"X-Model": []string{"luna"}}, "header", 0, 0)
	if d.Result().Verdict != "unknown" {
		t.Fatal("header incorrectly promoted")
	}
	d.JSON([]byte(`{"model":"astra"}`), "sse", "response.created", 1, 1)
	d.JSON([]byte(`{"model":"luna"}`), "sse", "response.completed", 2, 2)
	if d.Result().Verdict != "conflict" {
		t.Fatal(d.Result())
	}
}
func TestNoAliasGuessingAndBounds(t *testing.T) {
	d := New("astra", "astra")
	d.JSON([]byte(`{"model":"astra-2026-09-01"}`), "json", "", 0, 0)
	if d.Result().Verdict != "mismatch" {
		t.Fatal("guessed aliases")
	}
	for i := 0; i < 1000; i++ {
		d.JSON([]byte(`{"model":"astra-2026-09-01"}`), "json", "", i, 0)
	}
	if len(d.Result().Evidence) != 1 {
		t.Fatal("duplicate growth")
	}
	if RequestModel([]byte(`{"model":"astra","input":"secret"}`)) != "astra" {
		t.Fatal("request model")
	}
}
