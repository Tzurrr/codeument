// Package journal is the local SQLite store for events, batches, drafts and
// snapshots. It uses the pure-Go modernc.org/sqlite driver so the binary
// stays cgo-free.
package journal

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // sqlite driver

	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/paths"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (creating when needed) the journal at path and runs migrations.
func Open(path string) (*Store, error) {
	if err := paths.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenMemory opens an in-memory journal (tests).
func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying handle for packages that need custom queries.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	current := 0
	if v, err := s.GetKV(ctx, "schema_version"); err == nil && v != "" {
		fmt.Sscanf(v, "%d", &current) //nolint:errcheck // best effort, defaults to 0
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		sqlText, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(sqlText)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kv(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprint(version)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// GetKV reads a key; ErrNotFound when absent.
func (s *Store) GetKV(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetKV writes a key.
func (s *Store) SetKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// DeleteKV removes a key.
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key=?`, key)
	return err
}

// --- sessions ---------------------------------------------------------------

// UpsertSession records a session on first sight.
func (s *Store) UpsertSession(ctx context.Context, sess model.Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id,started_at,hostname,username,shell,pid) VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`, sess.ID, ns(sess.StartedAt), sess.Hostname, sess.Username, sess.Shell, sess.PID)
	return err
}

// EndSession marks a session ended.
func (s *Store) EndSession(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET ended_at=? WHERE id=?`, ns(at), id)
	return err
}

// ListSessions returns the most recent sessions with event counts.
func (s *Store) ListSessions(ctx context.Context, limit int) ([]model.Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, s.started_at, s.ended_at, s.hostname, s.username, s.shell, s.pid,
		(SELECT COUNT(*) FROM events e WHERE e.session_id=s.id)
		FROM sessions s ORDER BY s.started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Session
	for rows.Next() {
		var sess model.Session
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&sess.ID, &started, &ended, &sess.Hostname, &sess.Username, &sess.Shell, &sess.PID, &sess.EventCount); err != nil {
			return nil, err
		}
		sess.StartedAt = fromNS(started)
		if ended.Valid {
			t := fromNS(ended.Int64)
			sess.EndedAt = &t
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// --- events -----------------------------------------------------------------

// InsertEvent stores an event, assigning its sequence number within the
// session. The caller sets BatchID beforehand.
func (s *Store) InsertEvent(ctx context.Context, ev *model.Event) error {
	if ev.Seq == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE session_id=?`, ev.SessionID).Scan(&ev.Seq); err != nil {
			return err
		}
	}
	files, _ := json.Marshal(nz(ev.FilesTouched))
	reds, _ := json.Marshal(nz(ev.Redactions))
	res, err := s.db.ExecContext(ctx, `INSERT INTO events(session_id,seq,ts_start,ts_end,duration_ms,command,exit_code,cwd,git_root,git_branch,git_head,hostname,username,shell,kind,family,weight,milestone,files_touched,redactions,batch_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		ev.SessionID, ev.Seq, ns(ev.Start), ns(ev.End), ev.Duration().Milliseconds(), ev.Command, ev.ExitCode, ev.CWD,
		ev.GitRoot, ev.GitBranch, ev.GitHead, ev.Hostname, ev.Username, ev.Shell, string(ev.Kind), ev.Family, ev.Weight,
		boolInt(ev.Milestone), string(files), string(reds), ev.BatchID)
	if err != nil {
		return err
	}
	ev.ID, err = res.LastInsertId()
	return err
}

// EventQuery filters ListEvents.
type EventQuery struct {
	SessionID string
	BatchID   string
	Since     time.Time
	Until     time.Time
	Limit     int
	Desc      bool
	Kind      model.Kind
}

// ListEvents returns events matching q ordered by time (ascending unless
// Desc).
func (s *Store) ListEvents(ctx context.Context, q EventQuery) ([]model.Event, error) {
	where, args := eventWhere(q)
	order := "ASC"
	if q.Desc {
		order = "DESC"
	}
	sqlText := `SELECT id,session_id,seq,ts_start,ts_end,command,exit_code,cwd,git_root,git_branch,git_head,hostname,username,shell,kind,family,weight,milestone,files_touched,redactions,COALESCE(batch_id,'')
		FROM events ` + where + ` ORDER BY ts_start ` + order + `, id ` + order
	if q.Limit > 0 {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CountEvents counts events matching q.
func (s *Store) CountEvents(ctx context.Context, q EventQuery) (int, error) {
	where, args := eventWhere(q)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events `+where, args...).Scan(&n)
	return n, err
}

// DeleteEvents removes events matching q and returns how many went away. Any
// draft built on an affected batch is marked stale.
func (s *Store) DeleteEvents(ctx context.Context, q EventQuery) (int64, error) {
	where, args := eventWhere(q)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status='stale', updated_at=? WHERE status IN ('draft','accepted') AND batch_id IN (SELECT DISTINCT batch_id FROM events `+where+`)`, append([]any{ns(time.Now())}, args...)...); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events `+where, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE batches SET event_count=(SELECT COUNT(*) FROM events e WHERE e.batch_id=batches.id),
		meaningful_count=(SELECT COUNT(*) FROM events e WHERE e.batch_id=batches.id AND e.kind='meaningful'),
		score=(SELECT COALESCE(SUM(weight),0) FROM events e WHERE e.batch_id=batches.id)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batches WHERE event_count=0 AND status IN ('open','pending_summary','skipped')`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// DeleteLastEvents removes the N most recent events across all sessions.
func (s *Store) DeleteLastEvents(ctx context.Context, n int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status='stale', updated_at=? WHERE status IN ('draft','accepted') AND batch_id IN (SELECT DISTINCT batch_id FROM events ORDER BY ts_start DESC, id DESC LIMIT ?)`, ns(time.Now()), n); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE id IN (SELECT id FROM events ORDER BY ts_start DESC, id DESC LIMIT ?)`, n)
	if err != nil {
		return 0, err
	}
	deleted, _ := res.RowsAffected()
	return deleted, tx.Commit()
}

// PruneEvents deletes events older than cutoff that belong to closed batches.
func (s *Store) PruneEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE ts_start < ? AND (batch_id IS NULL OR batch_id NOT IN (SELECT id FROM batches WHERE status IN ('open','pending_summary','summarizing')))`, ns(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func eventWhere(q EventQuery) (string, []any) {
	var conds []string
	var args []any
	if q.SessionID != "" {
		conds = append(conds, "session_id=?")
		args = append(args, q.SessionID)
	}
	if q.BatchID != "" {
		conds = append(conds, "batch_id=?")
		args = append(args, q.BatchID)
	}
	if !q.Since.IsZero() {
		conds = append(conds, "ts_start>=?")
		args = append(args, ns(q.Since))
	}
	if !q.Until.IsZero() {
		conds = append(conds, "ts_start<?")
		args = append(args, ns(q.Until))
	}
	if q.Kind != "" {
		conds = append(conds, "kind=?")
		args = append(args, string(q.Kind))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(conds, " AND "), args
}

type scanner interface{ Scan(dest ...any) error }

func scanEvent(r scanner) (model.Event, error) {
	var ev model.Event
	var start, end int64
	var kind string
	var milestone int
	var files, reds string
	var cwd, root, branch, head, host, user, shell, family sql.NullString
	if err := r.Scan(&ev.ID, &ev.SessionID, &ev.Seq, &start, &end, &ev.Command, &ev.ExitCode, &cwd, &root, &branch, &head, &host, &user, &shell, &kind, &family, &ev.Weight, &milestone, &files, &reds, &ev.BatchID); err != nil {
		return ev, err
	}
	ev.Start, ev.End = fromNS(start), fromNS(end)
	ev.Kind = model.Kind(kind)
	ev.Milestone = milestone == 1
	ev.CWD, ev.GitRoot, ev.GitBranch, ev.GitHead = cwd.String, root.String, branch.String, head.String
	ev.Hostname, ev.Username, ev.Shell, ev.Family = host.String, user.String, shell.String, family.String
	_ = json.Unmarshal([]byte(files), &ev.FilesTouched)
	_ = json.Unmarshal([]byte(reds), &ev.Redactions)
	return ev, nil
}

// --- batches ----------------------------------------------------------------

// OpenBatch returns the open batch for a session, or ErrNotFound.
func (s *Store) OpenBatch(ctx context.Context, sessionID string) (*model.Batch, error) {
	return s.batchWhere(ctx, `session_id=? AND status='open' ORDER BY started_at DESC LIMIT 1`, sessionID)
}

// GetBatch fetches one batch by id.
func (s *Store) GetBatch(ctx context.Context, id string) (*model.Batch, error) {
	return s.batchWhere(ctx, `id=?`, id)
}

func (s *Store) batchWhere(ctx context.Context, where string, args ...any) (*model.Batch, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,session_id,started_at,last_event_at,closed_at,status,score,meaningful_count,event_count,COALESCE(trigger_reason,''),COALESCE(error,''),attempts FROM batches WHERE `+where, args...)
	b, err := scanBatch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func scanBatch(r scanner) (model.Batch, error) {
	var b model.Batch
	var started, last int64
	var closed sql.NullInt64
	var status string
	if err := r.Scan(&b.ID, &b.SessionID, &started, &last, &closed, &status, &b.Score, &b.MeaningfulCount, &b.EventCount, &b.TriggerReason, &b.Error, &b.Attempts); err != nil {
		return b, err
	}
	b.StartedAt, b.LastEventAt = fromNS(started), fromNS(last)
	if closed.Valid {
		t := fromNS(closed.Int64)
		b.ClosedAt = &t
	}
	b.Status = model.BatchStatus(status)
	return b, nil
}

// CreateBatch inserts a batch.
func (s *Store) CreateBatch(ctx context.Context, b *model.Batch) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO batches(id,session_id,started_at,last_event_at,closed_at,status,score,meaningful_count,event_count,trigger_reason,error,attempts) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.SessionID, ns(b.StartedAt), ns(b.LastEventAt), nsPtr(b.ClosedAt), string(b.Status), b.Score, b.MeaningfulCount, b.EventCount, b.TriggerReason, b.Error, b.Attempts)
	return err
}

// UpdateBatch writes all mutable fields of a batch.
func (s *Store) UpdateBatch(ctx context.Context, b *model.Batch) error {
	_, err := s.db.ExecContext(ctx, `UPDATE batches SET last_event_at=?,closed_at=?,status=?,score=?,meaningful_count=?,event_count=?,trigger_reason=?,error=?,attempts=? WHERE id=?`,
		ns(b.LastEventAt), nsPtr(b.ClosedAt), string(b.Status), b.Score, b.MeaningfulCount, b.EventCount, b.TriggerReason, b.Error, b.Attempts, b.ID)
	return err
}

// ListBatches returns batches in the given statuses, oldest first.
func (s *Store) ListBatches(ctx context.Context, statuses ...model.BatchStatus) ([]model.Batch, error) {
	where := "1=1"
	var args []any
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = "status IN (" + strings.Join(ph, ",") + ")"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,session_id,started_at,last_event_at,closed_at,status,score,meaningful_count,event_count,COALESCE(trigger_reason,''),COALESCE(error,''),attempts FROM batches WHERE `+where+` ORDER BY started_at ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BatchEvents loads the events of a batch in order.
func (s *Store) BatchEvents(ctx context.Context, batchID string) ([]model.Event, error) {
	return s.ListEvents(ctx, EventQuery{BatchID: batchID})
}

// --- drafts -----------------------------------------------------------------

// SaveDraft inserts or replaces a draft record.
func (s *Store) SaveDraft(ctx context.Context, d *model.DraftRecord) error {
	structured, err := json.Marshal(d.Draft)
	if err != nil {
		return err
	}
	loc, _ := json.Marshal(d.Location)
	tags, _ := json.Marshal(nz(d.Tags))
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	d.UpdatedAt = time.Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO drafts(id,batch_id,doc_id,status,title,body_md,structured,location,tags,provider,model,prompt_hash,usage_in,usage_out,hostname,created_at,updated_at,published_at,page_ref)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET batch_id=excluded.batch_id, doc_id=excluded.doc_id, status=excluded.status, title=excluded.title, body_md=excluded.body_md,
		structured=excluded.structured, location=excluded.location, tags=excluded.tags, provider=excluded.provider, model=excluded.model, prompt_hash=excluded.prompt_hash,
		usage_in=excluded.usage_in, usage_out=excluded.usage_out, hostname=excluded.hostname, updated_at=excluded.updated_at, published_at=excluded.published_at, page_ref=excluded.page_ref`,
		d.ID, d.BatchID, d.DocID, string(d.Status), d.Title, d.BodyMD, string(structured), string(loc), string(tags), d.Provider, d.Model, d.PromptHash,
		d.UsageIn, d.UsageOut, d.Hostname, ns(d.CreatedAt), ns(d.UpdatedAt), nsPtr(d.PublishedAt), nullStr(string(d.PageRef)))
	return err
}

// GetDraft fetches one draft.
func (s *Store) GetDraft(ctx context.Context, id string) (*model.DraftRecord, error) {
	rows, err := s.db.QueryContext(ctx, draftSelect+` WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	d, err := scanDraft(rows)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDrafts returns drafts, newest first, optionally filtered by status.
func (s *Store) ListDrafts(ctx context.Context, statuses ...model.DraftStatus) ([]model.DraftRecord, error) {
	where := "1=1"
	var args []any
	if len(statuses) > 0 {
		ph := make([]string, len(statuses))
		for i, st := range statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = "status IN (" + strings.Join(ph, ",") + ")"
	}
	rows, err := s.db.QueryContext(ctx, draftSelect+` WHERE `+where+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DraftRecord
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DraftByBatch returns the newest draft for a batch, or ErrNotFound.
func (s *Store) DraftByBatch(ctx context.Context, batchID string) (*model.DraftRecord, error) {
	rows, err := s.db.QueryContext(ctx, draftSelect+` WHERE batch_id=? ORDER BY created_at DESC LIMIT 1`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	d, err := scanDraft(rows)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

const draftSelect = `SELECT id,COALESCE(batch_id,''),COALESCE(doc_id,''),status,title,body_md,structured,COALESCE(location,'{}'),COALESCE(tags,'[]'),COALESCE(provider,''),COALESCE(model,''),COALESCE(prompt_hash,''),usage_in,usage_out,COALESCE(hostname,''),created_at,updated_at,published_at,page_ref FROM drafts`

func scanDraft(r scanner) (model.DraftRecord, error) {
	var d model.DraftRecord
	var status, structured, loc, tags string
	var created, updated int64
	var published sql.NullInt64
	var pageRef sql.NullString
	if err := r.Scan(&d.ID, &d.BatchID, &d.DocID, &status, &d.Title, &d.BodyMD, &structured, &loc, &tags, &d.Provider, &d.Model, &d.PromptHash, &d.UsageIn, &d.UsageOut, &d.Hostname, &created, &updated, &published, &pageRef); err != nil {
		return d, err
	}
	d.Status = model.DraftStatus(status)
	_ = json.Unmarshal([]byte(structured), &d.Draft)
	_ = json.Unmarshal([]byte(loc), &d.Location)
	_ = json.Unmarshal([]byte(tags), &d.Tags)
	d.CreatedAt, d.UpdatedAt = fromNS(created), fromNS(updated)
	if published.Valid {
		t := fromNS(published.Int64)
		d.PublishedAt = &t
	}
	if pageRef.Valid && pageRef.String != "" {
		d.PageRef = json.RawMessage(pageRef.String)
	}
	return d, nil
}

// RecentDocTitles lists accepted/published drafts (title + doc id) for
// prompt context.
func (s *Store) RecentDocTitles(ctx context.Context, limit int) ([]model.DraftRecord, error) {
	rows, err := s.db.QueryContext(ctx, draftSelect+` WHERE status IN ('accepted','published') ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DraftRecord
	for rows.Next() {
		d, err := scanDraft(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- doc pages --------------------------------------------------------------

// DocPage maps a codeument doc id to a provider page.
type DocPage struct {
	DocID          string
	Provider       string
	ProviderPageID string
	URL            string
	Version        int
	ContentHash    string
	Title          string
	UpdatedAt      time.Time
}

// SaveDocPage upserts a mapping.
func (s *Store) SaveDocPage(ctx context.Context, p DocPage) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO doc_pages(doc_id,provider,provider_page_id,url,version,content_hash,title,updated_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(doc_id) DO UPDATE SET provider=excluded.provider, provider_page_id=excluded.provider_page_id, url=excluded.url, version=excluded.version, content_hash=excluded.content_hash, title=excluded.title, updated_at=excluded.updated_at`,
		p.DocID, p.Provider, p.ProviderPageID, p.URL, p.Version, p.ContentHash, p.Title, ns(time.Now()))
	return err
}

// GetDocPage fetches a mapping.
func (s *Store) GetDocPage(ctx context.Context, docID string) (*DocPage, error) {
	var p DocPage
	var updated int64
	var url, hash, title sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT doc_id,provider,provider_page_id,url,version,content_hash,title,updated_at FROM doc_pages WHERE doc_id=?`, docID).
		Scan(&p.DocID, &p.Provider, &p.ProviderPageID, &url, &p.Version, &hash, &title, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.URL, p.ContentHash, p.Title, p.UpdatedAt = url.String, hash.String, title.String, fromNS(updated)
	return &p, nil
}

// ListDocPages returns all mappings.
func (s *Store) ListDocPages(ctx context.Context) ([]DocPage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT doc_id,provider,provider_page_id,url,version,content_hash,title,updated_at FROM doc_pages ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocPage
	for rows.Next() {
		var p DocPage
		var updated int64
		var url, hash, title sql.NullString
		if err := rows.Scan(&p.DocID, &p.Provider, &p.ProviderPageID, &url, &p.Version, &hash, &title, &updated); err != nil {
			return nil, err
		}
		p.URL, p.ContentHash, p.Title, p.UpdatedAt = url.String, hash.String, title.String, fromNS(updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- publish queue ----------------------------------------------------------

// QueueItem is a pending publish.
type QueueItem struct {
	ID            int64
	DraftID       string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
}

// Enqueue adds a draft to the publish retry queue.
func (s *Store) Enqueue(ctx context.Context, draftID string, next time.Time, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO publish_queue(draft_id,attempts,next_attempt_at,last_error) VALUES(?,0,?,?)`, draftID, ns(next), lastErr)
	return err
}

// DueQueue lists items due for retry.
func (s *Store) DueQueue(ctx context.Context, now time.Time) ([]QueueItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,draft_id,attempts,next_attempt_at,COALESCE(last_error,'') FROM publish_queue WHERE next_attempt_at<=? ORDER BY id`, ns(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueItem
	for rows.Next() {
		var it QueueItem
		var next int64
		if err := rows.Scan(&it.ID, &it.DraftID, &it.Attempts, &next, &it.LastError); err != nil {
			return nil, err
		}
		it.NextAttemptAt = fromNS(next)
		out = append(out, it)
	}
	return out, rows.Err()
}

// RequeueItem records a failed attempt and schedules the next one.
func (s *Store) RequeueItem(ctx context.Context, id int64, next time.Time, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE publish_queue SET attempts=attempts+1, next_attempt_at=?, last_error=? WHERE id=?`, ns(next), lastErr, id)
	return err
}

// DequeueItem removes a queue item.
func (s *Store) DequeueItem(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM publish_queue WHERE id=?`, id)
	return err
}

// --- snapshots --------------------------------------------------------------

// SnapshotRow is a stored snapshot.
type SnapshotRow struct {
	ID        string
	TakenAt   time.Time
	Hostname  string
	MachineID string
	Data      json.RawMessage
	Diff      json.RawMessage
	Published bool
	PageRef   json.RawMessage
}

// SaveSnapshot stores a snapshot.
func (s *Store) SaveSnapshot(ctx context.Context, r SnapshotRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshots(id,taken_at,hostname,machine_id,data,diff,published,page_ref) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET diff=excluded.diff, published=excluded.published, page_ref=excluded.page_ref`,
		r.ID, ns(r.TakenAt), r.Hostname, r.MachineID, string(r.Data), nullStr(string(r.Diff)), boolInt(r.Published), nullStr(string(r.PageRef)))
	return err
}

// LatestSnapshot returns the most recent snapshot for a machine.
func (s *Store) LatestSnapshot(ctx context.Context, machineID string) (*SnapshotRow, error) {
	var r SnapshotRow
	var taken int64
	var data string
	var diff, pageRef sql.NullString
	var published int
	err := s.db.QueryRowContext(ctx, `SELECT id,taken_at,hostname,machine_id,data,diff,published,page_ref FROM snapshots WHERE machine_id=? ORDER BY taken_at DESC LIMIT 1`, machineID).
		Scan(&r.ID, &taken, &r.Hostname, &r.MachineID, &data, &diff, &published, &pageRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.TakenAt, r.Data, r.Published = fromNS(taken), json.RawMessage(data), published == 1
	if diff.Valid {
		r.Diff = json.RawMessage(diff.String)
	}
	if pageRef.Valid {
		r.PageRef = json.RawMessage(pageRef.String)
	}
	return &r, nil
}

// ListSnapshots returns recent snapshots for a machine, newest first.
func (s *Store) ListSnapshots(ctx context.Context, machineID string, limit int) ([]SnapshotRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,taken_at,hostname,machine_id,data,diff,published,page_ref FROM snapshots WHERE machine_id=? ORDER BY taken_at DESC LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		var taken int64
		var data string
		var diff, pageRef sql.NullString
		var published int
		if err := rows.Scan(&r.ID, &taken, &r.Hostname, &r.MachineID, &data, &diff, &published, &pageRef); err != nil {
			return nil, err
		}
		r.TakenAt, r.Data, r.Published = fromNS(taken), json.RawMessage(data), published == 1
		if diff.Valid {
			r.Diff = json.RawMessage(diff.String)
		}
		if pageRef.Valid {
			r.PageRef = json.RawMessage(pageRef.String)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DirExplanation is a cached LLM explanation of a directory.
type DirExplanation struct {
	Path        string
	ContentHash string
	Explanation json.RawMessage
	UpdatedAt   time.Time
}

// GetDirExplanation fetches the cached explanation for a path.
func (s *Store) GetDirExplanation(ctx context.Context, path string) (*DirExplanation, error) {
	var d DirExplanation
	var updated int64
	var expl string
	err := s.db.QueryRowContext(ctx, `SELECT path,content_hash,explanation,updated_at FROM snapshot_dir_cache WHERE path=?`, path).Scan(&d.Path, &d.ContentHash, &expl, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.Explanation, d.UpdatedAt = json.RawMessage(expl), fromNS(updated)
	return &d, nil
}

// SaveDirExplanation caches an explanation.
func (s *Store) SaveDirExplanation(ctx context.Context, d DirExplanation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshot_dir_cache(path,content_hash,explanation,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET content_hash=excluded.content_hash, explanation=excluded.explanation, updated_at=excluded.updated_at`,
		d.Path, d.ContentHash, string(d.Explanation), ns(time.Now()))
	return err
}

// --- stats ------------------------------------------------------------------

// Stats summarises the journal.
type Stats struct {
	Events      int
	Meaningful  int
	Sessions    int
	OpenBatches int
	Pending     int
	Drafts      int
	FirstEvent  time.Time
	LastEvent   time.Time
}

// Stats computes journal counters.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	var first, last sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM events),
		(SELECT COUNT(*) FROM events WHERE kind='meaningful'),
		(SELECT COUNT(*) FROM sessions),
		(SELECT COUNT(*) FROM batches WHERE status='open'),
		(SELECT COUNT(*) FROM batches WHERE status='pending_summary'),
		(SELECT COUNT(*) FROM drafts WHERE status='draft'),
		(SELECT MIN(ts_start) FROM events),
		(SELECT MAX(ts_start) FROM events)`).Scan(&st.Events, &st.Meaningful, &st.Sessions, &st.OpenBatches, &st.Pending, &st.Drafts, &first, &last)
	if first.Valid {
		st.FirstEvent = fromNS(first.Int64)
	}
	if last.Valid {
		st.LastEvent = fromNS(last.Int64)
	}
	return st, err
}

// Purge deletes everything.
func (s *Store) Purge(ctx context.Context) error {
	for _, t := range []string{"events", "sessions", "batches", "drafts", "doc_pages", "snapshots", "snapshot_dir_cache", "publish_queue"} {
		if _, err := s.db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM kv WHERE key<>'schema_version'`)
	return err
}

func ns(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func nsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ns(*t)
}

func fromNS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
