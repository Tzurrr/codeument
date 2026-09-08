// Package server is the company relay: it authenticates enrolled clients,
// runs summarization with the credentials it holds, publishes documents,
// applies the credential policy and pushes shared defaults.
package server

import (
	_ "embed"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/paths"
)

//go:embed relay_default.yaml
var defaultYAML []byte

// Config is the relay configuration file.
type Config struct {
	Listen         string             `yaml:"listen"`
	TLS            TLS                `yaml:"tls"`
	InsecureHTTP   bool               `yaml:"insecure_http"`
	DBPath         string             `yaml:"db_path"`
	LLM            config.LLM         `yaml:"llm"`
	Docs           config.Docs        `yaml:"docs"`
	Credentials    config.Credentials `yaml:"credentials"`
	ClientDefaults ClientDefaults     `yaml:"client_defaults"`
	Limits         Limits             `yaml:"limits"`
	Audit          Audit              `yaml:"audit"`
	LogLevel       string             `yaml:"log_level"`
}

// TLS holds certificate paths.
type TLS struct {
	Cert     string `yaml:"cert"`
	Key      string `yaml:"key"`
	ClientCA string `yaml:"client_ca"`
}

// ClientDefaults is what the relay pushes to clients.
type ClientDefaults struct {
	Hints                []string          `yaml:"hints"`
	SnapshotLocation     model.Location    `yaml:"snapshot_location"`
	Ignore               []string          `yaml:"ignore"`
	Unignore             []string          `yaml:"unignore"`
	Weights              map[string]int    `yaml:"weights"`
	ExtraPatterns        []string          `yaml:"extra_patterns"`
	CredentialReferences map[string]string `yaml:"credential_references"`
	MinClientVersion     string            `yaml:"min_client_version"`
}

// Limits bound request sizes and rates.
type Limits struct {
	MaxBodyBytes      int64   `yaml:"max_body_bytes"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
}

// Audit configures the audit log.
type Audit struct {
	StorePayloads bool `yaml:"store_payloads"`
	RetentionDays int  `yaml:"retention_days"`
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() *Config {
	var c Config
	if err := yaml.Unmarshal(defaultYAML, &c); err != nil {
		panic(err)
	}
	return &c
}

// DefaultYAML is the commented default file.
func DefaultYAML() []byte { return defaultYAML }

// LoadResult is the loaded config plus provenance.
type LoadResult struct {
	Config  *Config
	Path    string
	Created bool
}

// Load reads the relay config, creating it with defaults when missing.
func Load(path string) (*LoadResult, error) {
	if path == "" {
		path = paths.RelayConfigPath()
	}
	res := &LoadResult{Path: path, Config: DefaultConfig()}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := paths.WriteFilePrivate(path, defaultYAML); err != nil {
			return nil, fmt.Errorf("create default relay config %s: %w", path, err)
		}
		res.Created = true
		data = defaultYAML
	case err != nil:
		return nil, err
	}
	if err := yaml.Unmarshal(data, res.Config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnv(res.Config)
	if err := res.Config.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return res, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("CODEUMENT_RELAY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_DB_PATH"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_INSECURE_HTTP"); v == "1" || v == "true" {
		c.InsecureHTTP = true
	}
	if v := os.Getenv("CODEUMENT_RELAY_LLM_PROVIDER"); v != "" {
		c.LLM.Provider = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_DOCS_PROVIDER"); v != "" {
		c.Docs.Provider = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_DOCS_MARKDOWN_ROOT"); v != "" {
		c.Docs.Markdown.Root = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_OLLAMA_BASE_URL"); v != "" {
		c.LLM.Ollama.BaseURL = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_OLLAMA_MODEL"); v != "" {
		c.LLM.Ollama.Model = v
	}
	if v := os.Getenv("CODEUMENT_RELAY_CREDENTIALS_MODE"); v != "" {
		c.Credentials.Mode = v
	}
	if c.LLM.Anthropic.APIKey == "" {
		c.LLM.Anthropic.APIKey = os.Getenv("ANTHROPIC_API_KEY")
	}
	for _, s := range []*string{&c.LLM.Anthropic.APIKey, &c.Docs.Confluence.APIToken, &c.Credentials.Manager.Vault.Token, &c.Credentials.Manager.OnePassword.Token, &c.Credentials.Manager.Bitwarden.AccessToken, &c.Credentials.Manager.Webhook.BearerToken} {
		if strings.HasPrefix(*s, "file://") {
			if data, err := os.ReadFile(paths.Expand(strings.TrimPrefix(*s, "file://"))); err == nil {
				*s = strings.TrimSpace(string(data))
			}
		}
	}
}

// Validate checks the config; TLS is required unless bound to loopback or
// insecure_http is set explicitly.
func (c *Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen is empty")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen %q: %w", c.Listen, err)
	}
	loopback := host == "localhost" || host == ""
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback && !c.HasTLS() && !c.InsecureHTTP {
		return fmt.Errorf("listen %q is not loopback: set tls.cert and tls.key, or set insecure_http: true behind a TLS-terminating proxy", c.Listen)
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return errors.New("tls.cert and tls.key must be set together")
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
	case config.CredentialsReference, config.CredentialsManager, config.CredentialsInline:
	default:
		return fmt.Errorf("credentials.mode must be reference, manager or inline (got %q)", c.Credentials.Mode)
	}
	if c.Credentials.Mode == config.CredentialsManager && c.Credentials.Manager.Provider == "" {
		return errors.New("credentials.mode is manager but credentials.manager.provider is empty")
	}
	if c.Limits.MaxBodyBytes <= 0 {
		c.Limits.MaxBodyBytes = 2 << 20
	}
	if c.Limits.RequestsPerSecond <= 0 {
		c.Limits.RequestsPerSecond = 2
	}
	if c.Limits.Burst <= 0 {
		c.Limits.Burst = 10
	}
	return nil
}

// HasTLS reports whether a server certificate is configured.
func (c *Config) HasTLS() bool { return c.TLS.Cert != "" && c.TLS.Key != "" }
