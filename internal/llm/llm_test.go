package llm

import "testing"

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`: `{"a":1}`,
		"text before {\"a\":{\"b\":\"}\"}} after": `{"a":{"b":"}"}}`,
		"```json\n{\"x\":[1,2]}\n```":             `{"x":[1,2]}`,
	}
	for in, want := range cases {
		got, err := ExtractJSON(in)
		if err != nil || string(got) != want {
			t.Errorf("ExtractJSON(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := ExtractJSON("no json here"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ExtractJSON(`{"unterminated": `); err == nil {
		t.Fatal("expected error")
	}
}
