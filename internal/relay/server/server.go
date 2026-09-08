package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/relay/api"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/summarize"
)

// Server is the relay HTTP service.
type Server struct {
	Cfg     *Config
	Store   *Store
	LLM     llm.Provider
	Docs    docs.Provider
	Policy  secrets.Policy
	Version string
	Log     *slog.Logger

	limiter *limiter
	mux     *http.ServeMux
}

type ctxKey int

const clientKey ctxKey = 1

// New wires the handlers.
func New(cfg *Config, store *Store, lp llm.Provider, dp docs.Provider, pol secrets.Policy, version string) *Server {
	s := &Server{Cfg: cfg, Store: store, LLM: lp, Docs: dp, Policy: pol, Version: version, Log: slog.Default()}
	s.limiter = newLimiter(cfg.Limits.RequestsPerSecond, cfg.Limits.Burst)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathHealthz, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET "+api.PathReadyz, s.handleReadyz)
	mux.HandleFunc("GET "+api.PathVersion, s.handleVersion)
	mux.HandleFunc("POST "+api.PathEnroll, s.handleEnroll)
	mux.Handle("GET "+api.PathConfig, s.auth(http.HandlerFunc(s.handleConfig)))
	mux.Handle("POST "+api.PathSummarize, s.auth(http.HandlerFunc(s.handleSummarize)))
	mux.Handle("POST "+api.PathExplain, s.auth(http.HandlerFunc(s.handleExplain)))
	mux.Handle("POST "+api.PathPublish, s.auth(http.HandlerFunc(s.handlePublish)))
	mux.Handle("POST "+api.PathDocGet, s.auth(http.HandlerFunc(s.handleDocGet)))
	mux.Handle("GET "+api.PathDocs+"{id}", s.auth(http.HandlerFunc(s.handleDocFind)))
	mux.Handle("POST "+api.PathCredentials, s.auth(http.HandlerFunc(s.handleCredentials)))
	s.mux = mux
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return http.MaxBytesHandler(s.logging(s.mux), s.Cfg.Limits.MaxBodyBytes)
}

// ListenAndServe runs the server until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      10 * time.Minute, // LLM calls can be slow
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", s.Cfg.Listen)
	if err != nil {
		return err
	}
	if s.Cfg.HasTLS() {
		tcfg := &tls.Config{MinVersion: tls.VersionTLS12}
		cert, err := tls.LoadX509KeyPair(s.Cfg.TLS.Cert, s.Cfg.TLS.Key)
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		tcfg.Certificates = []tls.Certificate{cert}
		if s.Cfg.TLS.ClientCA != "" {
			pem, err := os.ReadFile(s.Cfg.TLS.ClientCA)
			if err != nil {
				return fmt.Errorf("tls.client_ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return errors.New("tls.client_ca: no certificates found")
			}
			tcfg.ClientCAs = pool
			tcfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
		ln = tls.NewListener(ln, tcfg)
		s.Log.Info("relay listening", "addr", s.Cfg.Listen, "tls", true, "mtls", s.Cfg.TLS.ClientCA != "")
	} else {
		s.Log.Info("relay listening", "addr", s.Cfg.Listen, "tls", false)
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- middleware -------------------------------------------------------------

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		s.Log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", rw.status, "ms", time.Since(start).Milliseconds(), "ip", clientIP(r))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			writeErr(w, 401, "missing bearer token", "unauthorized")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		c, err := s.Store.Authenticate(r.Context(), token)
		if err != nil {
			writeErr(w, 401, "invalid or revoked token", "unauthorized")
			return
		}
		if !s.limiter.allow(c.ID) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, 429, "rate limited", "rate_limited")
			return
		}
		if s.Cfg.ClientDefaults.MinClientVersion != "" && r.Header.Get("X-Codeument-Version") != "" && versionLess(r.Header.Get("X-Codeument-Version"), s.Cfg.ClientDefaults.MinClientVersion) {
			writeErr(w, 426, "client version "+r.Header.Get("X-Codeument-Version")+" is older than the relay minimum "+s.Cfg.ClientDefaults.MinClientVersion, "upgrade_required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey, c)))
	})
}

func clientFrom(r *http.Request) *Client {
	c, _ := r.Context().Value(clientKey).(*Client)
	return c
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		return strings.TrimSpace(strings.Split(xf, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- handlers ---------------------------------------------------------------

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	problems := map[string]string{}
	if err := s.LLM.Ping(ctx); err != nil {
		problems["llm"] = err.Error()
	}
	if err := s.Docs.Ping(ctx); err != nil {
		problems["docs"] = err.Error()
	}
	if s.Policy.Store != nil {
		if err := s.Policy.Store.Ping(ctx); err != nil {
			problems["password_manager"] = err.Error()
		}
	}
	if len(problems) > 0 {
		writeJSON(w, 503, map[string]any{"status": "degraded", "problems": problems})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ready"})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	v := api.VersionResponse{Version: s.Version, LLMProvider: s.LLM.Name(), LLMModel: s.LLM.Model(), DocsProvider: s.Docs.Name(), CredentialMode: s.Policy.Mode, MinClientVersion: s.Cfg.ClientDefaults.MinClientVersion}
	if s.Policy.Store != nil {
		v.ManagerProvider = s.Policy.Store.Name()
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	over, err := s.Store.EnrollAttempt(r.Context(), clientIP(r), 10)
	if err != nil {
		writeErr(w, 500, err.Error(), "internal")
		return
	}
	if over {
		writeErr(w, 429, "too many enrollment attempts, try again in a minute", "rate_limited")
		return
	}
	var req api.EnrollRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	c, token, err := s.Store.Enroll(r.Context(), req.Code, Client{Hostname: req.Hostname, Username: req.Username, OS: req.OS, Version: req.Version, Scope: req.Scope})
	if errors.Is(err, ErrNotFound) {
		_ = s.Store.Audit(r.Context(), AuditEntry{Endpoint: api.PathEnroll, Status: 403, Detail: "bad code from " + clientIP(r)})
		writeErr(w, 403, "enrollment code is invalid, expired or already used", "bad_code")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error(), "internal")
		return
	}
	_ = s.Store.Audit(r.Context(), AuditEntry{ClientID: c.ID, Endpoint: api.PathEnroll, Status: 200, Detail: c.Name + " " + req.Hostname})
	s.Log.Info("client enrolled", "id", c.ID, "name", c.Name, "host", req.Hostname)
	writeJSON(w, 200, api.EnrollResponse{ClientID: c.ID, Token: token, Name: c.Name})
}

// Defaults builds the client defaults from the relay config.
func (s *Server) Defaults() engine.Defaults {
	cd := s.Cfg.ClientDefaults
	d := engine.Defaults{
		Hints: cd.Hints, DefaultLocation: s.Cfg.Docs.DefaultLocation, SnapshotLocation: cd.SnapshotLocation,
		Ignore: cd.Ignore, Unignore: cd.Unignore, Weights: cd.Weights, ExtraPatterns: cd.ExtraPatterns,
		CredentialMode: s.Policy.Mode, CaptureCreds: s.Policy.CaptureFromCommands, PromptOnReview: s.Policy.PromptOnReview,
		CredentialRefs: cd.CredentialReferences, MinClientVersion: cd.MinClientVersion,
	}
	if s.Policy.Store != nil {
		d.ManagerName = s.Policy.Store.Name()
	}
	return d
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r)
	_ = s.Store.Audit(r.Context(), AuditEntry{ClientID: c.ID, Endpoint: api.PathConfig, Status: 200})
	writeJSON(w, 200, s.Defaults())
}

func (s *Server) handleSummarize(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r)
	var req api.SummarizeRequest
	size, err := decodeSized(r, &req)
	if err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	in := req.Input
	if len(in.Hints) == 0 {
		in.Hints = s.Cfg.ClientDefaults.Hints
	}
	if in.DefaultLocation.Space == "" {
		in.DefaultLocation = s.Cfg.Docs.DefaultLocation
	}
	out, err := summarize.Summarize(r.Context(), s.LLM, in)
	entry := AuditEntry{ClientID: c.ID, Endpoint: api.PathSummarize, Bytes: size, EventCount: len(in.Batch.Events)}
	if s.Cfg.Audit.StorePayloads {
		if b, err := json.Marshal(in); err == nil {
			entry.Payload = string(b)
		}
	}
	if err != nil {
		entry.Status, entry.Detail = llmStatus(err), err.Error()
		_ = s.Store.Audit(r.Context(), entry)
		writeErr(w, entry.Status, err.Error(), llmCode(err))
		return
	}
	entry.Status, entry.TokensIn, entry.TokensOut = 200, out.Usage.InputTokens, out.Usage.OutputTokens
	_ = s.Store.Audit(r.Context(), entry)
	writeJSON(w, 200, api.SummarizeResponse{Output: *out})
}

func (s *Server) handleExplain(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r)
	var req api.ExplainRequest
	size, err := decodeSized(r, &req)
	if err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	if len(req.Input.Hints) == 0 {
		req.Input.Hints = s.Cfg.ClientDefaults.Hints
	}
	out, err := explain.Run(r.Context(), s.LLM, req.Input)
	entry := AuditEntry{ClientID: c.ID, Endpoint: api.PathExplain, Bytes: size, EventCount: len(req.Input.Dirs)}
	if err != nil {
		entry.Status, entry.Detail = llmStatus(err), err.Error()
		_ = s.Store.Audit(r.Context(), entry)
		writeErr(w, entry.Status, err.Error(), llmCode(err))
		return
	}
	entry.Status = 200
	_ = s.Store.Audit(r.Context(), entry)
	writeJSON(w, 200, api.ExplainResponse{Explanations: out})
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r)
	var req api.PublishRequest
	size, err := decodeSized(r, &req)
	if err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	doc := req.Document
	if doc.ID == "" || doc.Title == "" {
		writeErr(w, 400, "document needs an id and a title", "bad_request")
		return
	}
	if doc.Location.Space == "" {
		doc.Location = s.Cfg.Docs.DefaultLocation
	}
	if doc.Meta == nil {
		doc.Meta = map[string]string{}
	}
	doc.Meta["relay_client"] = c.Name
	if s.Policy.Mode == secrets.ModeInline && len(s.Policy.RestrictGroups) > 0 && doc.Meta["kind"] == "server" {
		doc.RestrictToGroups = s.Policy.RestrictGroups
	}
	if _, err := s.Docs.EnsureHierarchy(r.Context(), doc.Location); err != nil {
		_ = s.Store.Audit(r.Context(), AuditEntry{ClientID: c.ID, Endpoint: api.PathPublish, Bytes: size, Status: 502, Detail: err.Error()})
		writeErr(w, 502, "ensure hierarchy: "+err.Error(), "docs_error")
		return
	}
	ref, err := s.Docs.Upsert(r.Context(), doc)
	if err != nil {
		_ = s.Store.Audit(r.Context(), AuditEntry{ClientID: c.ID, Endpoint: api.PathPublish, Bytes: size, Status: 502, Detail: err.Error()})
		writeErr(w, 502, err.Error(), "docs_error")
		return
	}
	_ = s.Store.Audit(r.Context(), AuditEntry{ClientID: c.ID, Endpoint: api.PathPublish, Bytes: size, Status: 200, Detail: doc.ID + " " + doc.Title})
	writeJSON(w, 200, api.PublishResponse{PageRef: ref})
}

func (s *Server) handleDocFind(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ref, err := s.Docs.Find(r.Context(), id)
	if err != nil {
		writeErr(w, 502, err.Error(), "docs_error")
		return
	}
	writeJSON(w, 200, api.FindResponse{PageRef: ref})
}

func (s *Server) handleDocGet(w http.ResponseWriter, r *http.Request) {
	var req api.DocGetRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	doc, err := s.Docs.Get(r.Context(), req.Ref)
	if errors.Is(err, docs.ErrNotFound) {
		writeErr(w, 404, "page not found", "not_found")
		return
	}
	if err != nil {
		writeErr(w, 502, err.Error(), "docs_error")
		return
	}
	writeJSON(w, 200, api.DocGetResponse{Document: doc})
}

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request) {
	c := clientFrom(r)
	var req api.CredentialRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error(), "bad_request")
		return
	}
	cred := req.Credential
	if cred.Host == "" || cred.Username == "" {
		writeErr(w, 400, "credential needs host and username", "bad_request")
		return
	}
	if cred.Password == "" {
		writeJSON(w, 200, api.CredentialResponse{Reference: s.Policy.ReferenceOnly(cred.Host, cred.Username)})
		return
	}
	ref, err := s.Policy.Apply(r.Context(), cred)
	entry := AuditEntry{ClientID: c.ID, Endpoint: api.PathCredentials, Detail: cred.Host + "/" + cred.Username}
	if err != nil {
		entry.Status = 502
		if errors.Is(err, secrets.ErrNoStore) || errors.Is(err, secrets.ErrPolicy) {
			entry.Status = 403
		}
		_ = s.Store.Audit(r.Context(), entry)
		writeErr(w, entry.Status, err.Error(), "credential_policy")
		return
	}
	entry.Status = 200
	_ = s.Store.Audit(r.Context(), entry)
	writeJSON(w, 200, api.CredentialResponse{Reference: ref})
}

// --- helpers ----------------------------------------------------------------

func decode(r *http.Request, v any) error {
	_, err := decodeSized(r, v)
	return err
}

func decodeSized(r *http.Request, v any) (int64, error) {
	cr := &countingReader{r: r.Body}
	dec := json.NewDecoder(cr)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return cr.n, fmt.Errorf("request body too large (limit %d bytes)", mbe.Limit)
		}
		return cr.n, fmt.Errorf("invalid JSON body: %w", err)
	}
	return cr.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, api.Error{Error: msg, Code: code})
}

func llmStatus(err error) int {
	switch {
	case errors.Is(err, llm.ErrRateLimited):
		return 429
	case errors.Is(err, llm.ErrRefused):
		return 422
	case errors.Is(err, llm.ErrUnavailable):
		return 503
	}
	return 502
}

func llmCode(err error) string {
	switch {
	case errors.Is(err, llm.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, llm.ErrRefused):
		return "refused"
	case errors.Is(err, llm.ErrUnavailable):
		return "llm_unavailable"
	case errors.Is(err, llm.ErrInvalidJSON):
		return "invalid_output"
	}
	return "llm_error"
}

// versionLess compares dotted versions loosely ("v1.2.3" < "1.3").
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "dev" || v == "" {
		return [3]int{1 << 30, 0, 0} // dev builds are never "too old"
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		n := 0
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}

// --- rate limiter -----------------------------------------------------------

type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// BuildProviders constructs the relay's providers from its config.
func BuildProviders(cfg *Config, newStore func(config.Credentials) (secrets.Store, error)) (llm.Provider, docs.Provider, secrets.Policy, error) {
	var lc llm.Config
	lc.Provider = cfg.LLM.Provider
	lc.Anthropic.APIKey, lc.Anthropic.Model, lc.Anthropic.MaxTokens, lc.Anthropic.Fallbacks = cfg.LLM.Anthropic.APIKey, cfg.LLM.Anthropic.Model, cfg.LLM.Anthropic.MaxTokens, cfg.LLM.Anthropic.Fallbacks
	lc.Ollama.BaseURL, lc.Ollama.Model, lc.Ollama.NumCtx = cfg.LLM.Ollama.BaseURL, cfg.LLM.Ollama.Model, cfg.LLM.Ollama.NumCtx
	lp, err := llm.New(lc)
	if err != nil {
		return nil, nil, secrets.Policy{}, err
	}
	var dc docs.Config
	dc.Provider = cfg.Docs.Provider
	dc.Markdown.Root = cfg.Docs.Markdown.Root
	dc.Confluence.BaseURL, dc.Confluence.Email, dc.Confluence.APIToken, dc.Confluence.Space = cfg.Docs.Confluence.BaseURL, cfg.Docs.Confluence.Email, cfg.Docs.Confluence.APIToken, cfg.Docs.Confluence.Space
	dc.DefaultLocation = cfg.Docs.DefaultLocation
	dp, err := docs.New(dc)
	if err != nil {
		return nil, nil, secrets.Policy{}, err
	}
	pol := secrets.Policy{Mode: cfg.Credentials.Mode, CaptureFromCommands: cfg.Credentials.CaptureFromCommands, PromptOnReview: cfg.Credentials.PromptOnReview, References: cfg.ClientDefaults.CredentialReferences, RestrictGroups: cfg.Credentials.Inline.PageRestrictionGroups}
	if cfg.Credentials.Manager.Provider != "" && newStore != nil {
		st, err := newStore(cfg.Credentials)
		if err != nil {
			return nil, nil, secrets.Policy{}, err
		}
		pol.Store = st
	}
	return lp, dp, pol, nil
}

// OSName is a short OS identifier for enrollment.
func OSName() string { return runtime.GOOS + "/" + runtime.GOARCH }
