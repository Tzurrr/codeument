package markdown

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/model"
)

func TestUpsertFindGet(t *testing.T) {
	root := t.TempDir()
	p, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	doc := docs.Document{ID: "doc-1", Title: "Rotate nginx certs", BodyMD: "## Steps\n\n1. run certbot\n", Location: model.Location{Space: "OPS", ParentPath: []string{"Runbooks", "Web"}}, Labels: []string{"nginx"}, Meta: map[string]string{"host": "web-01"}}
	ref, err := p.Upsert(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("ops", "runbooks", "web", "rotate-nginx-certs.md")
	if ref.ProviderID != want || ref.Version != 1 {
		t.Fatalf("ref = %+v", ref)
	}
	data, _ := os.ReadFile(filepath.Join(root, want))
	if !strings.Contains(string(data), "codeument_id: doc-1") || !strings.Contains(string(data), "# Rotate nginx certs") {
		t.Fatalf("content:\n%s", data)
	}

	found, err := p.Find(ctx, "doc-1")
	if err != nil || found == nil || found.ProviderID != want {
		t.Fatalf("find: %v %+v", err, found)
	}
	if none, err := p.Find(ctx, "nope"); err != nil || none != nil {
		t.Fatalf("find missing: %v %+v", err, none)
	}

	// Update keeps the path and bumps the version, even if the title changed.
	doc.Title = "Rotate nginx certificates"
	doc.BodyMD = "## Steps\n\n1. run certbot renew\n"
	ref2, err := p.Upsert(ctx, doc)
	if err != nil || ref2.ProviderID != want || ref2.Version != 2 {
		t.Fatalf("update: %v %+v", err, ref2)
	}
	got, err := p.Get(ctx, ref2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Rotate nginx certificates" || got.BodyMD != "## Steps\n\n1. run certbot renew\n" || got.Meta["host"] != "web-01" || got.Location.Space != "ops" {
		t.Fatalf("get = %+v", got)
	}

	// Index loss is recovered by scanning front matter.
	_ = os.Remove(p.indexPath())
	found, err = p.Find(ctx, "doc-1")
	if err != nil || found == nil || found.Version != 2 {
		t.Fatalf("find after index loss: %v %+v", err, found)
	}

	// Same title, different id -> new file with suffix.
	ref3, err := p.Upsert(ctx, docs.Document{ID: "doc-2", Title: "Rotate nginx certs", BodyMD: "x", Location: doc.Location})
	if err != nil || ref3.ProviderID != filepath.Join("ops", "runbooks", "web", "rotate-nginx-certs-2.md") {
		t.Fatalf("collision: %v %+v", err, ref3)
	}
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := p.List()
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
}

func TestSlug(t *testing.T) {
	if s := docs.Slug("Hello, World! 2026/09"); s != "hello-world-2026-09" {
		t.Fatal(s)
	}
	if s := docs.Slug("   "); s != "untitled" {
		t.Fatal(s)
	}
}
