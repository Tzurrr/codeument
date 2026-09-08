package credcache

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tzurrr/codeument/internal/secrets"
)

func open(t *testing.T, dir string) *Cache {
	t.Helper()
	c, err := Open(filepath.Join(dir, "credcache.db"), filepath.Join(dir, "credcache.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRoundTripAndEncryptionAtRest(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	c := open(t, dir)

	if err := c.Put(ctx, Entry{Host: "web-01", Username: "root", Password: "Winter2026!", Source: SourceReview, Notes: "console"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "web-01", "root")
	if err != nil || got.Password != "Winter2026!" || got.Source != SourceReview || got.Kind != "os" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if _, err := c.Get(ctx, "web-01", "nobody"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// The database file must not contain the plaintext.
	raw, err := os.ReadFile(filepath.Join(dir, "credcache.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Winter2026!") {
		t.Fatal("password stored in plaintext")
	}
	for _, p := range []string{"credcache.db", "credcache.key"} {
		st, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable by others (%o)", p, st.Mode().Perm())
		}
	}

	// Re-opening with the same key file decrypts; a different key does not.
	c2 := open(t, dir)
	if got, err := c2.Get(ctx, "web-01", "root"); err != nil || got.Password != "Winter2026!" {
		t.Fatalf("reopen: %v %+v", err, got)
	}
	other := t.TempDir()
	if err := os.Rename(filepath.Join(dir, "credcache.db"), filepath.Join(other, "credcache.db")); err != nil {
		t.Fatal(err)
	}
	c3 := open(t, other) // fresh key file
	if _, err := c3.Get(ctx, "web-01", "root"); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("expected a decryption error with a different key, got %v", err)
	}
}

func TestUpsertSuggestionsPushAndForget(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	c := open(t, dir)

	if err := c.Put(ctx, Entry{Host: "db-01", Username: "postgres", Password: "old", Source: SourceReview}); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, Entry{Host: "db-01", Username: "postgres", Password: "new", Source: SourceReview}); err != nil {
		t.Fatal(err)
	}
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 || list[0].Password != "new" {
		t.Fatalf("upsert: %v %+v", err, list)
	}

	if err := c.Put(ctx, Entry{Host: "db-01", Username: "deploy", Password: "seen", Source: SourceCommand, Program: "chpasswd"}); err != nil {
		t.Fatal(err)
	}
	sugg, err := c.Suggestions(ctx, "db-01")
	if err != nil || len(sugg) != 1 || sugg[0].Program != "chpasswd" {
		t.Fatalf("suggestions: %v %+v", err, sugg)
	}

	entry, _ := c.Get(ctx, "db-01", "postgres")
	if err := c.MarkPushed(ctx, entry.ID, secrets.Reference{Mode: secrets.ModeManager, Ref: "vault://secret/x", URL: "https://vault/x", Password: "must not persist"}); err != nil {
		t.Fatal(err)
	}
	entry, _ = c.Get(ctx, "db-01", "postgres")
	if entry.PushedAt == nil || entry.Reference.Ref != "vault://secret/x" || entry.Reference.Password != "" {
		t.Fatalf("pushed: %+v", entry)
	}
	if cr := entry.Credential("machine-1"); cr.MachineID != "machine-1" || cr.Password != "new" || cr.Host != "db-01" {
		t.Fatalf("credential: %+v", cr)
	}

	n, err := c.Forget(ctx, "db-01", "deploy")
	if err != nil || n != 1 {
		t.Fatalf("forget one: %d %v", n, err)
	}
	n, err = c.Forget(ctx, "", "")
	if err != nil || n != 1 {
		t.Fatalf("forget all: %d %v", n, err)
	}
	if list, _ := c.List(ctx); len(list) != 0 {
		t.Fatalf("cache should be empty: %+v", list)
	}
}

func TestRejectsLooseKeyPermissions(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	_ = c.Close()
	keyPath := filepath.Join(dir, "credcache.key")
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(dir, "credcache.db"), keyPath); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("expected a permission error, got %v", err)
	}
}

func TestPutValidates(t *testing.T) {
	c := open(t, t.TempDir())
	ctx := context.Background()
	if err := c.Put(ctx, Entry{Username: "root", Password: "x"}); err == nil {
		t.Fatal("host is required")
	}
	if err := c.Put(ctx, Entry{Host: "h", Username: "root"}); err == nil {
		t.Fatal("password is required")
	}
}
