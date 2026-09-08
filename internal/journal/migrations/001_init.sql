CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    hostname    TEXT,
    username    TEXT,
    shell       TEXT,
    pid         INTEGER
);

CREATE TABLE IF NOT EXISTS events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    ts_start      INTEGER NOT NULL,
    ts_end        INTEGER NOT NULL,
    duration_ms   INTEGER NOT NULL,
    command       TEXT NOT NULL,
    exit_code     INTEGER NOT NULL,
    cwd           TEXT,
    git_root      TEXT,
    git_branch    TEXT,
    git_head      TEXT,
    hostname      TEXT,
    username      TEXT,
    shell         TEXT,
    kind          TEXT NOT NULL,
    family        TEXT,
    weight        INTEGER NOT NULL DEFAULT 0,
    milestone     INTEGER NOT NULL DEFAULT 0,
    files_touched TEXT,
    redactions    TEXT,
    batch_id      TEXT
);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id, seq);
CREATE INDEX IF NOT EXISTS events_batch ON events(batch_id);
CREATE INDEX IF NOT EXISTS events_ts ON events(ts_start);

CREATE TABLE IF NOT EXISTS batches (
    id               TEXT PRIMARY KEY,
    session_id       TEXT NOT NULL,
    started_at       INTEGER NOT NULL,
    last_event_at    INTEGER NOT NULL,
    closed_at        INTEGER,
    status           TEXT NOT NULL,
    score            INTEGER NOT NULL DEFAULT 0,
    meaningful_count INTEGER NOT NULL DEFAULT 0,
    event_count      INTEGER NOT NULL DEFAULT 0,
    trigger_reason   TEXT,
    error            TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS batches_session_status ON batches(session_id, status);
CREATE INDEX IF NOT EXISTS batches_status ON batches(status);

CREATE TABLE IF NOT EXISTS drafts (
    id           TEXT PRIMARY KEY,
    batch_id     TEXT,
    doc_id       TEXT,
    status       TEXT NOT NULL,
    title        TEXT NOT NULL,
    body_md      TEXT NOT NULL,
    structured   TEXT NOT NULL,
    location     TEXT,
    tags         TEXT,
    provider     TEXT,
    model        TEXT,
    prompt_hash  TEXT,
    usage_in     INTEGER NOT NULL DEFAULT 0,
    usage_out    INTEGER NOT NULL DEFAULT 0,
    hostname     TEXT,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    published_at INTEGER,
    page_ref     TEXT
);
CREATE INDEX IF NOT EXISTS drafts_status ON drafts(status, created_at);

CREATE TABLE IF NOT EXISTS doc_pages (
    doc_id           TEXT PRIMARY KEY,
    provider         TEXT NOT NULL,
    provider_page_id TEXT NOT NULL,
    url              TEXT,
    version          INTEGER NOT NULL DEFAULT 0,
    content_hash     TEXT,
    title            TEXT,
    updated_at       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS snapshots (
    id         TEXT PRIMARY KEY,
    taken_at   INTEGER NOT NULL,
    hostname   TEXT,
    machine_id TEXT,
    data       TEXT NOT NULL,
    diff       TEXT,
    published  INTEGER NOT NULL DEFAULT 0,
    page_ref   TEXT
);
CREATE INDEX IF NOT EXISTS snapshots_taken ON snapshots(taken_at);

CREATE TABLE IF NOT EXISTS snapshot_dir_cache (
    path         TEXT PRIMARY KEY,
    content_hash TEXT NOT NULL,
    explanation  TEXT NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS publish_queue (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    draft_id        TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT
);

CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
