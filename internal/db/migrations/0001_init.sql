-- M0: the tables M0 actually uses. Nothing more.
--
-- The rest of the §4 data model (packs, assets, tags, jobs, ...) arrives with
-- the milestone that reads it, so its shape is decided by working code rather
-- than guessed at now. §4 calls itself "sketch, not gospel".
--
-- Time convention for the whole schema: INTEGER Unix seconds, UTC. Chosen over
-- ISO-8601 TEXT because Go's driver then needs no format negotiation to scan a
-- timestamp. When reading the database by hand, wrap the column:
--   SELECT datetime(created_at, 'unixepoch') FROM users;
--
-- STRICT tables reject a value of the wrong type instead of silently coercing
-- it, which is worth having on the tables that hold credentials.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    -- Stored already lowercased and trimmed; see auth.NormalizeUsername.
    username      TEXT    NOT NULL UNIQUE,
    -- Full argon2id encoded hash, parameters included, never a bare digest.
    password_hash TEXT    NOT NULL,
    -- §11: keep the column, ship exactly one role. No permission system for
    -- two people who trust each other.
    role          TEXT    NOT NULL DEFAULT 'user',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_login_at INTEGER
) STRICT;

-- §11: server-side session rows, so a logout genuinely invalidates.
CREATE TABLE sessions (
    id              INTEGER PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- SHA-256 of the cookie value. The cookie value itself is never stored, so
    -- a leaked database does not hand over live sessions.
    token_hash      BLOB    NOT NULL UNIQUE,
    created_at      INTEGER NOT NULL,
    -- Absolute expiry: hard ceiling regardless of activity.
    expires_at      INTEGER NOT NULL,
    -- Idle expiry: slides forward as the session is used.
    idle_expires_at INTEGER NOT NULL,
    user_agent      TEXT    NOT NULL DEFAULT '',
    ip              TEXT    NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX sessions_user_id_idx    ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

-- §11: logins, token creation, deletions and metadata edits. M0 writes login
-- events; later milestones add their own actions. Nothing reads it yet, which
-- is fine — the point is that the record exists from the first login onward.
CREATE TABLE audit_log (
    -- ON DELETE SET NULL, not CASCADE: removing a user must not erase the
    -- record of what that user did.
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    action      TEXT    NOT NULL,
    entity      TEXT    NOT NULL DEFAULT '',
    entity_id   TEXT    NOT NULL DEFAULT '',
    detail_json TEXT    NOT NULL DEFAULT '{}',
    ip          TEXT    NOT NULL DEFAULT '',
    at          INTEGER NOT NULL
) STRICT;

CREATE INDEX audit_log_at_idx     ON audit_log(at);
CREATE INDEX audit_log_action_idx ON audit_log(action, at);
