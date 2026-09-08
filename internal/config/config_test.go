package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultYAMLMatchesDefault(t *testing.T) {
	var fromYAML Config
	dec := yaml.NewDecoder(bytesReader(defaultYAML))
	dec.KnownFields(true)
	if err := dec.Decode(&fromYAML); err != nil {
		t.Fatalf("default.yaml does not parse strictly: %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(&fromYAML, want) {
		got, _ := yaml.Marshal(&fromYAML)
		exp, _ := yaml.Marshal(want)
		t.Fatalf("default.yaml and Default() differ.\nfrom yaml:\n%s\nfrom Go:\n%s", got, exp)
	}
}

func TestLoadCreatesFileOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg", "config.yaml")
	t.Setenv("CODEUMENT_DATA_DIR", filepath.Join(dir, "data"))
	res, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Fatal("expected Created=true")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", st.Mode().Perm())
	}
	if res.Config.Mode != ModeLocal {
		t.Fatalf("mode = %q", res.Config.Mode)
	}
	if res.Config.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("data dir override not applied: %q", res.Config.DataDir)
	}

	// Second load uses the existing file unchanged.
	res2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created {
		t.Fatal("second load must not recreate the file")
	}
}

func TestLoadUsesExistingFileAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("mode: direct\nllm:\n  provider: ollama\ntriggers:\n  idle_gap: 5m\nunknown_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEUMENT_LLM_OLLAMA_MODEL", "llama3")
	t.Setenv("CODEUMENT_TRIGGERS_SCORE_THRESHOLD", "3")
	t.Setenv("CODEUMENT_CAPTURE_IGNORE", "foo, bar")
	res, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Config
	if c.Mode != ModeDirect || c.LLM.Provider != "ollama" {
		t.Fatalf("file values not applied: %+v", c)
	}
	if c.Triggers.IdleGap.Minutes() != 5 {
		t.Fatalf("idle_gap = %v", c.Triggers.IdleGap)
	}
	if c.LLM.Ollama.Model != "llama3" || c.Triggers.ScoreThreshold != 3 {
		t.Fatalf("env overrides not applied: %+v", c)
	}
	if !reflect.DeepEqual(c.Capture.Ignore, []string{"foo", "bar"}) {
		t.Fatalf("ignore = %v", c.Capture.Ignore)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("expected a warning for unknown_key")
	}
	// Defaults survive for keys the file did not mention.
	if c.Triggers.MeaningfulCount != 6 {
		t.Fatalf("default lost: %d", c.Triggers.MeaningfulCount)
	}
}

func TestSecretFileAndMask(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "key")
	if err := os.WriteFile(secret, []byte("sk-ant-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("llm:\n  anthropic:\n    api_key: file://"+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.LLM.Anthropic.APIKey != "sk-ant-test" {
		t.Fatalf("secret file not resolved: %q", res.Config.LLM.Anthropic.APIKey)
	}
	if res.Config.Masked().LLM.Anthropic.APIKey != "********" {
		t.Fatal("mask failed")
	}
	if res.Config.LLM.Anthropic.APIKey != "sk-ant-test" {
		t.Fatal("Masked must not mutate the original")
	}
}

func TestSetValuePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, defaultYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetValue(path, "llm.provider", "ollama"); err != nil {
		t.Fatal(err)
	}
	if err := SetValue(path, "mode", "direct"); err != nil {
		t.Fatal(err)
	}
	if err := SetValue(path, "docs.default_location.parent_path", "[Runbooks, Linux]"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !contains(data, "# codeument client configuration.") {
		t.Fatal("comments were lost")
	}
	res, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.Config.LLM.Provider != "ollama" || res.Config.Mode != ModeDirect {
		t.Fatalf("values not set: %+v", res.Config)
	}
	if !reflect.DeepEqual(res.Config.Docs.DefaultLocation.ParentPath, []string{"Runbooks", "Linux"}) {
		t.Fatalf("list value not set: %v", res.Config.Docs.DefaultLocation.ParentPath)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Mode = "bogus"
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for bogus mode")
	}
	c = Default()
	c.Mode = ModeRelay
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for relay mode without url")
	}
}
