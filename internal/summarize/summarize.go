// Package summarize builds the prompt for a batch, calls the LLM and
// validates the resulting Draft.
package summarize

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/model"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

var (
	systemTmpl = template.Must(template.New("system").ParseFS(promptFS, "prompts/system.tmpl"))
	userTmpl   = template.Must(template.New("user.tmpl").Funcs(template.FuncMap{"join": strings.Join}).ParseFS(promptFS, "prompts/user.tmpl"))
)

// DocRef is an existing document the model may append to.
type DocRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Input is everything the summarizer needs. It is serialisable so the relay
// can receive it as-is.
type Input struct {
	Batch           model.Batch    `json:"batch"`
	Hostname        string         `json:"hostname"`
	Username        string         `json:"username"`
	RecentDocs      []DocRef       `json:"recent_docs,omitempty"`
	Hints           []string       `json:"hints,omitempty"`
	Note            string         `json:"note,omitempty"`
	DefaultLocation model.Location `json:"default_location"`
	PreviousDraft   *model.Draft   `json:"previous_draft,omitempty"`
	Instruction     string         `json:"instruction,omitempty"`
	GitLog          string         `json:"git_log,omitempty"`
	GitDiffStat     string         `json:"git_diff_stat,omitempty"`
	MaxContextChars int            `json:"max_context_chars,omitempty"`
}

// Output is the summarizer result.
type Output struct {
	Draft      model.Draft `json:"draft"`
	Usage      llm.Usage   `json:"usage"`
	Model      string      `json:"model"`
	Provider   string      `json:"provider"`
	PromptHash string      `json:"prompt_hash"`
}

// DefaultMaxContextChars bounds the user prompt.
const DefaultMaxContextChars = 60000

type promptData struct {
	Hostname, Username, Shell, Window, SessionID string
	Repos, CWDs, Lines, FilesTouched, Hints      []string
	GitLog, GitDiffStat, Note                    string
	RecentDocs                                   []DocRef
	DefaultLocation                              model.Location
	PreviousDraft, Instruction                   string
}

// BuildPrompt renders the system and user prompts and a hash of both.
func BuildPrompt(in Input) (system, user, hash string, err error) {
	var sb bytes.Buffer
	if err := systemTmpl.ExecuteTemplate(&sb, "system.tmpl", nil); err != nil {
		return "", "", "", err
	}
	data := promptData{
		Hostname: in.Hostname, Username: in.Username, SessionID: in.Batch.SessionID,
		Hints: in.Hints, Note: in.Note, RecentDocs: in.RecentDocs, DefaultLocation: in.DefaultLocation,
		GitLog: in.GitLog, GitDiffStat: in.GitDiffStat, Instruction: in.Instruction,
	}
	evs := in.Batch.Events
	if len(evs) > 0 {
		data.Shell = evs[0].Shell
		data.Window = fmt.Sprintf("%s to %s (%s)", evs[0].Start.Format("2006-01-02 15:04"), evs[len(evs)-1].End.Format("15:04"), humanDur(evs[len(evs)-1].End.Sub(evs[0].Start)))
	}
	seenCWD, seenRepo, seenFile := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range evs {
		if !seenCWD[e.CWD] {
			seenCWD[e.CWD] = true
			data.CWDs = append(data.CWDs, e.CWD)
		}
		if e.GitRoot != "" {
			key := e.GitRoot + "@" + e.GitBranch
			if !seenRepo[key] {
				seenRepo[key] = true
				data.Repos = append(data.Repos, fmt.Sprintf("%s (branch %s)", e.GitRoot, e.GitBranch))
			}
		}
		for _, f := range e.FilesTouched {
			if !seenFile[f] {
				seenFile[f] = true
				data.FilesTouched = append(data.FilesTouched, f)
			}
		}
		marker := ""
		if e.Kind == model.KindNoise {
			marker = "(context) "
		}
		data.Lines = append(data.Lines, fmt.Sprintf("[%d] %s exit=%d %s %s %s%s", e.Seq, e.Start.Format("15:04:05"), e.ExitCode, humanDur(e.Duration()), shortCWD(e.CWD), marker, strings.ReplaceAll(e.Command, "\n", " ")))
	}
	if in.PreviousDraft != nil {
		b, _ := json.MarshalIndent(in.PreviousDraft, "", "  ")
		data.PreviousDraft = string(b)
	}
	var ub bytes.Buffer
	if err := userTmpl.ExecuteTemplate(&ub, "user.tmpl", data); err != nil {
		return "", "", "", err
	}
	user = ub.String()
	limit := in.MaxContextChars
	if limit <= 0 {
		limit = DefaultMaxContextChars
	}
	if len(user) > limit {
		user = user[:limit] + "\n[context truncated]\n"
	}
	h := sha256.Sum256([]byte(sb.String() + "\x00" + user))
	return sb.String(), user, hex.EncodeToString(h[:])[:16], nil
}

// Summarize runs the LLM and validates the draft, retrying once with the
// validation error when the output does not fit.
func Summarize(ctx context.Context, p llm.Provider, in Input) (*Output, error) {
	if len(in.Batch.Events) == 0 {
		return nil, errors.New("summarize: batch has no events")
	}
	system, user, hash, err := BuildPrompt(in)
	if err != nil {
		return nil, err
	}
	req := llm.Request{System: system, User: user, Schema: model.DraftSchema()}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := p.GenerateJSON(ctx, req)
		if err != nil {
			if errors.Is(err, llm.ErrInvalidJSON) && attempt == 0 {
				lastErr = err
				req.User = user + "\n\nYour previous reply was not valid JSON. Reply with JSON only."
				continue
			}
			return nil, err
		}
		var d model.Draft
		if err := json.Unmarshal(resp.JSON, &d); err != nil {
			lastErr = fmt.Errorf("%w: %v", llm.ErrInvalidJSON, err)
			req.User = user + "\n\nYour previous reply did not match the schema: " + err.Error() + ". Reply with JSON only."
			continue
		}
		if err := Validate(&d); err != nil {
			lastErr = err
			req.User = user + "\n\nYour previous reply was rejected: " + err.Error() + ". Fix it and reply with JSON only."
			continue
		}
		Normalize(&d, in)
		return &Output{Draft: d, Usage: resp.Usage, Model: resp.Model, Provider: p.Name(), PromptHash: hash}, nil
	}
	return nil, lastErr
}

// Validate enforces the parts of the schema a lenient model may miss.
func Validate(d *model.Draft) error {
	if strings.TrimSpace(d.Title) == "" {
		return errors.New("title is empty")
	}
	if strings.TrimSpace(d.Summary) == "" {
		return errors.New("summary is empty")
	}
	switch d.DocKind {
	case "runbook", "howto", "changelog", "incident", "note":
	case "":
		d.DocKind = "note"
	default:
		return fmt.Errorf("doc_kind %q is not one of runbook, howto, changelog, incident, note", d.DocKind)
	}
	switch d.Confidence {
	case "low", "medium", "high":
	case "":
		d.Confidence = "medium"
	default:
		return fmt.Errorf("confidence %q is not one of low, medium, high", d.Confidence)
	}
	if strings.Contains(d.Title, "<redacted:") {
		return errors.New("title must not contain redaction markers")
	}
	return nil
}

// Normalize fills defaults and nil slices so downstream code can rely on
// them.
func Normalize(d *model.Draft, in Input) {
	if d.SuggestedLocation.Space == "" {
		d.SuggestedLocation.Space = in.DefaultLocation.Space
	}
	if len(d.SuggestedLocation.ParentPath) == 0 {
		d.SuggestedLocation.ParentPath = append([]string{}, in.DefaultLocation.ParentPath...)
	}
	if d.SuggestedLocation.PageTitle == "" {
		d.SuggestedLocation.PageTitle = d.Title
	}
	for _, s := range []*[]string{&d.Commands, &d.AffectedSystems, &d.FilesTouched, &d.Tags, &d.OpenQuestions} {
		if *s == nil {
			*s = []string{}
		}
	}
	if d.Steps == nil {
		d.Steps = []model.Step{}
	}
	for i := range d.Steps {
		if d.Steps[i].Commands == nil {
			d.Steps[i].Commands = []string{}
		}
	}
	d.Title = strings.TrimSpace(d.Title)
}

// RenderMarkdown turns a draft into the page body (without the H1 title).
func RenderMarkdown(d model.Draft, ev []model.Event) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(d.Summary))
	b.WriteString("\n\n")
	if d.Intent != "" && d.Intent != "unclear" {
		fmt.Fprintf(&b, "**Why:** %s\n\n", strings.TrimSpace(d.Intent))
	}
	if len(d.AffectedSystems) > 0 {
		fmt.Fprintf(&b, "**Affected:** %s\n\n", strings.Join(d.AffectedSystems, ", "))
	}
	if len(d.Steps) > 0 {
		b.WriteString("## Steps\n\n")
		for i, s := range d.Steps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(s.Description))
			if len(s.Commands) > 0 {
				b.WriteString("\n   ```bash\n")
				for _, c := range s.Commands {
					b.WriteString("   " + c + "\n")
				}
				b.WriteString("   ```\n")
			}
			if strings.TrimSpace(s.Notes) != "" {
				fmt.Fprintf(&b, "\n   %s\n", strings.TrimSpace(s.Notes))
			}
			b.WriteString("\n")
		}
	} else if len(d.Commands) > 0 {
		b.WriteString("## Commands\n\n```bash\n")
		for _, c := range d.Commands {
			b.WriteString(c + "\n")
		}
		b.WriteString("```\n\n")
	}
	if len(d.FilesTouched) > 0 {
		b.WriteString("## Files\n\n")
		for _, f := range d.FilesTouched {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}
	if len(d.OpenQuestions) > 0 {
		b.WriteString("## Open questions\n\n")
		for _, q := range d.OpenQuestions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
		b.WriteString("\n")
	}
	if len(ev) > 0 {
		host := ev[0].Hostname
		fmt.Fprintf(&b, "---\n\n_Recorded by codeument on %s, %s to %s, %d commands, confidence %s._\n",
			host, ev[0].Start.Format("2006-01-02 15:04"), ev[len(ev)-1].End.Format("15:04"), len(ev), d.Confidence)
	}
	return b.String()
}

// GitContext runs git in the batch's repository to fetch the commit log and
// diff stat between the first and last recorded HEAD. It is best-effort and
// bounded.
func GitContext(ctx context.Context, evs []model.Event) (gitLog, diffStat string) {
	var root, first, last string
	hasCommit := false
	for _, e := range evs {
		if e.GitRoot == "" {
			continue
		}
		if root == "" {
			root, first = e.GitRoot, e.GitHead
		}
		if e.GitRoot == root && e.GitHead != "" {
			last = e.GitHead
		}
		if e.Family == "git" && strings.Contains(e.Command, "commit") {
			hasCommit = true
		}
	}
	if root == "" || first == "" || last == "" || first == last || !hasCommit {
		return "", ""
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", ""
	}
	if st, err := os.Stat(filepath.Join(root, ".git")); err != nil || (!st.IsDir() && st.Size() == 0) {
		return "", ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run := func(args ...string) string {
		cmd := exec.CommandContext(cctx, "git", append([]string{"-C", root, "--no-pager"}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return capLines(string(out), 60)
	}
	return run("log", "--oneline", "--no-decorate", first+".."+last), run("diff", "--stat", first, last)
}

func capLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("... (%d more lines)", len(lines)-n))
	}
	return strings.Join(lines, "\n")
}

func humanDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func shortCWD(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		p = "~" + strings.TrimPrefix(p, h)
	}
	p = strings.ReplaceAll(p, " ", "\\ ")
	if len(p) > 40 {
		return "…" + p[len(p)-39:]
	}
	return p
}
