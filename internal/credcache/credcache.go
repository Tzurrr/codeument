// Package credcache is the encrypted local store for credentials the user
// entered during review or that were captured from command lines. Values are
// sealed with NaCl secretbox under a key file the user owns; the plaintext
// journal never holds them.
package credcache

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/nacl/secretbox"
	_ "modernc.org/sqlite" // driver

	"github.com/Tzurrr/codeument/internal/paths"
	"github.com/Tzurrr/codeument/internal/secrets"
)

// Source says where a credential came from.
const (
	SourceReview  = "review"
	SourceCommand = "command"
	SourceFile    = "file"
)

// ErrNotFound is returned when no credential matches.
var ErrNotFound = errors.New("credcache: not found")

// Entry is a stored credential (decrypted).
type Entry struct {
	ID        int64             `json:"id"`
	Host      string            `json:"host"`
	Username  string            `json:"username"`
	Kind      string            `json:"kind"`
	Password  string            `json:"-"` // never serialised
	Notes     string            `json:"notes,omitempty"`
	Source    string            `json:"source"`
	Program   string            `json:"program,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	PushedAt  *time.Time        `json:"pushed_at,omitempty"`
	Reference secrets.Reference `json:"reference,omitempty"`
}

// Cache is the encrypted store.
type Cache struct {
	db  *sql.DB
	key [32]byte
}

const schema = `
CREATE TABLE IF NOT EXISTS credentials (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    host       TEXT NOT NULL,
    username   TEXT NOT NULL,
    kind       TEXT NOT NULL DEFAULT 'os',
    ciphertext TEXT NOT NULL,
    notes      TEXT,
    source     TEXT NOT NULL,
    program    TEXT,
    created_at INTEGER NOT NULL,
    pushed_at  INTEGER,
    reference  TEXT,
    UNIQUE(host, username, kind)
);`

// Open opens (creating when needed) the cache and its key file.
func Open(dbPath, keyPath string) (*Cache, error) {
	key, err := loadOrCreateKey(keyPath)
	if err != nil {
		return nil, err
	}
	if err := paths.EnsureDir(filepath.Dir(dbPath)); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", dbPath))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	if dbPath != ":memory:" {
		_ = os.Chmod(dbPath, 0o600)
	}
	return &Cache{db: db, key: key}, nil
}

// Close closes the database.
func (c *Cache) Close() error { return c.db.Close() }

func loadOrCreateKey(path string) ([32]byte, error) {
	var key [32]byte
	if path == "" {
		if _, err := rand.Read(key[:]); err != nil {
			return key, err
		}
		return key, nil // in-memory use
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if derr != nil || len(raw) != 32 {
			return key, fmt.Errorf("credcache: key file %s is not a 32-byte base64 key", path)
		}
		copy(key[:], raw)
		if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
			return key, fmt.Errorf("credcache: key file %s is readable by others (chmod 600 it)", path)
		}
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		if _, err := rand.Read(key[:]); err != nil {
			return key, err
		}
		encoded := base64.StdEncoding.EncodeToString(key[:])
		if err := paths.WriteFilePrivate(path, []byte(encoded+"\n")); err != nil {
			return key, err
		}
		return key, nil
	default:
		return key, err
	}
}

func (c *Cache) seal(plaintext string) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	sealed := secretbox.Seal(nonce[:], []byte(plaintext), &nonce, &c.key)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (c *Cache) open(ciphertext string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(raw) < 25 {
		return "", errors.New("credcache: corrupt ciphertext")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	out, ok := secretbox.Open(nil, raw[24:], &nonce, &c.key)
	if !ok {
		return "", errors.New("credcache: cannot decrypt (wrong key file?)")
	}
	return string(out), nil
}

// Put stores or replaces a credential.
func (c *Cache) Put(ctx context.Context, e Entry) error {
	if e.Host == "" || e.Username == "" {
		return errors.New("credcache: host and username are required")
	}
	if e.Password == "" {
		return errors.New("credcache: password is empty")
	}
	if e.Kind == "" {
		e.Kind = "os"
	}
	if e.Source == "" {
		e.Source = SourceReview
	}
	ct, err := c.seal(e.Password)
	if err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO credentials(host,username,kind,ciphertext,notes,source,program,created_at) VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(host,username,kind) DO UPDATE SET ciphertext=excluded.ciphertext, notes=excluded.notes, source=excluded.source, program=excluded.program, created_at=excluded.created_at, pushed_at=NULL, reference=NULL`,
		e.Host, e.Username, e.Kind, ct, e.Notes, e.Source, e.Program, time.Now().Unix())
	return err
}

// Get returns the credential for a host and user.
func (c *Cache) Get(ctx context.Context, host, username string) (*Entry, error) {
	row := c.db.QueryRowContext(ctx, selectCols+` WHERE host=? AND username=? ORDER BY created_at DESC LIMIT 1`, host, username)
	e, err := c.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

const selectCols = `SELECT id,host,username,kind,ciphertext,COALESCE(notes,''),source,COALESCE(program,''),created_at,pushed_at,COALESCE(reference,'') FROM credentials`

type rowScanner interface{ Scan(dest ...any) error }

func (c *Cache) scan(r rowScanner) (*Entry, error) {
	var e Entry
	var ct, refJSON string
	var created int64
	var pushed sql.NullInt64
	if err := r.Scan(&e.ID, &e.Host, &e.Username, &e.Kind, &ct, &e.Notes, &e.Source, &e.Program, &created, &pushed, &refJSON); err != nil {
		return nil, err
	}
	pw, err := c.open(ct)
	if err != nil {
		return nil, err
	}
	e.Password = pw
	e.CreatedAt = time.Unix(created, 0)
	if pushed.Valid {
		t := time.Unix(pushed.Int64, 0)
		e.PushedAt = &t
	}
	if refJSON != "" {
		e.Reference = parseReference(refJSON)
	}
	return &e, nil
}

// List returns every credential, newest first. Passwords are decrypted.
func (c *Cache) List(ctx context.Context) ([]Entry, error) {
	rows, err := c.db.QueryContext(ctx, selectCols+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		e, err := c.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// Suggestions returns credentials captured from commands for a host, so the
// review screen can offer them for an account.
func (c *Cache) Suggestions(ctx context.Context, host string) ([]Entry, error) {
	rows, err := c.db.QueryContext(ctx, selectCols+` WHERE source=? AND (host=? OR host='') ORDER BY created_at DESC LIMIT 20`, SourceCommand, host)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Entry
	for rows.Next() {
		e, err := c.scan(rows)
		if err != nil {
			continue // a corrupt row must not break review
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// MarkPushed records that a credential reached the password manager.
func (c *Cache) MarkPushed(ctx context.Context, id int64, ref secrets.Reference) error {
	_, err := c.db.ExecContext(ctx, `UPDATE credentials SET pushed_at=?, reference=? WHERE id=?`, time.Now().Unix(), formatReference(ref), id)
	return err
}

// Forget deletes credentials; with no filter it wipes the cache.
func (c *Cache) Forget(ctx context.Context, host, username string) (int64, error) {
	var res sql.Result
	var err error
	switch {
	case host == "" && username == "":
		res, err = c.db.ExecContext(ctx, `DELETE FROM credentials`)
	case username == "":
		res, err = c.db.ExecContext(ctx, `DELETE FROM credentials WHERE host=?`, host)
	default:
		res, err = c.db.ExecContext(ctx, `DELETE FROM credentials WHERE host=? AND username=?`, host, username)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Credential converts an entry to the transport type.
func (e Entry) Credential(machineID string) secrets.Credential {
	return secrets.Credential{Host: e.Host, MachineID: machineID, Username: e.Username, Kind: e.Kind, Password: e.Password, Notes: e.Notes}
}

func formatReference(r secrets.Reference) string {
	// Passwords never go into the reference column.
	r.Password = ""
	return strings.Join([]string{r.Mode, r.Ref, r.URL}, "\x1f")
}

func parseReference(s string) secrets.Reference {
	parts := strings.SplitN(s, "\x1f", 3)
	var r secrets.Reference
	if len(parts) > 0 {
		r.Mode = parts[0]
	}
	if len(parts) > 1 {
		r.Ref = parts[1]
	}
	if len(parts) > 2 {
		r.URL = parts[2]
	}
	return r
}
