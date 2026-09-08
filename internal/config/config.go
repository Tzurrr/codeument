// Package config defines the client configuration, its defaults, and how it
// is loaded: the file is created with defaults on first run, otherwise it is
// used as is, then environment overrides are applied.
package config

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/paths"
)

//go:embed default.yaml
var defaultYAML []byte

// Modes.
const (
	ModeLocal  = "local"
	ModeDirect = "direct"
	ModeRelay  = "relay"
)

// EnvPrefix prefixes every environment override.
const EnvPrefix = "CODEUMENT_"

// Config is the full client configuration.
type Config struct {
	Mode        string      `yaml:"mode"`
	Relay       RelayClient `yaml:"relay"`
	Capture     Capture     `yaml:"capture"`
	Redact      Redact      `yaml:"redact"`
	Triggers    Triggers    `yaml:"triggers"`
	Notify      Notify      `yaml:"notify"`
	LLM         LLM         `yaml:"llm"`
	Docs        Docs        `yaml:"docs"`
	Hints       []string    `yaml:"hints"`
	Snapshot    Snapshot    `yaml:"snapshot"`
	Credentials Credentials `yaml:"credentials"`
	Retention   Retention   `yaml:"retention"`
	Editor      string      `yaml:"editor"`
	DataDir     string      `yaml:"data_dir"`
	LogLevel    string      `yaml:"log_level"`
}

// RelayClient is how the client reaches the relay.
type RelayClient struct {
	URL       string `yaml:"url"`
	TokenFile string `yaml:"token_file"`
	CAFile    string `yaml:"ca_file"`
}

// Capture tunes what gets journaled.
type Capture struct {
	StoreNoise bool           `yaml:"store_noise"`
	Ignore     []string       `yaml:"ignore"`
	Unignore   []string       `yaml:"unignore"`
	Weights    map[string]int `yaml:"weights"`
}

// Redact extends the built-in redaction rules.
type Redact struct {
	ExtraPatterns          []string `yaml:"extra_patterns"`
	ExtraSensitiveCommands []string `yaml:"extra_sensitive_commands"`
}

// Triggers decide when a batch is summarized.
type Triggers struct {
	IdleGap         time.Duration `yaml:"idle_gap"`
	ScoreThreshold  int           `yaml:"score_threshold"`
	MeaningfulCount int           `yaml:"meaningful_count"`
	Timer           time.Duration `yaml:"timer"`
	MinInterval     time.Duration `yaml:"min_interval"`
	Milestones      bool          `yaml:"milestones"`
}

// Notify controls the prompt-time notice.
type Notify struct {
	Style string `yaml:"style"`
}

// LLM selects and configures the inference provider (direct mode).
type LLM struct {
	Provider  string    `yaml:"provider"`
	Anthropic Anthropic `yaml:"anthropic"`
	Ollama    Ollama    `yaml:"ollama"`
}

// Anthropic configures the Claude provider.
type Anthropic struct {
	APIKey    string `yaml:"api_key"`
	Model     string `yaml:"model"`
	MaxTokens int    `yaml:"max_tokens"`
	Fallbacks bool   `yaml:"fallbacks"`
}

// Ollama configures the Ollama provider.
type Ollama struct {
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
	NumCtx  int    `yaml:"num_ctx"`
}

// Docs selects and configures the documentation provider (direct mode).
type Docs struct {
	Provider        string         `yaml:"provider"`
	Markdown        Markdown       `yaml:"markdown"`
	Confluence      Confluence     `yaml:"confluence"`
	DefaultLocation model.Location `yaml:"default_location"`
}

// Markdown configures the filesystem provider.
type Markdown struct {
	Root string `yaml:"root"`
}

// Confluence configures the Confluence Cloud provider.
type Confluence struct {
	BaseURL  string `yaml:"base_url"`
	Email    string `yaml:"email"`
	APIToken string `yaml:"api_token"`
	Space    string `yaml:"space"`
}

// Snapshot configures server snapshots.
type Snapshot struct {
	Enabled              bool              `yaml:"enabled"`
	Interval             time.Duration     `yaml:"interval"`
	Publish              bool              `yaml:"publish"`
	Dirs                 SnapshotDirs      `yaml:"dirs"`
	CredentialReferences map[string]string `yaml:"credential_references"`
	DocLocation          model.Location    `yaml:"doc_location"`
}

// SnapshotDirs adds or removes directories from the snapshot.
type SnapshotDirs struct {
	Include []string `yaml:"include"`
	Exclude []string `yaml:"exclude"`
}

// Credential policy modes.
const (
	CredentialsReference = "reference"
	CredentialsManager   = "manager"
	CredentialsInline    = "inline"
)

// Credentials is the password-handling policy.
type Credentials struct {
	Mode                string            `yaml:"mode"`
	CaptureFromCommands bool              `yaml:"capture_from_commands"`
	PromptOnReview      bool              `yaml:"prompt_on_review"`
	Manager             CredentialManager `yaml:"manager"`
	Inline              CredentialInline  `yaml:"inline"`
}

// CredentialManager selects and configures the password manager.
type CredentialManager struct {
	Provider    string       `yaml:"provider"`
	Vault       VaultStore   `yaml:"vault"`
	OnePassword OnePassword  `yaml:"onepassword"`
	Bitwarden   Bitwarden    `yaml:"bitwarden"`
	Webhook     WebhookStore `yaml:"webhook"`
}

// VaultStore configures HashiCorp Vault KV v2.
type VaultStore struct {
	Address      string `yaml:"address"`
	Token        string `yaml:"token"`
	Auth         string `yaml:"auth"`
	RoleID       string `yaml:"role_id"`
	SecretID     string `yaml:"secret_id"`
	Mount        string `yaml:"mount"`
	PathTemplate string `yaml:"path_template"`
}

// OnePassword configures 1Password Connect.
type OnePassword struct {
	ConnectURL    string `yaml:"connect_url"`
	Token         string `yaml:"token"`
	VaultID       string `yaml:"vault_id"`
	TitleTemplate string `yaml:"title_template"`
}

// Bitwarden configures Bitwarden Secrets Manager.
type Bitwarden struct {
	ServerURL      string `yaml:"server_url"`
	AccessToken    string `yaml:"access_token"`
	OrganizationID string `yaml:"organization_id"`
	ProjectID      string `yaml:"project_id"`
}

// WebhookStore configures the generic webhook store.
type WebhookStore struct {
	URL         string        `yaml:"url"`
	BearerToken string        `yaml:"bearer_token"`
	Timeout     time.Duration `yaml:"timeout"`
}

// CredentialInline configures inline passwords on pages.
type CredentialInline struct {
	PageRestrictionGroups []string `yaml:"page_restriction_groups"`
}

// Retention configures pruning.
type Retention struct {
	Events time.Duration `yaml:"events"`
}

// Default returns the built-in defaults. It must match default.yaml; a test
// enforces that.
func Default() *Config {
	return &Config{
		Mode: ModeLocal,
		Relay: RelayClient{
			TokenFile: "~/.config/codeument/relay-token",
		},
		Capture: Capture{StoreNoise: true, Ignore: []string{}, Unignore: []string{}, Weights: map[string]int{}},
		Redact:  Redact{ExtraPatterns: []string{}, ExtraSensitiveCommands: []string{}},
		Triggers: Triggers{
			IdleGap:         20 * time.Minute,
			ScoreThreshold:  8,
			MeaningfulCount: 6,
			Timer:           45 * time.Minute,
			MinInterval:     10 * time.Minute,
			Milestones:      true,
		},
		Notify: Notify{Style: "line"},
		LLM: LLM{
			Provider:  "anthropic",
			Anthropic: Anthropic{Model: "claude-opus-5", MaxTokens: 8000, Fallbacks: true},
			Ollama:    Ollama{BaseURL: "http://127.0.0.1:11434", Model: "qwen2.5:14b", NumCtx: 16384},
		},
		Docs: Docs{
			Provider:        "markdown",
			Markdown:        Markdown{Root: "~/Documents/codeument-docs"},
			Confluence:      Confluence{Space: "OPS"},
			DefaultLocation: model.Location{Space: "OPS", ParentPath: []string{"Runbooks"}},
		},
		Hints: []string{},
		Snapshot: Snapshot{
			Interval:             24 * time.Hour,
			Publish:              true,
			Dirs:                 SnapshotDirs{Include: []string{}, Exclude: []string{}},
			CredentialReferences: map[string]string{"default": "vault://infra/{hostname}"},
			DocLocation:          model.Location{Space: "OPS", ParentPath: []string{"Servers"}},
		},
		Credentials: Credentials{
			Mode:           CredentialsReference,
			PromptOnReview: true,
			Manager: CredentialManager{
				Vault:       VaultStore{Auth: "token", Mount: "secret", PathTemplate: "servers/{hostname}/{username}"},
				OnePassword: OnePassword{TitleTemplate: "{hostname} / {username}"},
				Webhook:     WebhookStore{Timeout: 10 * time.Second},
			},
			Inline: CredentialInline{PageRestrictionGroups: []string{}},
		},
		Retention: Retention{Events: 90 * 24 * time.Hour},
		DataDir:   "~/.local/share/codeument",
		LogLevel:  "info",
	}
}

// DefaultYAML is the commented default config file content.
func DefaultYAML() []byte { return defaultYAML }

// Result is what Load returns: the effective config plus where it came from.
type Result struct {
	Config       *Config
	Path         string
	Created      bool
	SystemPath   string
	SystemLoaded bool
	Warnings     []string
}

// Load reads the config at path (or the default user path when empty). A
// missing file is created from the defaults first. The system config, when
// present, is applied under the user file. Environment overrides come last.
func Load(path string) (*Result, error) {
	if path == "" {
		path = paths.UserConfigPath()
	}
	res := &Result{Path: path, Config: Default()}

	sys := paths.SystemConfigPath()
	if data, err := os.ReadFile(sys); err == nil && sys != path {
		if err := decodeInto(res.Config, data, sys, &res.Warnings); err != nil {
			return nil, err
		}
		res.SystemPath = sys
		res.SystemLoaded = true
	}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := paths.WriteFilePrivate(path, defaultYAML); err != nil {
			return nil, fmt.Errorf("create default config %s: %w", path, err)
		}
		res.Created = true
		data = defaultYAML
	case err != nil:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := decodeInto(res.Config, data, path, &res.Warnings); err != nil {
		return nil, err
	}

	applyEnv(reflect.ValueOf(res.Config).Elem(), []string{})
	if res.Config.LLM.Anthropic.APIKey == "" {
		res.Config.LLM.Anthropic.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	if err := resolveSecretFiles(reflect.ValueOf(res.Config).Elem(), ""); err != nil {
		return nil, err
	}
	res.Config.expandPaths()
	if err := res.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return res, nil
}

func decodeInto(cfg *Config, data []byte, source string, warnings *[]string) error {
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	probe := Default()
	if err := strict.Decode(probe); err != nil && !errors.Is(err, errEOF(err)) {
		if !strings.Contains(err.Error(), "not found in type") {
			return fmt.Errorf("parse %s: %w", source, err)
		}
		*warnings = append(*warnings, fmt.Sprintf("%s: %s", source, strings.ReplaceAll(err.Error(), "\n", "; ")))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("parse %s: %w", source, err)
	}
	return nil
}

// errEOF returns the error itself when it is an EOF from an empty document so
// callers can ignore it, or a sentinel that never matches otherwise.
func errEOF(err error) error {
	if err != nil && err.Error() == "EOF" {
		return err
	}
	return errNever
}

var errNever = errors.New("never")

func (c *Config) expandPaths() {
	c.DataDir = paths.Expand(c.DataDir)
	if v := os.Getenv(paths.EnvDataDir); v != "" {
		c.DataDir = paths.Expand(v)
	}
	c.Relay.TokenFile = paths.Expand(c.Relay.TokenFile)
	c.Relay.CAFile = paths.Expand(c.Relay.CAFile)
	c.Docs.Markdown.Root = paths.Expand(c.Docs.Markdown.Root)
}

// Validate checks enumerations and required combinations.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeDirect, ModeRelay:
	default:
		return fmt.Errorf("mode must be local, direct or relay (got %q)", c.Mode)
	}
	switch c.LLM.Provider {
	case "anthropic", "ollama", "fake":
	default:
		return fmt.Errorf("llm.provider must be anthropic or ollama (got %q)", c.LLM.Provider)
	}
	switch c.Docs.Provider {
	case "markdown", "confluence", "fake":
	default:
		return fmt.Errorf("docs.provider must be markdown or confluence (got %q)", c.Docs.Provider)
	}
	switch c.Credentials.Mode {
	case CredentialsReference, CredentialsManager, CredentialsInline:
	default:
		return fmt.Errorf("credentials.mode must be reference, manager or inline (got %q)", c.Credentials.Mode)
	}
	if c.Mode == ModeRelay && c.Relay.URL == "" {
		return errors.New("mode is relay but relay.url is empty (run `codeument enroll`)")
	}
	if c.Triggers.IdleGap <= 0 || c.Triggers.Timer <= 0 {
		return errors.New("triggers.idle_gap and triggers.timer must be positive durations")
	}
	if c.Notify.Style != "line" && c.Notify.Style != "silent" {
		return fmt.Errorf("notify.style must be line or silent (got %q)", c.Notify.Style)
	}
	return nil
}

// applyEnv walks the struct and applies CODEUMENT_<PATH> overrides.
func applyEnv(v reflect.Value, path []string) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		fv := v.Field(i)
		p := append(append([]string{}, path...), tag)
		if fv.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)) {
			applyEnv(fv, p)
			continue
		}
		env := EnvPrefix + strings.ToUpper(strings.Join(p, "_"))
		val, ok := os.LookupEnv(env)
		if !ok {
			continue
		}
		setFromString(fv, val)
	}
}

func setFromString(fv reflect.Value, val string) {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(val)
	case reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err == nil {
			fv.SetBool(b)
		}
	case reflect.Int, reflect.Int64:
		if fv.Type() == reflect.TypeOf(time.Duration(0)) {
			if d, err := time.ParseDuration(val); err == nil {
				fv.SetInt(int64(d))
			}
			return
		}
		if n, err := strconv.ParseInt(val, 10, 64); err == nil {
			fv.SetInt(n)
		}
	case reflect.Slice:
		if fv.Type().Elem().Kind() == reflect.String {
			parts := []string{}
			for _, s := range strings.Split(val, ",") {
				if s = strings.TrimSpace(s); s != "" {
					parts = append(parts, s)
				}
			}
			fv.Set(reflect.ValueOf(parts))
		}
	}
}

// IsSecretField reports whether a yaml key names a secret.
func IsSecretField(name string) bool {
	n := strings.ToLower(name)
	for _, w := range []string{"key", "token", "password", "secret"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

func resolveSecretFiles(v reflect.Value, prefix string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)) {
			if err := resolveSecretFiles(fv, prefix+tag+"."); err != nil {
				return err
			}
			continue
		}
		if fv.Kind() != reflect.String || !IsSecretField(tag) {
			continue
		}
		s := fv.String()
		if !strings.HasPrefix(s, "file://") {
			continue
		}
		data, err := os.ReadFile(paths.Expand(strings.TrimPrefix(s, "file://")))
		if err != nil {
			return fmt.Errorf("%s%s: %w", prefix, tag, err)
		}
		fv.SetString(strings.TrimSpace(string(data)))
	}
	return nil
}

// Masked returns a copy with secret values replaced by asterisks.
func (c *Config) Masked() *Config {
	cp := *c
	mask(reflect.ValueOf(&cp).Elem())
	return &cp
}

func mask(v reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && f.Type != reflect.TypeOf(time.Duration(0)) {
			mask(fv)
			continue
		}
		if fv.Kind() == reflect.String && IsSecretField(tag) && fv.String() != "" {
			fv.SetString("********")
		}
	}
}

// Marshal renders the config as YAML.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), enc.Close()
}

// SetValue updates one dotted key (e.g. "llm.provider") in the YAML file at
// path, preserving comments and ordering, and validates the result.
func SetValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data = defaultYAML
	} else if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	node := doc.Content[0]
	parts := strings.Split(key, ".")
	for i, part := range parts {
		if node.Kind != yaml.MappingNode {
			return fmt.Errorf("%s is not a mapping", strings.Join(parts[:i], "."))
		}
		var child *yaml.Node
		for j := 0; j+1 < len(node.Content); j += 2 {
			if node.Content[j].Value == part {
				child = node.Content[j+1]
				break
			}
		}
		if child == nil {
			child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if i == len(parts)-1 {
				child = &yaml.Node{Kind: yaml.ScalarNode}
			}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, child)
		}
		node = child
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		var seq yaml.Node
		if err := yaml.Unmarshal([]byte(value), &seq); err == nil && len(seq.Content) == 1 {
			*node = *seq.Content[0]
		}
	} else {
		node.Kind = yaml.ScalarNode
		node.Tag = ""
		node.Style = 0
		node.Content = nil
		node.Value = value
		if value == "" {
			node.Style = yaml.DoubleQuotedStyle
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	probe := Default()
	if err := yaml.Unmarshal(buf.Bytes(), probe); err != nil {
		return fmt.Errorf("resulting config is invalid: %w", err)
	}
	return paths.WriteFilePrivate(path, buf.Bytes())
}

// JournalPath is the SQLite journal file.
func (c *Config) JournalPath() string { return filepath.Join(c.DataDir, "journal.db") }

// NotifyPath is the prompt-time notification file.
func (c *Config) NotifyPath() string { return filepath.Join(c.DataDir, "notify") }

// SeenDir holds per-session "notification seen" markers.
func (c *Config) SeenDir() string { return filepath.Join(c.DataDir, "seen") }

// PausePath is the pause marker file.
func (c *Config) PausePath() string { return filepath.Join(c.DataDir, "paused") }

// LockPath is the worker single-instance lock.
func (c *Config) LockPath() string { return filepath.Join(c.DataDir, "work.lock") }

// CredCachePath is the encrypted credential cache database.
func (c *Config) CredCachePath() string { return filepath.Join(c.DataDir, "credcache.db") }

// CredCacheKeyPath is the key file for the credential cache.
func (c *Config) CredCacheKeyPath() string { return filepath.Join(c.DataDir, "credcache.key") }
