package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/docs"
	docsfake "github.com/Tzurrr/codeument/internal/docs/fake"
	"github.com/Tzurrr/codeument/internal/llm"
	llmfake "github.com/Tzurrr/codeument/internal/llm/fake"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/relay/api"
	"github.com/Tzurrr/codeument/internal/relay/client"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/summarize"
)

type fakeStore struct{ puts []secrets.Credential }

func (f *fakeStore) Name() string { return "fakevault" }
func (f *fakeStore) Put(_ context.Context, c secrets.Credential) (secrets.Reference, error) {
	f.puts = append(f.puts, c)
	return secrets.Reference{Ref: "fakevault://" + c.Host + "/" + c.Username, URL: "https://vault/" + c.Username}, nil
}
func (f *fakeStore) Ping(context.Context) error { return nil }

func newTestServer(t *testing.T, mode string) (*httptest.Server, *Server, *docsfake.Provider, *fakeStore) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Listen = "127.0.0.1:0"
	cfg.LLM.Provider = "fake"
	cfg.Docs.Provider = "fake"
	cfg.Credentials.Mode = mode
	if mode == secrets.ModeManager {
		cfg.Credentials.Manager.Provider = "fakevault"
	}
	cfg.ClientDefaults.Hints = []string{"relay hint"}
	cfg.ClientDefaults.MinClientVersion = "0.2.0"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	dp := docsfake.New()
	fs := &fakeStore{}
	pol := secrets.Policy{Mode: mode, References: cfg.ClientDefaults.CredentialReferences}
	if mode == secrets.ModeManager {
		pol.Store = fs
	}
	s := New(cfg, store, llmfake.New(), dp, pol, "test")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s, dp, fs
}

func enroll(t *testing.T, ts *httptest.Server, s *Server, name string) *client.Engine {
	t.Helper()
	code, err := s.Store.NewEnrollCode(context.Background(), name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.New(client.Options{BaseURL: ts.URL, Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Enroll(context.Background(), api.EnrollRequest{Code: strings.ToLower(code), Hostname: "laptop", Username: "alice", OS: "linux", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" || resp.ClientID == "" || resp.Name != name {
		t.Fatalf("enroll = %+v", resp)
	}
	c.Token = resp.Token
	return client.NewEngine(c)
}

func TestEnrollAuthAndFlows(t *testing.T) {
	ts, s, dp, _ := newTestServer(t, secrets.ModeReference)
	ctx := context.Background()
	eng := enroll(t, ts, s, "alice-laptop")

	// Code is single-use.
	c2, _ := client.New(client.Options{BaseURL: ts.URL})
	if _, err := c2.Enroll(ctx, api.EnrollRequest{Code: "AAAA-BBBB-CCCC"}); err == nil {
		t.Fatal("bogus code accepted")
	}
	// Unauthenticated and bad tokens are rejected.
	if err := client.NewEngine(c2).Ping(ctx); err == nil {
		t.Fatal("expected 401 without token")
	}
	c2.Token = "cdm_bad_token"
	if err := client.NewEngine(c2).Ping(ctx); err == nil {
		t.Fatal("expected 401 with bad token")
	}

	d, err := eng.Defaults(ctx)
	if err != nil || d.Hints[0] != "relay hint" || d.CredentialMode != secrets.ModeReference || d.DefaultLocation.Space != "OPS" {
		t.Fatalf("defaults: %v %+v", err, d)
	}

	now := time.Now()
	in := summarize.Input{Hostname: "laptop", Username: "alice", Batch: model.Batch{ID: "b", SessionID: "s", Events: []model.Event{
		{Seq: 1, Start: now, End: now, Command: "apt install nginx", Kind: model.KindMeaningful, Weight: 3, CWD: "/", Shell: "bash"},
		{Seq: 2, Start: now, End: now, Command: "systemctl restart nginx", Kind: model.KindMeaningful, Weight: 5, CWD: "/", Shell: "bash"},
	}}}
	out, err := eng.Summarize(ctx, in)
	if err != nil || out.Draft.Title == "" || len(out.Draft.Commands) != 2 || out.Draft.SuggestedLocation.Space != "OPS" {
		t.Fatalf("summarize: %v %+v", err, out)
	}

	ref, err := eng.Publish(ctx, docs.Document{ID: "doc-1", Title: "Install nginx", BodyMD: "body"})
	if err != nil || ref.Provider != "fake" {
		t.Fatalf("publish: %v %+v", err, ref)
	}
	if dp.Pages["doc-1"].Meta["relay_client"] != "alice-laptop" || dp.Pages["doc-1"].Location.Space != "OPS" {
		t.Fatalf("published doc = %+v", dp.Pages["doc-1"])
	}
	found, err := eng.FindDoc(ctx, "doc-1")
	if err != nil || found == nil || found.ProviderID != ref.ProviderID {
		t.Fatalf("find: %v %+v", err, found)
	}
	got, err := eng.GetDoc(ctx, *found)
	if err != nil || got.BodyMD != "body" {
		t.Fatalf("get: %v %+v", err, got)
	}
	if none, err := eng.FindDoc(ctx, "missing"); err != nil || none != nil {
		t.Fatalf("find missing: %v %+v", err, none)
	}
	if _, err := eng.GetDoc(ctx, docs.PageRef{ProviderID: "nope"}); !errors.Is(err, docs.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Reference mode: credentials are never stored, only referenced.
	r, err := eng.StoreCredential(ctx, secrets.Credential{Host: "web-01", Username: "root", Password: "pw"})
	if err != nil || r.Mode != secrets.ModeReference || r.Ref != "vault://infra/web-01" || r.Password != "" {
		t.Fatalf("reference: %v %+v", err, r)
	}

	rows, err := s.Store.RecentAudit(ctx, 20)
	if err != nil || len(rows) < 4 {
		t.Fatalf("audit: %v %d", err, len(rows))
	}
	for _, row := range rows {
		if strings.Contains(row.Detail, "apt install") {
			t.Fatal("audit stored command payload by default")
		}
	}
	clients, _ := s.Store.ListClients(ctx)
	if len(clients) != 1 || clients[0].Name != "alice-laptop" || clients[0].LastSeen.IsZero() {
		t.Fatalf("clients = %+v", clients)
	}

	// Revocation takes effect immediately.
	if n, err := s.Store.RevokeClient(ctx, "alice-laptop"); err != nil || n != 1 {
		t.Fatalf("revoke: %d %v", n, err)
	}
	if err := eng.Ping(ctx); err == nil {
		t.Fatal("revoked client still accepted")
	}
}

func TestManagerModeAndVersionGate(t *testing.T) {
	ts, s, _, fs := newTestServer(t, secrets.ModeManager)
	ctx := context.Background()
	eng := enroll(t, ts, s, "ops-box")
	r, err := eng.StoreCredential(ctx, secrets.Credential{Host: "db-01", Username: "postgres", Password: "pw"})
	if err != nil || r.Mode != secrets.ModeManager || r.Ref != "fakevault://db-01/postgres" || r.Password != "" {
		t.Fatalf("manager: %v %+v", err, r)
	}
	if len(fs.puts) != 1 || fs.puts[0].Password != "pw" {
		t.Fatalf("store not called: %+v", fs.puts)
	}
	v, err := eng.Version(ctx)
	if err != nil || v.CredentialMode != secrets.ModeManager || v.ManagerProvider != "fakevault" {
		t.Fatalf("version: %v %+v", err, v)
	}

	old := *eng.Client
	old.ClientVersion = "0.1.0"
	if err := client.NewEngine(&old).Ping(ctx); err == nil || !strings.Contains(err.Error(), "426") {
		t.Fatalf("expected upgrade_required, got %v", err)
	}
}

func TestRateLimitAndBodyLimit(t *testing.T) {
	ts, s, _, _ := newTestServer(t, secrets.ModeReference)
	s.limiter = newLimiter(0.0001, 2)
	ctx := context.Background()
	eng := enroll(t, ts, s, "spammy")
	var rateErr error
	for i := 0; i < 5; i++ {
		if err := eng.Ping(ctx); err != nil {
			rateErr = err
			break
		}
	}
	if !errors.Is(rateErr, llm.ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", rateErr)
	}

	s.limiter = newLimiter(100, 100)
	big := strings.Repeat("x", int(s.Cfg.Limits.MaxBodyBytes)+10)
	_, err := eng.Publish(ctx, docs.Document{ID: "d", Title: "t", BodyMD: big})
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized body, got %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	c.Listen = "0.0.0.0:8443"
	if err := c.Validate(); err == nil {
		t.Fatal("LAN listen without TLS must be refused")
	}
	c.InsecureHTTP = true
	if err := c.Validate(); err != nil {
		t.Fatalf("insecure_http should allow it: %v", err)
	}
	c.InsecureHTTP = false
	c.TLS.Cert = "/c.pem"
	if err := c.Validate(); err == nil {
		t.Fatal("cert without key must be refused")
	}
	c.TLS.Key = "/k.pem"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Credentials.Mode = "manager"
	if err := c.Validate(); err == nil {
		t.Fatal("manager mode without provider must be refused")
	}
}
