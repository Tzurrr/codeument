// Package secrets defines the credential policy and the password-manager
// store interface. Store implementations live in sub-packages.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Policy modes.
const (
	ModeReference = "reference"
	ModeManager   = "manager"
	ModeInline    = "inline"
)

// Credential is a password for an account on a host.
type Credential struct {
	Host      string `json:"host"`
	MachineID string `json:"machine_id,omitempty"`
	Username  string `json:"username"`
	Kind      string `json:"kind"` // os | db | service
	Password  string `json:"password"`
	Notes     string `json:"notes,omitempty"`
}

// Reference is what ends up on the page.
type Reference struct {
	Mode     string `json:"mode"`
	Ref      string `json:"ref,omitempty"`
	URL      string `json:"url,omitempty"`
	Password string `json:"password,omitempty"` // set only in inline mode
}

// Store is one password manager.
type Store interface {
	Name() string
	// Put stores (or updates) the credential and returns how to reference it.
	Put(ctx context.Context, c Credential) (Reference, error)
	Ping(ctx context.Context) error
}

// Policy applies the mode and default references.
type Policy struct {
	Mode                string
	CaptureFromCommands bool
	PromptOnReview      bool
	// References maps username (or "default") to a reference template with
	// {hostname} and {username} placeholders, used in reference mode.
	References map[string]string
	Store      Store
	// RestrictGroups are applied to pages carrying inline passwords.
	RestrictGroups []string
}

// ErrNoStore is returned in manager mode without a configured store.
var ErrNoStore = errors.New("secrets: credentials.mode is manager but no password manager is configured")

// ErrPolicy is returned when a mode refuses the operation.
var ErrPolicy = errors.New("secrets: refused by credential policy")

// Apply resolves a credential to a reference according to the policy.
func (p Policy) Apply(ctx context.Context, c Credential) (Reference, error) {
	switch p.Mode {
	case ModeInline:
		return Reference{Mode: ModeInline, Password: c.Password, Ref: p.referenceFor(c)}, nil
	case ModeManager:
		if p.Store == nil {
			return Reference{}, ErrNoStore
		}
		ref, err := p.Store.Put(ctx, c)
		if err != nil {
			return Reference{}, err
		}
		ref.Mode = ModeManager
		ref.Password = ""
		return ref, nil
	default:
		return Reference{Mode: ModeReference, Ref: p.referenceFor(c)}, nil
	}
}

// ReferenceOnly returns the reference string for an account without a
// password (what the page shows before any credential is known).
func (p Policy) ReferenceOnly(host, username string) Reference {
	return Reference{Mode: ModeReference, Ref: p.referenceFor(Credential{Host: host, Username: username})}
}

func (p Policy) referenceFor(c Credential) string {
	tmpl := p.References[c.Username]
	if tmpl == "" {
		tmpl = p.References["default"]
	}
	if tmpl == "" {
		tmpl = "vault://infra/{hostname}/{username}"
	}
	return Expand(tmpl, c.Host, c.Username)
}

// Expand substitutes {hostname} and {username} in a template.
func Expand(tmpl, host, username string) string {
	r := strings.NewReplacer("{hostname}", host, "{username}", username, "{host}", host, "{user}", username)
	return r.Replace(tmpl)
}

// Timeout is the default per-store HTTP timeout.
const Timeout = 15 * time.Second

// Validate checks a mode string.
func ValidateMode(mode string) error {
	switch mode {
	case ModeReference, ModeManager, ModeInline:
		return nil
	}
	return fmt.Errorf("credentials.mode must be reference, manager or inline (got %q)", mode)
}
