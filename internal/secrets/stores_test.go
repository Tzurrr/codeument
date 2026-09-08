package secrets_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/secrets/bitwarden"
	"github.com/Tzurrr/codeument/internal/secrets/factory"
	"github.com/Tzurrr/codeument/internal/secrets/onepassword"
	"github.com/Tzurrr/codeument/internal/secrets/vault"
	"github.com/Tzurrr/codeument/internal/secrets/webhook"
)

var cred = secrets.Credential{Host: "web-01", MachineID: "m1", Username: "root", Kind: "os", Password: "Pl4in!", Notes: "console login"}

func TestVaultStore(t *testing.T) {
	var written map[string]any
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Vault-Token")
		switch {
		case r.URL.Path == "/v1/auth/approle/login":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["role_id"] != "rid" || in["secret_id"] != "sid" {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "s.approle-token"}})
		case r.URL.Path == "/v1/sys/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"sealed": false})
		case strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
			if gotToken == "" {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&written)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"version": 1}})
		case r.URL.Path == "/v1/secret/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	s, err := vault.New(vault.Options{Address: srv.URL, Token: "s.root", Mount: "secret", PathTemplate: "servers/{hostname}/{username}"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "vault://secret/servers/web-01/root" || !strings.Contains(ref.URL, "/ui/vault/secrets/secret/show/servers/web-01/root") {
		t.Fatalf("ref = %+v", ref)
	}
	data, _ := written["data"].(map[string]any)
	if data["password"] != "Pl4in!" || data["username"] != "root" || data["managed_by"] != "codeument" {
		t.Fatalf("written = %+v", written)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}

	// AppRole logs in once and reuses the token.
	ap, err := vault.New(vault.Options{Address: srv.URL, Auth: "approle", RoleID: "rid", SecretID: "sid", Mount: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Put(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	if gotToken != "s.approle-token" {
		t.Fatalf("approle token not used: %q", gotToken)
	}
	if _, err := vault.New(vault.Options{Address: srv.URL, Auth: "approle"}); err == nil {
		t.Fatal("approle without role_id must be refused")
	}
	if _, err := vault.New(vault.Options{Address: srv.URL}); err == nil {
		t.Fatal("token auth without a token must be refused")
	}
}

func TestOnePasswordStore(t *testing.T) {
	items := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer op-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vaults/v1":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "v1", "name": "Infra"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vaults/v1/items":
			var out []map[string]any
			for _, it := range items {
				if strings.Contains(r.URL.Query().Get("filter"), it["title"].(string)) {
					out = append(out, it)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vaults/v1/items":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			in["id"] = "item-1"
			in["version"] = 1.0
			items["item-1"] = in
			_ = json.NewEncoder(w).Encode(in)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/vaults/v1/items/"):
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := strings.TrimPrefix(r.URL.Path, "/v1/vaults/v1/items/")
			in["id"] = id
			items[id] = in
			_ = json.NewEncoder(w).Encode(in)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	s, err := onepassword.New(onepassword.Options{ConnectURL: srv.URL, Token: "op-token", VaultID: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "op://v1/web-01 / root" || !strings.Contains(ref.URL, "items/item-1") {
		t.Fatalf("ref = %+v", ref)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	fields, _ := items["item-1"]["fields"].([]any)
	found := false
	for _, f := range fields {
		m := f.(map[string]any)
		if m["purpose"] == "PASSWORD" && m["value"] == "Pl4in!" && m["type"] == "CONCEALED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("password field missing: %+v", fields)
	}
	// A second Put updates the same item instead of creating a duplicate.
	if _, err := s.Put(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected one item after update, got %d", len(items))
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad, _ := onepassword.New(onepassword.Options{ConnectURL: srv.URL, Token: "wrong", VaultID: "v1"})
	if err := bad.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

func TestBitwardenStore(t *testing.T) {
	secretsByID := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bw-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/organizations/org1/secrets":
			var data []map[string]any
			for id, s := range secretsByID {
				data = append(data, map[string]any{"id": id, "key": s["key"]})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		case r.Method == http.MethodPost && r.URL.Path == "/api/organizations/org1/secrets":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			secretsByID["sec-1"] = in
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sec-1", "key": in["key"]})
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/secrets/"):
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			id := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
			secretsByID[id] = in
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "key": in["key"]})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	s, err := bitwarden.New(bitwarden.Options{ServerURL: srv.URL, AccessToken: "bw-token", OrganizationID: "org1", ProjectID: "proj1"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "bitwarden://web-01/root" || !strings.Contains(ref.URL, "secrets/sec-1") {
		t.Fatalf("ref = %+v", ref)
	}
	if secretsByID["sec-1"]["value"] != "Pl4in!" {
		t.Fatalf("stored = %+v", secretsByID["sec-1"])
	}
	if _, err := s.Put(context.Background(), cred); err != nil || len(secretsByID) != 1 {
		t.Fatalf("update path: %v %d", err, len(secretsByID))
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWebhookStore(t *testing.T) {
	var got webhook.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(405)
			return
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(200)
			return
		}
		if r.Header.Get("Authorization") != "Bearer hook-token" {
			w.WriteHeader(403)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(webhook.Response{Reference: "itsm://cred/42", URL: "https://it.corp/creds/42"})
	}))
	defer srv.Close()

	s, err := webhook.New(webhook.Options{URL: srv.URL, BearerToken: "hook-token"})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Ref != "itsm://cred/42" || ref.URL != "https://it.corp/creds/42" {
		t.Fatalf("ref = %+v", ref)
	}
	if got.Host != "web-01" || got.Username != "root" || got.Password != "Pl4in!" || got.MachineID != "m1" {
		t.Fatalf("request = %+v", got)
	}
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := webhook.New(webhook.Options{URL: "http://it.corp/hook"}); err == nil {
		t.Fatal("plain http to a remote host must be refused")
	}

	// An endpoint that answers without a reference is an error, not silence.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bad.Close()
	bs, _ := webhook.New(webhook.Options{URL: bad.URL})
	if _, err := bs.Put(context.Background(), cred); err == nil {
		t.Fatal("expected an error for a reference-less response")
	}
}

func TestPolicyModes(t *testing.T) {
	ctx := context.Background()
	refs := map[string]string{"default": "vault://infra/{hostname}/{username}", "root": "1password://Infra/{hostname} root"}

	p := secrets.Policy{Mode: secrets.ModeReference, References: refs}
	r, err := p.Apply(ctx, cred)
	if err != nil || r.Mode != secrets.ModeReference || r.Ref != "1password://Infra/web-01 root" || r.Password != "" {
		t.Fatalf("reference: %v %+v", err, r)
	}
	if got := p.ReferenceOnly("web-01", "alice"); got.Ref != "vault://infra/web-01/alice" {
		t.Fatalf("default template: %+v", got)
	}

	p.Mode = secrets.ModeInline
	r, err = p.Apply(ctx, cred)
	if err != nil || r.Password != "Pl4in!" {
		t.Fatalf("inline: %v %+v", err, r)
	}

	p.Mode = secrets.ModeManager
	if _, err := p.Apply(ctx, cred); err != secrets.ErrNoStore {
		t.Fatalf("manager without a store must fail with ErrNoStore, got %v", err)
	}

	if err := secrets.ValidateMode("bogus"); err == nil {
		t.Fatal("expected an error for a bogus mode")
	}
}

func TestFactory(t *testing.T) {
	if s, err := factory.New(config.Credentials{}); err != nil || s != nil {
		t.Fatalf("empty provider: %v %v", s, err)
	}
	c := config.Credentials{}
	c.Manager.Provider = "nope"
	if _, err := factory.New(c); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
	c.Manager.Provider = "vault"
	c.Manager.Vault.Address = "https://vault.corp"
	c.Manager.Vault.Token = "t"
	s, err := factory.New(c)
	if err != nil || s == nil || s.Name() != "vault" {
		t.Fatalf("vault: %v %v", s, err)
	}
	c.Manager.Provider = "webhook"
	c.Manager.Webhook.URL = "https://it.corp/hook"
	if s, err := factory.New(c); err != nil || s.Name() != "webhook" {
		t.Fatalf("webhook: %v %v", s, err)
	}
}
