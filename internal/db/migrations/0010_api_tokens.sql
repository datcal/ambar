-- M8: API tokens (§4, §11).
--
-- §11: "store only a hash; show plaintext once at creation; scopes (read, write,
-- admin), expiry, revocation, last_used_at so stale tokens are visible." Each
-- person's Godot plugin uses its own token so one can be revoked without
-- disrupting the other.

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- A human label so a token is identifiable in the list ("laptop", "desktop").
    name         TEXT    NOT NULL,
    -- SHA-256 of the 32-byte random token. The plaintext is shown once and never
    -- stored, exactly like the session tokens.
    token_hash   BLOB    NOT NULL UNIQUE,
    -- Comma-separated subset of read,write,admin. read is the floor.
    scopes       TEXT    NOT NULL DEFAULT 'read',
    -- NULL until the token is first used, then updated so a stale token is visible.
    last_used_at INTEGER,
    -- NULL means no expiry.
    expires_at   INTEGER,
    -- NULL until revoked; set (not deleted) so the audit trail survives.
    revoked_at   INTEGER,
    created_at   INTEGER NOT NULL
) STRICT;

CREATE INDEX api_tokens_user_idx ON api_tokens(user_id, created_at DESC);
