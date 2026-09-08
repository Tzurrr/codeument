// Package snapshot captures a server's state, explains its important
// directories with the LLM, diffs it against the previous snapshot and
// renders the "Server: <hostname>" page.
package snapshot

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
)

// Snapshot is the full state of a machine at one point in time.
type Snapshot struct {
	ID         string              `json:"id"`
	TakenAt    time.Time           `json:"taken_at"`
	Hostname   string              `json:"hostname"`
	MachineID  string              `json:"machine_id"`
	OS         collect.OSInfo      `json:"os"`
	Resources  collect.Resources   `json:"resources"`
	Ports      []collect.Port      `json:"ports"`
	Services   []collect.Service   `json:"services"`
	Cron       []collect.CronEntry `json:"cron"`
	Containers []collect.Container `json:"containers"`
	Accounts   []collect.Account   `json:"accounts"`
	Dirs       []Dir               `json:"dirs"`
	Partial    collect.Partial     `json:"partial,omitempty"`
	Duration   time.Duration       `json:"duration"`
}

// Dir is a directory dossier plus its explanation.
type Dir struct {
	explain.Dossier
	Explanation *explain.Explanation `json:"explanation,omitempty"`
}

// AccountView is an account with its credential reference for rendering.
type AccountView struct {
	collect.Account
	Credential secrets.Reference `json:"credential"`
}

// Options configure a snapshot run.
type Options struct {
	Env         collect.Env
	DirInclude  []string
	DirExclude  []string
	MaxDirs     int
	SkipDirs    bool
	SkipCmdInfo bool
}

// Collect gathers everything except explanations.
func Collect(ctx context.Context, opts Options) *Snapshot {
	e := opts.Env
	if e.Run == nil {
		e = collect.Default
	}
	start := time.Now()
	s := &Snapshot{ID: uuid.New().String(), TakenAt: e.Now(), Partial: collect.Partial{}}
	var mu sync.Mutex
	partials := []collect.Partial{}
	newPartial := func() collect.Partial {
		p := collect.Partial{}
		mu.Lock()
		partials = append(partials, p)
		mu.Unlock()
		return p
	}

	s.OS = collect.OS(ctx, e, newPartial())
	s.Hostname, s.MachineID = s.OS.Hostname, s.OS.MachineID

	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	run(func() { s.Resources = collect.ResourcesInfo(ctx, e, newPartial()) })
	run(func() { s.Ports = collect.Ports(ctx, e, newPartial()) })
	run(func() { s.Services = collect.Services(ctx, e, newPartial()) })
	run(func() { s.Cron = collect.Cron(ctx, e, newPartial()) })
	run(func() { s.Containers = collect.Containers(ctx, e, newPartial()) })
	run(func() { s.Accounts = collect.Accounts(ctx, e, newPartial()) })
	wg.Wait()

	if !opts.SkipDirs {
		var svc []string
		for _, x := range s.Services {
			if x.Active == "running" {
				svc = append(svc, x.Name)
			}
		}
		for _, d := range collect.Dirs(ctx, e, collect.DirOptions{Include: opts.DirInclude, Exclude: opts.DirExclude, Services: svc, MaxDirs: opts.MaxDirs}, newPartial()) {
			s.Dirs = append(s.Dirs, Dir{Dossier: d})
		}
	}
	for _, p := range partials {
		for k, v := range p {
			s.Partial.Add(k, v)
		}
	}
	if len(s.Partial) == 0 {
		s.Partial = nil
	}
	s.Duration = time.Since(start)
	return s
}

// Explainer produces directory explanations (the engine).
type Explainer interface {
	Explain(ctx context.Context, in explain.Input) ([]explain.Explanation, error)
}

// ExplainCache remembers explanations by path and content hash.
type ExplainCache interface {
	Get(ctx context.Context, path string) (hash string, expl *explain.Explanation, ok bool)
	Put(ctx context.Context, path, hash string, expl explain.Explanation) error
}

// Explain fills in explanations for directories whose content changed,
// reusing the cache for the rest. Returns how many were sent to the model.
func Explain(ctx context.Context, s *Snapshot, ex Explainer, cache ExplainCache, hints []string) (int, error) {
	var pending []explain.Dossier
	idx := map[string]int{}
	for i := range s.Dirs {
		d := &s.Dirs[i]
		if cache != nil {
			if hash, e, ok := cache.Get(ctx, d.Path); ok && hash == d.ContentHash && e != nil {
				d.Explanation = e
				continue
			}
		}
		idx[d.Path] = i
		pending = append(pending, d.Dossier)
	}
	if len(pending) == 0 || ex == nil {
		return 0, nil
	}
	out, err := ex.Explain(ctx, explain.Input{Hostname: s.Hostname, OS: s.OS.Distro, Dirs: pending, Hints: hints})
	if err != nil {
		return 0, err
	}
	for _, e := range out {
		i, ok := idx[e.Path]
		if !ok {
			continue
		}
		ec := e
		s.Dirs[i].Explanation = &ec
		if cache != nil {
			_ = cache.Put(ctx, e.Path, s.Dirs[i].ContentHash, e)
		}
	}
	return len(pending), nil
}

// Marshal serialises a snapshot.
func (s *Snapshot) Marshal() ([]byte, error) { return json.Marshal(s) }

// Unmarshal parses a snapshot.
func Unmarshal(data []byte) (*Snapshot, error) {
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// DocID is the stable document id for a machine's page.
func DocID(machineID string) string { return "server:" + machineID }

// Title is the page title.
func Title(hostname string) string { return "Server: " + hostname }
