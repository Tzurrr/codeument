package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver

	"github.com/Tzurrr/codeument/internal/paths"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is the relay database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS clients (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    hostname   TEXT,
    username   TEXT,
    os         TEXT,
    version    TEXT,
    scope      TEXT,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER,
    revoked    INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS enroll_codes (
    code_hash  TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);
CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id   TEXT,
    endpoint    TEXT NOT NULL,
    ts          INTEGER NOT NULL,
    bytes       INTEGER NOT NULL DEFAULT 0,
    event_count INTEGER NOT NULL DEFAULT 0,
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    status      INTEGER NOT NULL DEFAULT 0,
    detail      TEXT,
    payload     TEXT
);
CREATE INDEX IF NOT EXISTS audit_ts ON audit_log(ts);
CREATE TABLE IF NOT EXISTS enroll_attempts (
    ip TEXT PRIMARY KEY, count INTEGER NOT NULL, window_start INTEGER NOT NULL
);
`

// OpenStore opens the relay database.
func OpenStore(path string) (*Store, error) {
	if path == ":memory:" {
		db, err := sql.Open("sqlite", "file::memory:?_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		s := &Store{db: db}
		_, err = db.Exec(schema)
		return s, err
	}
	if err := paths.EnsureDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Client is an enrolled client.
type Client struct {
	ID        string
	Name      string
	Hostname  string
	Username  string
	OS        string
	Version   string
	Scope     string
	CreatedAt time.Time
	LastSeen  time.Time
	Revoked   bool
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "=")), nil
}

// NewEnrollCode creates a one-time enrollment code for a named client.
func (s *Store) NewEnrollCode(ctx context.Context, name string, ttl time.Duration) (string, error) {
	raw, err := randomString(10)
	if err != nil {
		return "", err
	}
	code := strings.ToUpper(raw[:4] + "-" + raw[4:8] + "-" + raw[8:12])
	_, err = s.db.ExecContext(ctx, `INSERT INTO enroll_codes(code_hash,name,expires_at) VALUES(?,?,?)`, hash(normalizeCode(code)), name, time.Now().Add(ttl).Unix())
	return code, err
}

func normalizeCode(c string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(c), " ", ""))
}

// Enroll consumes a code and creates a client, returning its bearer token.
func (s *Store) Enroll(ctx context.Context, code string, c Client) (Client, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return c, "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var name string
	var expires int64
	var used sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT name, expires_at, used_at FROM enroll_codes WHERE code_hash=?`, hash(normalizeCode(code))).Scan(&name, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (used.Valid || time.Now().Unix() > expires)) {
		return c, "", ErrNotFound
	}
	if err != nil {
		return c, "", err
	}
	id, err := randomString(8)
	if err != nil {
		return c, "", err
	}
	secret, err := randomString(24)
	if err != nil {
		return c, "", err
	}
	token := "cdm_" + id + "_" + secret
	c.ID, c.Name, c.CreatedAt = id, name, time.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO clients(id,name,token_hash,hostname,username,os,version,scope,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Name, hash(token), c.Hostname, c.Username, c.OS, c.Version, c.Scope, c.CreatedAt.Unix()); err != nil {
		return c, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enroll_codes SET used_at=? WHERE code_hash=?`, time.Now().Unix(), hash(normalizeCode(code))); err != nil {
		return c, "", err
	}
	return c, token, tx.Commit()
}

// Authenticate resolves a bearer token to a client.
func (s *Store) Authenticate(ctx context.Context, token string) (*Client, error) {
	var c Client
	var created int64
	var seen sql.NullInt64
	var revoked int
	err := s.db.QueryRowContext(ctx, `SELECT id,name,COALESCE(hostname,''),COALESCE(username,''),COALESCE(os,''),COALESCE(version,''),COALESCE(scope,''),created_at,last_seen,revoked FROM clients WHERE token_hash=?`, hash(token)).
		Scan(&c.ID, &c.Name, &c.Hostname, &c.Username, &c.OS, &c.Version, &c.Scope, &created, &seen, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if revoked == 1 {
		return nil, ErrNotFound
	}
	c.CreatedAt = time.Unix(created, 0)
	if seen.Valid {
		c.LastSeen = time.Unix(seen.Int64, 0)
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE clients SET last_seen=? WHERE id=?`, time.Now().Unix(), c.ID)
	return &c, nil
}

// ListClients returns all clients.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,COALESCE(hostname,''),COALESCE(username,''),COALESCE(os,''),COALESCE(version,''),COALESCE(scope,''),created_at,last_seen,revoked FROM clients ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		var c Client
		var created int64
		var seen sql.NullInt64
		var revoked int
		if err := rows.Scan(&c.ID, &c.Name, &c.Hostname, &c.Username, &c.OS, &c.Version, &c.Scope, &created, &seen, &revoked); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(created, 0)
		if seen.Valid {
			c.LastSeen = time.Unix(seen.Int64, 0)
		}
		c.Revoked = revoked == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeClient disables a client by id or name.
func (s *Store) RevokeClient(ctx context.Context, idOrName string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE clients SET revoked=1 WHERE id=? OR name=?`, idOrName, idOrName)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AuditEntry is one audit row.
type AuditEntry struct {
	ClientID   string
	Endpoint   string
	Bytes      int64
	EventCount int
	TokensIn   int
	TokensOut  int
	Status     int
	Detail     string
	Payload    string
}

// Audit writes an entry.
func (s *Store) Audit(ctx context.Context, e AuditEntry) error {
	var payload any
	if e.Payload != "" {
		payload = e.Payload
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log(client_id,endpoint,ts,bytes,event_count,tokens_in,tokens_out,status,detail,payload) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.ClientID, e.Endpoint, time.Now().Unix(), e.Bytes, e.EventCount, e.TokensIn, e.TokensOut, e.Status, e.Detail, payload)
	return err
}

// PruneAudit removes entries older than the retention.
func (s *Store) PruneAudit(ctx context.Context, olderThan time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE ts < ?`, time.Now().Add(-olderThan).Unix())
	return err
}

// AuditRow is a read-back audit entry.
type AuditRow struct {
	ID         int64
	ClientID   string
	Endpoint   string
	At         time.Time
	Bytes      int64
	EventCount int
	TokensIn   int
	TokensOut  int
	Status     int
	Detail     string
}

// RecentAudit lists the newest entries.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(client_id,''),endpoint,ts,bytes,event_count,tokens_in,tokens_out,status,COALESCE(detail,'') FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var r AuditRow
		var ts int64
		if err := rows.Scan(&r.ID, &r.ClientID, &r.Endpoint, &ts, &r.Bytes, &r.EventCount, &r.TokensIn, &r.TokensOut, &r.Status, &r.Detail); err != nil {
			return nil, err
		}
		r.At = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnrollAttempt counts enrollment attempts per IP in a sliding minute and
// reports whether the caller is over the limit.
func (s *Store) EnrollAttempt(ctx context.Context, ip string, limit int) (bool, error) {
	now := time.Now().Unix()
	var count int
	var start int64
	err := s.db.QueryRowContext(ctx, `SELECT count, window_start FROM enroll_attempts WHERE ip=?`, ip).Scan(&count, &start)
	if errors.Is(err, sql.ErrNoRows) || now-start > 60 {
		count, start = 0, now
	} else if err != nil {
		return false, err
	}
	count++
	if _, err := s.db.ExecContext(ctx, `INSERT INTO enroll_attempts(ip,count,window_start) VALUES(?,?,?) ON CONFLICT(ip) DO UPDATE SET count=excluded.count, window_start=excluded.window_start`, ip, count, start); err != nil {
		return false, err
	}
	return count > limit, nil
}
