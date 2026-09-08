//go:build integration

package cli_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codeument-it")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "codeument")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/codeument")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type shellCase struct {
	name    string
	exe     string
	args    []string
	load    string
	rcEnv   string
	prepare func(t *testing.T, home string)
}

func TestShellHooks(t *testing.T) {
	cases := []shellCase{
		{name: "bash", exe: "bash", args: []string{"--norc", "--noprofile", "-i"}, load: `eval "$(` + binary + ` hook bash)"`},
		{name: "zsh", exe: "zsh", args: []string{"-f", "-i"}, load: `eval "$(` + binary + ` hook zsh)"`},
		{name: "fish", exe: "fish", args: []string{"-i", "--no-config"}, load: binary + ` hook fish | source`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if _, err := exec.LookPath(c.exe); err != nil {
				t.Skipf("%s not installed", c.exe)
			}
			runShellCase(t, c)
		})
	}
}

func runShellCase(t *testing.T, c shellCase) {
	home := t.TempDir()
	data := filepath.Join(home, "data")
	cfgPath := filepath.Join(home, "config.yaml")
	env := append(os.Environ(),
		"HOME="+home, "CODEUMENT_CONFIG="+cfgPath, "CODEUMENT_DATA_DIR="+data,
		"CODEUMENT_NO_WORKER=1", "TERM=dumb", "PS1=$ ", "PROMPT=%% ", "HISTFILE=", "LC_ALL=C",
	)
	cmd := exec.Command(c.exe, c.args...)
	cmd.Env = env
	cmd.Dir = home
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptmx.Close(); _ = cmd.Process.Kill() }()

	var output strings.Builder
	go func() { _, _ = io.Copy(&output, ptmx) }()

	send := func(line string) {
		if _, err := fmt.Fprintf(ptmx, "%s\r", line); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	send(c.load)
	time.Sleep(500 * time.Millisecond)
	send("mysql -u root -phunter2 db")
	send("cd /tmp")
	send("false")
	send("git status")
	send("exit")

	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("shell did not exit; output:\n%s", output.String())
	}

	store, err := journal.Open(filepath.Join(data, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var evs []model.Event
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		evs, err = store.ListEvents(context.Background(), journal.EventQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) >= 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(evs) < 4 {
		t.Fatalf("expected at least 4 events, got %d; shell output:\n%s", len(evs), output.String())
	}
	byCmd := map[string]model.Event{}
	for _, e := range evs {
		byCmd[e.Command] = e
	}
	my, ok := byCmd["mysql -u root -p<redacted:password> db"]
	if !ok {
		t.Fatalf("mysql command not redacted; events: %+v", evs)
	}
	if my.Redactions[0] != "password" || my.Family != "database" || my.Kind != model.KindMeaningful {
		t.Fatalf("mysql event: %+v", my)
	}
	if f, ok := byCmd["false"]; !ok || f.ExitCode != 1 {
		t.Fatalf("exit code not captured: %+v", byCmd["false"])
	}
	if g, ok := byCmd["git status"]; !ok || g.CWD != "/tmp" || g.Family != "git" {
		t.Fatalf("cwd/family not captured: %+v", byCmd["git status"])
	}
	if cd, ok := byCmd["cd /tmp"]; !ok || cd.Kind != model.KindNoise {
		t.Fatalf("cd should be noise: %+v", byCmd["cd /tmp"])
	}
	for _, e := range evs {
		if e.SessionID == "" || e.Shell != c.name || e.Start.IsZero() {
			t.Fatalf("missing metadata: %+v", e)
		}
		if strings.Contains(e.Command, "hunter2") {
			t.Fatalf("secret leaked: %+v", e)
		}
	}
	sessions, err := store.ListSessions(context.Background(), 5)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %+v", err, sessions)
	}
	if sessions[0].EndedAt == nil {
		t.Logf("warning: session end not recorded (exit trap)")
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("config not auto-created: %v", err)
	}
}
