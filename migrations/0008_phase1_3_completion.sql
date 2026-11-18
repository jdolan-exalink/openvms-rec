-- Phase 1-3 completion: per-volume camera assignments & folder_quota_bytes,
-- runtime volume overrides, per-camera recording state, and session revocation.

ALTER TABLE volumes ADD COLUMN cameras_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE volumes ADD COLUMN folder_quota_bytes INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS volume_runtime (
    volume_id    TEXT PRIMARY KEY,
    draining     INTEGER,
    cameras_json TEXT,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS camera_recording_state (
    camera_id          TEXT PRIMARY KEY,
    recording_enabled  INTEGER NOT NULL DEFAULT 1,
    updated_at         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS revoked_tokens (
    token_hash TEXT PRIMARY KEY,
    username   TEXT NOT NULL DEFAULT '',
    revoked_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS revoked_tokens_exp_idx ON revoked_tokens (expires_at);
