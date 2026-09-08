package tui

import (
	"strings"
	"testing"

	"github.com/Tzurrr/codeument/internal/model"
)

func TestEditableRoundTrip(t *testing.T) {
	d := model.DraftRecord{ID: "d1", Title: "Rotate certs", BodyMD: "Did the thing.\n\n## Steps\n\n1. certbot\n", Location: model.Location{Space: "OPS", ParentPath: []string{"Runbooks"}}, Tags: []string{"tls"}, Draft: model.Draft{DocKind: "runbook"}}
	text := EditableMarkdown(d)
	if !strings.HasPrefix(text, "---\ntitle: Rotate certs\n") || !strings.Contains(text, "## Steps") {
		t.Fatalf("editable:\n%s", text)
	}
	edited := strings.Replace(text, "title: Rotate certs", "title: Rotate TLS certs", 1)
	edited = strings.Replace(edited, "1. certbot", "1. certbot renew", 1)
	edited = strings.Replace(edited, "  - tls", "  - tls\n  - nginx", 1)
	if err := ApplyEditedMarkdown(&d, edited); err != nil {
		t.Fatal(err)
	}
	if d.Title != "Rotate TLS certs" || !strings.Contains(d.BodyMD, "certbot renew") || len(d.Tags) != 2 || d.Location.Space != "OPS" {
		t.Fatalf("applied = %+v", d)
	}
	if err := ApplyEditedMarkdown(&d, "---\ntitle: ''\n---\nbody"); err == nil {
		t.Fatal("expected error for empty title")
	}
	if err := ApplyEditedMarkdown(&d, "plain body without header\n"); err != nil || d.BodyMD != "plain body without header\n" {
		t.Fatalf("plain body: %v %q", err, d.BodyMD)
	}
}

func TestWrap(t *testing.T) {
	got := wrap("one two three four five six seven eight nine ten", 20)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 20 {
			t.Fatalf("line too long: %q", line)
		}
	}
	if code := wrap("```\nvery long line inside a code block that should not wrap at all\n```", 20); strings.Count(code, "\n") != 2 {
		t.Fatalf("code block wrapped: %q", code)
	}
}
