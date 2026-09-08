// Package api holds the wire types shared by the relay server and client.
// Every request/response is JSON; the version is part of the path (/v1).
package api

import (
	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/summarize"
)

// Paths.
const (
	PathEnroll      = "/v1/enroll"
	PathConfig      = "/v1/config"
	PathSummarize   = "/v1/summarize"
	PathExplain     = "/v1/explain"
	PathPublish     = "/v1/publish"
	PathDocs        = "/v1/docs/"
	PathDocGet      = "/v1/docs/get"
	PathCredentials = "/v1/credentials"
	PathVersion     = "/v1/version"
	PathHealthz     = "/healthz"
	PathReadyz      = "/readyz"
)

// Error is the JSON error body.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// EnrollRequest joins a client to the relay with a one-time code.
type EnrollRequest struct {
	Code     string `json:"code"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	OS       string `json:"os"`
	Version  string `json:"version"`
	Scope    string `json:"scope"` // user | system
}

// EnrollResponse returns the client's credentials.
type EnrollResponse struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
	Name     string `json:"name"`
}

// ClientDefaults are pushed to clients.
type ClientDefaults = engine.Defaults

// SummarizeRequest carries the summarizer input.
type SummarizeRequest struct {
	Input summarize.Input `json:"input"`
}

// SummarizeResponse carries the draft.
type SummarizeResponse struct {
	Output summarize.Output `json:"output"`
}

// ExplainRequest carries directory dossiers.
type ExplainRequest struct {
	Input explain.Input `json:"input"`
}

// ExplainResponse carries explanations.
type ExplainResponse struct {
	Explanations []explain.Explanation `json:"explanations"`
}

// PublishRequest carries a document.
type PublishRequest struct {
	Document docs.Document `json:"document"`
}

// PublishResponse carries the page reference.
type PublishResponse struct {
	PageRef docs.PageRef `json:"page_ref"`
}

// DocGetRequest fetches a page by reference.
type DocGetRequest struct {
	Ref docs.PageRef `json:"ref"`
}

// DocGetResponse carries the page.
type DocGetResponse struct {
	Document *docs.Document `json:"document"`
}

// FindResponse carries an optional page reference.
type FindResponse struct {
	PageRef *docs.PageRef `json:"page_ref"`
}

// CredentialRequest stores a credential per the relay's policy.
type CredentialRequest struct {
	Credential secrets.Credential `json:"credential"`
}

// CredentialResponse is the resulting reference.
type CredentialResponse struct {
	Reference secrets.Reference `json:"reference"`
}

// VersionResponse describes the relay.
type VersionResponse struct {
	Version          string `json:"version"`
	LLMProvider      string `json:"llm_provider"`
	LLMModel         string `json:"llm_model"`
	DocsProvider     string `json:"docs_provider"`
	CredentialMode   string `json:"credential_mode"`
	ManagerProvider  string `json:"manager_provider,omitempty"`
	MinClientVersion string `json:"min_client_version,omitempty"`
}
