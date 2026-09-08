package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRecord(t *testing.T) {
	raw := "cdm1\x00git commit -m x\x001\x001700000000.123456\x001700000000.5\x00/srv/app\x00zsh\x00host-1-2-3\x004242\x00"
	rec, err := ParseRecordBytes([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Command != "git commit -m x" || rec.ExitCode != 1 || rec.CWD != "/srv/app" || rec.Shell != "zsh" || rec.SessionID != "host-1-2-3" || rec.PID != 4242 {
		t.Fatalf("parsed = %+v", rec)
	}
	if d := rec.End.Sub(rec.Start).Milliseconds(); d < 376 || d > 377 {
		t.Fatalf("duration = %dms", d)
	}
	if _, err := ParseRecordBytes([]byte("junk")); err == nil {
		t.Fatal("expected error for malformed record")
	}
	// Comma decimal separator.
	rec, err = ParseRecordBytes([]byte("cdm1\x00ls\x000\x001700000000,5\x001700000001,0\x00/\x00bash\x00s\x001\x00"))
	if err != nil || rec.End.Sub(rec.Start).Milliseconds() != 500 {
		t.Fatalf("comma epoch: %v %+v", err, rec)
	}
}

func TestRenderHook(t *testing.T) {
	for _, sh := range Shells {
		out, err := RenderHook(sh, HookOptions{Binary: "/usr/local/bin/codeument", NotifyFile: "/tmp/n", SeenDir: "/tmp/seen"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "/usr/local/bin/codeument") || strings.Contains(out, "[[") {
			t.Fatalf("%s hook not rendered: %s", sh, out)
		}
	}
	if _, err := RenderHook("csh", HookOptions{}); err == nil {
		t.Fatal("expected error for unsupported shell")
	}
	if _, err := RenderHook("bash", HookOptions{Binary: "/bad'path"}); err == nil {
		t.Fatal("expected error for unquotable path")
	}
}

func TestLookupGit(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, ".git", "refs", "heads"), 0o755))
	must(os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, ".git", "refs", "heads", "main"), []byte("abc123\n"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "sub", "dir"), 0o755))

	info := LookupGit(filepath.Join(root, "sub", "dir"))
	if info.Root != root || info.Branch != "main" || info.Head != "abc123" {
		t.Fatalf("info = %+v", info)
	}
	// packed refs
	must(os.Remove(filepath.Join(root, ".git", "refs", "heads", "main")))
	must(os.WriteFile(filepath.Join(root, ".git", "packed-refs"), []byte("# pack-refs\ndef456 refs/heads/main\n"), 0o644))
	if info := LookupGit(root); info.Head != "def456" {
		t.Fatalf("packed ref not read: %+v", info)
	}
	if info := LookupGit(t.TempDir()); info.Root != "" {
		t.Fatalf("expected no repo, got %+v", info)
	}
}
