// Package factory builds a secrets.Store from configuration. It lives apart
// from the store packages so importing it pulls in every implementation
// exactly once, on both the client and the relay.
package factory

import (
	"fmt"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/secrets/bitwarden"
	"github.com/Tzurrr/codeument/internal/secrets/onepassword"
	"github.com/Tzurrr/codeument/internal/secrets/vault"
	"github.com/Tzurrr/codeument/internal/secrets/webhook"
)

// Providers lists the supported password managers.
var Providers = []string{"vault", "onepassword", "bitwarden", "webhook"}

// New builds the store named in the credential policy. An empty provider
// returns (nil, nil): no manager is configured.
func New(c config.Credentials) (secrets.Store, error) {
	m := c.Manager
	switch m.Provider {
	case "":
		return nil, nil
	case "vault":
		return vault.New(vault.Options{
			Address: m.Vault.Address, Token: m.Vault.Token, Auth: m.Vault.Auth,
			RoleID: m.Vault.RoleID, SecretID: m.Vault.SecretID, Mount: m.Vault.Mount, PathTemplate: m.Vault.PathTemplate,
		})
	case "onepassword":
		return onepassword.New(onepassword.Options{
			ConnectURL: m.OnePassword.ConnectURL, Token: m.OnePassword.Token,
			VaultID: m.OnePassword.VaultID, TitleTemplate: m.OnePassword.TitleTemplate,
		})
	case "bitwarden":
		return bitwarden.New(bitwarden.Options{
			ServerURL: m.Bitwarden.ServerURL, AccessToken: m.Bitwarden.AccessToken,
			OrganizationID: m.Bitwarden.OrganizationID, ProjectID: m.Bitwarden.ProjectID,
		})
	case "webhook":
		return webhook.New(webhook.Options{URL: m.Webhook.URL, BearerToken: m.Webhook.BearerToken, Timeout: m.Webhook.Timeout})
	}
	return nil, fmt.Errorf("credentials.manager.provider must be one of %v (got %q)", Providers, m.Provider)
}
