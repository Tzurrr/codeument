// Package tui is the interactive draft review screen.
package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gopkg.in/yaml.v3"

	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/worker"
)

// Options configure the review screen.
type Options struct {
	Worker *worker.Worker
	Editor string
	// Drafts to show initially (all statuses); the list can be filtered.
	Drafts []model.DraftRecord
	// KnownDocs are existing documents for the "merge into" picker.
	KnownDocs []DocChoice
	// CredentialPrompt, when set, is invoked for server snapshot drafts.
	CredentialPrompt func(ctx context.Context, d *model.DraftRecord) error
}

// DocChoice is an existing document.
type DocChoice struct {
	ID    string
	Title string
}

type mode int

const (
	modeList mode = iota
	modeDetail
	modeInstruction
	modeMerge
	modeConfirm
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("62"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	helpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	headerStyle = lipgloss.NewStyle().Bold(true)
)

type item struct{ d model.DraftRecord }

func (i item) Title() string {
	badge := string(i.d.Status)
	switch i.d.Status {
	case model.DraftStatusDraft:
		badge = warnStyle.Render("draft")
	case model.DraftStatusAccepted:
		badge = okStyle.Render("accepted")
	case model.DraftStatusPublished:
		badge = okStyle.Render("published")
	case model.DraftStatusDismissed, model.DraftStatusStale:
		badge = dimStyle.Render(string(i.d.Status))
	}
	return fmt.Sprintf("[%s] %s", badge, i.d.Title)
}

func (i item) Description() string {
	return fmt.Sprintf("%s · %s · %d cmds · %s · %s", i.d.Draft.DocKind, i.d.Hostname, len(i.d.Draft.Commands), i.d.CreatedAt.Format("Jan 2 15:04"), i.d.Draft.Confidence+" confidence")
}

func (i item) FilterValue() string { return i.d.Title + " " + strings.Join(i.d.Tags, " ") }

type msgDone struct {
	text string
	err  error
	d    *model.DraftRecord
}

type reviewModel struct {
	opts     Options
	mode     mode
	list     list.Model
	view     viewport.Model
	input    textinput.Model
	merge    list.Model
	status   string
	width    int
	height   int
	current  *model.DraftRecord
	busy     bool
	confirm  string
	onYes    func() tea.Cmd
	quitting bool
}

type mergeItem struct{ c DocChoice }

func (m mergeItem) Title() string       { return m.c.Title }
func (m mergeItem) Description() string { return m.c.ID }
func (m mergeItem) FilterValue() string { return m.c.Title }

// Run starts the review UI.
func Run(ctx context.Context, opts Options) error {
	items := make([]list.Item, 0, len(opts.Drafts))
	for _, d := range opts.Drafts {
		items = append(items, item{d: d})
	}
	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 80, 20)
	l.Title = "codeument drafts"
	l.SetShowStatusBar(true)
	l.SetFilteringEnabled(true)
	l.AdditionalShortHelpKeys = func() []key.Binding {
		return []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "open")),
			key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "accept")),
			key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "publish")),
			key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "dismiss")),
		}
	}
	mergeItems := make([]list.Item, 0, len(opts.KnownDocs))
	for _, c := range opts.KnownDocs {
		mergeItems = append(mergeItems, mergeItem{c: c})
	}
	ml := list.New(mergeItems, list.NewDefaultDelegate(), 80, 20)
	ml.Title = "Merge into which document?"
	in := textinput.New()
	in.Placeholder = "e.g. make it shorter, add the rollback step"
	in.CharLimit = 400
	in.Width = 70

	m := reviewModel{opts: opts, list: l, merge: ml, input: in, view: viewport.New(80, 20)}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

func (m reviewModel) Init() tea.Cmd { return nil }

func (m reviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.list.SetSize(msg.Width, msg.Height-2)
		m.merge.SetSize(msg.Width, msg.Height-2)
		m.view.Width = msg.Width
		m.view.Height = msg.Height - 6
		return m, nil
	case msgDone:
		m.busy = false
		if msg.err != nil {
			m.status = errStyle.Render("error: " + msg.err.Error())
		} else {
			m.status = okStyle.Render(msg.text)
		}
		if msg.d != nil {
			m.current = msg.d
			m.replaceItem(*msg.d)
			if m.mode == modeDetail {
				m.view.SetContent(renderDetail(*msg.d, m.width))
			}
		}
		return m, nil
	case editorDoneMsg:
		m.busy = false
		if msg.err != nil {
			m.status = errStyle.Render("edit: " + msg.err.Error())
			return m, nil
		}
		if msg.d != nil {
			m.current = msg.d
			m.replaceItem(*msg.d)
			m.view.SetContent(renderDetail(*msg.d, m.width))
			m.status = okStyle.Render("draft updated")
		}
		return m, nil
	case tea.KeyMsg:
		if m.busy {
			return m, nil
		}
		switch m.mode {
		case modeList:
			return m.updateList(msg)
		case modeDetail:
			return m.updateDetail(msg)
		case modeInstruction:
			return m.updateInstruction(msg)
		case modeMerge:
			return m.updateMerge(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		}
	}
	var cmd tea.Cmd
	switch m.mode {
	case modeList:
		m.list, cmd = m.list.Update(msg)
	case modeDetail:
		m.view, cmd = m.view.Update(msg)
	case modeInstruction:
		m.input, cmd = m.input.Update(msg)
	case modeMerge:
		m.merge, cmd = m.merge.Update(msg)
	}
	return m, cmd
}

func (m *reviewModel) selected() *model.DraftRecord {
	if it, ok := m.list.SelectedItem().(item); ok {
		d := it.d
		return &d
	}
	return nil
}

func (m *reviewModel) replaceItem(d model.DraftRecord) {
	for i, it := range m.list.Items() {
		if x, ok := it.(item); ok && x.d.ID == d.ID {
			m.list.SetItem(i, item{d: d})
			return
		}
	}
}

func (m reviewModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "enter":
		if d := m.selected(); d != nil {
			m.current = d
			m.mode = modeDetail
			m.view.SetContent(renderDetail(*d, m.width))
			m.view.GotoTop()
		}
		return m, nil
	case "a", "p", "d", "e", "r", "m":
		if d := m.selected(); d != nil {
			m.current = d
			return m.action(msg.String())
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m reviewModel) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc", "backspace":
		m.mode = modeList
		return m, nil
	case "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "a", "p", "d", "e", "r", "m":
		return m.action(msg.String())
	}
	var cmd tea.Cmd
	m.view, cmd = m.view.Update(msg)
	return m, cmd
}

func (m reviewModel) action(k string) (tea.Model, tea.Cmd) {
	d := m.current
	if d == nil {
		return m, nil
	}
	w := m.opts.Worker
	switch k {
	case "a":
		m.busy = true
		return m, func() tea.Msg {
			if err := w.Accept(context.Background(), d); err != nil {
				return msgDone{err: err}
			}
			return msgDone{text: "accepted", d: d}
		}
	case "d":
		m.mode = modeConfirm
		m.confirm = "Dismiss \"" + d.Title + "\"? (y/n)"
		m.onYes = func() tea.Cmd {
			return func() tea.Msg {
				if err := w.Dismiss(context.Background(), d); err != nil {
					return msgDone{err: err}
				}
				return msgDone{text: "dismissed", d: d}
			}
		}
		return m, nil
	case "p":
		m.busy = true
		m.status = "publishing…"
		return m, func() tea.Msg {
			if m.opts.CredentialPrompt != nil && d.Draft.DocKind == "server" {
				if err := m.opts.CredentialPrompt(context.Background(), d); err != nil {
					return msgDone{err: err}
				}
			}
			ref, err := w.Publish(context.Background(), d)
			if err != nil {
				_ = w.QueuePublish(context.Background(), d, err)
				return msgDone{err: fmt.Errorf("%v (queued for retry)", err)}
			}
			return msgDone{text: "published: " + ref.URL, d: d}
		}
	case "e":
		m.busy = true
		return m, editInEditor(m.opts, d)
	case "r":
		m.mode = modeInstruction
		m.input.SetValue("")
		m.input.Focus()
		return m, textinput.Blink
	case "m":
		if len(m.opts.KnownDocs) == 0 {
			m.status = warnStyle.Render("no existing documents to merge into yet")
			return m, nil
		}
		m.mode = modeMerge
		return m, nil
	}
	return m, nil
}

func (m reviewModel) updateInstruction(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeDetail
		if m.current == nil {
			m.mode = modeList
		}
		return m, nil
	case "enter":
		instr := strings.TrimSpace(m.input.Value())
		d := m.current
		if instr == "" || d == nil {
			m.mode = modeDetail
			return m, nil
		}
		m.mode = modeDetail
		m.busy = true
		m.status = "regenerating…"
		w := m.opts.Worker
		return m, func() tea.Msg {
			nd, err := w.Regenerate(context.Background(), d, instr)
			if err != nil {
				return msgDone{err: err}
			}
			return msgDone{text: "regenerated", d: nd}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m reviewModel) updateMerge(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.merge.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.merge, cmd = m.merge.Update(msg)
		return m, cmd
	}
	switch msg.String() {
	case "esc", "q":
		m.mode = modeDetail
		return m, nil
	case "enter":
		if it, ok := m.merge.SelectedItem().(mergeItem); ok && m.current != nil {
			d := m.current
			d.DocID = it.c.ID
			m.mode = modeDetail
			m.busy = true
			w := m.opts.Worker
			return m, func() tea.Msg {
				if err := w.Store.SaveDraft(context.Background(), d); err != nil {
					return msgDone{err: err}
				}
				return msgDone{text: "will be appended to: " + it.c.Title, d: d}
			}
		}
	}
	var cmd tea.Cmd
	m.merge, cmd = m.merge.Update(msg)
	return m, cmd
}

func (m reviewModel) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		m.mode = modeList
		if m.current != nil && m.mode == modeDetail {
			m.mode = modeDetail
		}
		m.busy = true
		return m, m.onYes()
	default:
		m.mode = modeList
		m.status = ""
		return m, nil
	}
}

func (m reviewModel) View() string {
	if m.quitting {
		return ""
	}
	var body string
	switch m.mode {
	case modeList:
		body = m.list.View()
	case modeDetail:
		body = m.view.View() + "\n" + helpStyle.Render("↑/↓ scroll · e edit · a accept · p publish · r regenerate · m merge into · d dismiss · esc back")
	case modeInstruction:
		body = headerStyle.Render("How should the draft change?") + "\n\n" + m.input.View() + "\n\n" + helpStyle.Render("enter to regenerate · esc to cancel")
	case modeMerge:
		body = m.merge.View()
	case modeConfirm:
		body = m.list.View() + "\n" + warnStyle.Render(m.confirm)
	}
	if m.status != "" {
		body += "\n" + m.status
	}
	if m.busy {
		body += "\n" + dimStyle.Render("working…")
	}
	return body
}

func renderDetail(d model.DraftRecord, width int) string {
	if width <= 0 {
		width = 80
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(d.Title) + "\n")
	loc := d.Location.Space
	if len(d.Location.ParentPath) > 0 {
		loc += " / " + strings.Join(d.Location.ParentPath, " / ")
	}
	meta := fmt.Sprintf("%s · %s · confidence %s · %s", d.Draft.DocKind, d.Status, d.Draft.Confidence, loc)
	if d.DocID != "" {
		meta += " · doc " + d.DocID
	}
	if len(d.Tags) > 0 {
		meta += " · tags: " + strings.Join(d.Tags, ", ")
	}
	b.WriteString(dimStyle.Render(meta) + "\n")
	if d.Model != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("%s/%s · %d in / %d out tokens", d.Provider, d.Model, d.UsageIn, d.UsageOut)) + "\n")
	}
	b.WriteString(strings.Repeat("─", minInt(width, 100)) + "\n\n")
	b.WriteString(wrap(d.BodyMD, minInt(width-2, 100)))
	return b.String()
}

// editorDoneMsg reports the outcome of an external edit.
type editorDoneMsg struct {
	d   *model.DraftRecord
	err error
}

type editHeader struct {
	Title    string   `yaml:"title"`
	Space    string   `yaml:"space"`
	Parent   []string `yaml:"parent_path"`
	Tags     []string `yaml:"tags"`
	DocID    string   `yaml:"doc_id,omitempty"`
	DocKind  string   `yaml:"doc_kind"`
	Hostname string   `yaml:"hostname,omitempty"`
}

func editInEditor(opts Options, d *model.DraftRecord) tea.Cmd {
	editor := opts.Editor
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	tmp, err := os.CreateTemp("", "codeument-draft-*.md")
	if err != nil {
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	path := tmp.Name()
	content := EditableMarkdown(*d)
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return func() tea.Msg { return editorDoneMsg{err: err} }
	}
	_ = tmp.Close()
	parts := strings.Fields(editor)
	c := exec.Command(parts[0], append(parts[1:], path)...)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		defer func() { _ = os.Remove(path) }()
		if err != nil {
			return editorDoneMsg{err: err}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return editorDoneMsg{err: err}
		}
		if string(data) == content {
			return editorDoneMsg{d: d}
		}
		if err := ApplyEditedMarkdown(d, string(data)); err != nil {
			return editorDoneMsg{err: err}
		}
		if err := opts.Worker.Store.SaveDraft(context.Background(), d); err != nil {
			return editorDoneMsg{err: err}
		}
		return editorDoneMsg{d: d}
	})
}

// EditableMarkdown renders a draft as a Markdown file with a YAML header the
// user can edit.
func EditableMarkdown(d model.DraftRecord) string {
	h := editHeader{Title: d.Title, Space: d.Location.Space, Parent: d.Location.ParentPath, Tags: d.Tags, DocID: d.DocID, DocKind: d.Draft.DocKind, Hostname: d.Hostname}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(h)
	_ = enc.Close()
	return "---\n" + buf.String() + "---\n\n" + strings.TrimSpace(d.BodyMD) + "\n"
}

// ApplyEditedMarkdown parses an edited file back into the draft.
func ApplyEditedMarkdown(d *model.DraftRecord, text string) error {
	body := text
	if strings.HasPrefix(text, "---\n") {
		end := strings.Index(text[4:], "\n---\n")
		if end < 0 {
			return fmt.Errorf("front matter is not terminated by ---")
		}
		var h editHeader
		if err := yaml.Unmarshal([]byte(text[4:4+end]), &h); err != nil {
			return fmt.Errorf("front matter: %w", err)
		}
		if strings.TrimSpace(h.Title) == "" {
			return fmt.Errorf("title must not be empty")
		}
		d.Title = strings.TrimSpace(h.Title)
		d.Location = model.Location{Space: h.Space, ParentPath: h.Parent, PageTitle: h.Title}
		d.Tags = h.Tags
		d.DocID = h.DocID
		if h.DocKind != "" {
			d.Draft.DocKind = h.DocKind
		}
		body = text[4+end+5:]
	}
	d.BodyMD = strings.TrimSpace(body) + "\n"
	d.Draft.Title = d.Title
	d.Draft.Tags = d.Tags
	d.Draft.SuggestedLocation = d.Location
	return nil
}

func wrap(s string, width int) string {
	if width < 20 {
		return s
	}
	var out []string
	inCode := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inCode = !inCode
		}
		if inCode || len(line) <= width {
			out = append(out, line)
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		words := strings.Fields(line)
		cur := strings.Repeat(" ", indent)
		for _, w := range words {
			if len(cur)+len(w)+1 > width && strings.TrimSpace(cur) != "" {
				out = append(out, cur)
				cur = strings.Repeat(" ", indent) + w
				continue
			}
			if strings.TrimSpace(cur) == "" {
				cur += w
			} else {
				cur += " " + w
			}
		}
		out = append(out, cur)
	}
	return strings.Join(out, "\n")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TempDir is where edit files go (exposed for tests).
func TempDir() string { return filepath.Clean(os.TempDir()) }
