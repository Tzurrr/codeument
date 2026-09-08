// Package explain holds the request/response types and prompt for LLM
// explanations of server directories. It is shared by the snapshot module,
// the engine and the relay.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Tzurrr/codeument/internal/llm"
)

// Dossier is what we know about a directory before asking the model.
type Dossier struct {
	Path        string   `json:"path"`
	Listing     []string `json:"listing"`
	SizeBytes   int64    `json:"size_bytes"`
	Heads       []Head   `json:"heads,omitempty"`
	Services    []string `json:"services,omitempty"`
	ContentHash string   `json:"content_hash"`
}

// Head is the redacted first lines of an informative file.
type Head struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// Input is a batch of directories to explain.
type Input struct {
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	Dirs     []Dossier `json:"dirs"`
	Hints    []string  `json:"hints,omitempty"`
}

// Explanation is the model's description of one directory.
type Explanation struct {
	Path         string   `json:"path"`
	Purpose      string   `json:"purpose"`
	WhatRunsHere string   `json:"what_runs_here"`
	HowToOperate string   `json:"how_to_operate"`
	ConfigFiles  []string `json:"config_files"`
	Risks        string   `json:"risks"`
	Confidence   string   `json:"confidence"`
}

// Output wraps the explanations.
type Output struct {
	Explanations []Explanation `json:"explanations"`
}

// Schema is the JSON schema for Output.
var Schema = json.RawMessage(`{
  "type": "object", "additionalProperties": false, "required": ["explanations"],
  "properties": {
    "explanations": {
      "type": "array",
      "items": {
        "type": "object", "additionalProperties": false,
        "required": ["path", "purpose", "what_runs_here", "how_to_operate", "config_files", "risks", "confidence"],
        "properties": {
          "path": {"type": "string"},
          "purpose": {"type": "string"},
          "what_runs_here": {"type": "string"},
          "how_to_operate": {"type": "string"},
          "config_files": {"type": "array", "items": {"type": "string"}},
          "risks": {"type": "string"},
          "confidence": {"type": "string", "enum": ["low", "medium", "high"]}
        }
      }
    }
  }
}`)

const systemPrompt = `You are codeument, documenting a server for the team that operates it.
For each directory you are given a listing, sizes and the first lines of informative files (README, compose files, unit files, configs) with secrets removed.
Explain, briefly and concretely, what the directory is for, what runs from it, how an operator would start/stop/inspect it, which files are its configuration, and what is risky to touch.
Only state what the evidence supports; say "unknown" rather than guessing. Never reproduce values shown as <redacted:...>.
Respond only with JSON matching the schema, one explanation per directory, keeping the exact path strings.`

// BuildPrompt renders the prompts.
func BuildPrompt(in Input) (system, user string, err error) {
	payload, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return "", "", err
	}
	return systemPrompt, "Explain these directories.\n\n" + string(payload), nil
}

// Run asks the provider to explain the directories, in chunks of ten.
func Run(ctx context.Context, p llm.Provider, in Input) ([]Explanation, error) {
	if len(in.Dirs) == 0 {
		return nil, nil
	}
	var out []Explanation
	const chunk = 10
	for i := 0; i < len(in.Dirs); i += chunk {
		end := i + chunk
		if end > len(in.Dirs) {
			end = len(in.Dirs)
		}
		part := in
		part.Dirs = in.Dirs[i:end]
		system, user, err := BuildPrompt(part)
		if err != nil {
			return nil, err
		}
		resp, err := p.GenerateJSON(ctx, llm.Request{System: system, User: user, Schema: Schema})
		if err != nil {
			return nil, err
		}
		var o Output
		if err := json.Unmarshal(resp.JSON, &o); err != nil {
			return nil, fmt.Errorf("%w: %v", llm.ErrInvalidJSON, err)
		}
		byPath := map[string]Explanation{}
		for _, e := range o.Explanations {
			byPath[strings.TrimRight(e.Path, "/")] = e
		}
		for _, d := range part.Dirs {
			e, ok := byPath[strings.TrimRight(d.Path, "/")]
			if !ok {
				e = Explanation{Path: d.Path, Purpose: "unknown", Confidence: "low", ConfigFiles: []string{}}
			}
			e.Path = d.Path
			if e.ConfigFiles == nil {
				e.ConfigFiles = []string{}
			}
			if e.Confidence == "" {
				e.Confidence = "low"
			}
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("explain: empty response")
	}
	return out, nil
}
