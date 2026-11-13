-- OpenVMS Recorder initial schema.
-- Timestamps are INTEGER milliseconds since Unix epoch, UTC.
-- Video never lives in SQLite: segments reference files on volumes.

CREATE TABLE cameras (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    enabled             INTEGER NOT NULL DEFAULT 1,
    driver              TEXT NOT NULL DEFAULT 'onvif_generic',
    host                TEXT NOT NULL DEFAULT '',
    -- Stream URLs are stored WITHOUT credentials; credentials_ref points into
    -- the secrets file / secret manager.
    stream_main_url     TEXT NOT NULL DEFAULT '',
    stream_sub_url      TEXT NOT NULL DEFAULT '',
    credentials_ref     TEXT NOT NULL DEFAULT '',
    rtsp_transport      TEXT NOT NULL DEFAULT 'tcp' CHECK (rtsp_transport IN ('tcp', 'udp')),
    recording_mode      TEXT NOT NULL DEFAULT 'continuous_with_event_markers',
    dual_recording      INTEGER NOT NULL DEFAULT 0,
    recording_enabled   INTEGER NOT NULL DEFAULT 1,
    priority            INTEGER NOT NULL DEFAULT 5,
    retention_main_days INTEGER,          -- NULL = global default
    retention_sub_days  INTEGER,
    retention_min_days  INTEGER,
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL
);

-- Capability matrix per camera, with evidence. status is never inferred
-- from the vendor name (PRD M01).
CREATE TABLE camera_capabilities (
    camera_id   TEXT NOT NULL REFERENCES cameras(id) ON DELETE CASCADE,
    capability  TEXT NOT NULL,           -- e.g. onvif.profile_g.replay
    status      TEXT NOT NULL CHECK (status IN ('confirmed', 'unsupported', 'unknown', 'error')),
    evidence    TEXT NOT NULL DEFAULT '', -- redacted JSON of the operation performed
    firmware    TEXT NOT NULL DEFAULT '',
    checked_at  INTEGER NOT NULL,
    PRIMARY KEY (camera_id, capability)
);

CREATE TABLE volumes (
    id              TEXT PRIMARY KEY,
    root_path       TEXT NOT NULL,
    stable_id       TEXT NOT NULL DEFAULT '',
    fs_type         TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1,
    draining        INTEGER NOT NULL DEFAULT 0,
    priority        INTEGER NOT NULL DEFAULT 0,
    weight          INTEGER NOT NULL DEFAULT 1,
    max_percent     REAL,                -- NULL = global default (90)
    quota_bytes     INTEGER,
    min_free_bytes  INTEGER,
    state           TEXT NOT NULL DEFAULT 'uninitialized',
    state_reason    TEXT NOT NULL DEFAULT '',
    is_system       INTEGER NOT NULL DEFAULT 0,
    is_network      INTEGER NOT NULL DEFAULT 0,
    is_removable    INTEGER NOT NULL DEFAULT 0,
    capacity_bytes  INTEGER NOT NULL DEFAULT 0,
    free_bytes      INTEGER NOT NULL DEFAULT 0,
    last_seen_at    INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
-- Two configured paths on the same filesystem would double count capacity.
CREATE UNIQUE INDEX volumes_stable_id ON volumes(stable_id) WHERE stable_id <> '';

CREATE TABLE camera_volume_assignments (
    camera_id  TEXT NOT NULL REFERENCES cameras(id) ON DELETE CASCADE,
    volume_id  TEXT NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
    position   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (camera_id, volume_id)
);

-- Sticky placement (camera_affinity) persisted across restarts.
CREATE TABLE placement_affinity (
    camera_id  TEXT PRIMARY KEY,
    volume_id  TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

-- ADR-001 segment model. Rows outlive missing volumes: metadata of a
-- disconnected disk is kept and marked unavailable, never dropped.
CREATE TABLE segments (
    id                INTEGER PRIMARY KEY,
    camera_id         TEXT NOT NULL,
    representation_id TEXT NOT NULL DEFAULT 'main',
    start_utc         INTEGER NOT NULL,
    end_utc           INTEGER,            -- NULL while writing
    codec             TEXT NOT NULL DEFAULT '',
    keyframe_info     TEXT NOT NULL DEFAULT '', -- JSON
    volume_id         TEXT NOT NULL,
    relative_path     TEXT NOT NULL,      -- forward slashes, relative to volume root
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    checksum          TEXT NOT NULL DEFAULT '', -- sha256 hex
    status            TEXT NOT NULL DEFAULT 'writing'
                      CHECK (status IN ('writing', 'final', 'corrupt', 'deleting', 'deleted')),
    protection_until  INTEGER,            -- NULL = not protected
    source            TEXT NOT NULL DEFAULT 'live_ingest'
                      CHECK (source IN ('live_ingest', 'recovered_sd', 'remote_only')),
    generation        INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    UNIQUE (volume_id, relative_path)
);
CREATE INDEX segments_timeline ON segments(camera_id, representation_id, start_utc);
CREATE INDEX segments_volume_age ON segments(volume_id, status, start_utc);

-- Holds block deletion while an export or migration reads a segment.
CREATE TABLE segment_holds (
    segment_id INTEGER NOT NULL REFERENCES segments(id) ON DELETE CASCADE,
    holder     TEXT NOT NULL,             -- e.g. export:<id>
    expires_at INTEGER NOT NULL,
    PRIMARY KEY (segment_id, holder)
);

-- Explicit gaps: never invent continuity (ADR-003).
CREATE TABLE recording_gaps (
    id                INTEGER PRIMARY KEY,
    camera_id         TEXT NOT NULL,
    representation_id TEXT NOT NULL DEFAULT 'main',
    start_utc         INTEGER NOT NULL,
    end_utc           INTEGER,
    reason            TEXT NOT NULL      -- stream_lost, no_volume, volume_failed, ...
);
CREATE INDEX recording_gaps_timeline ON recording_gaps(camera_id, start_utc);

CREATE TABLE events (
    id             TEXT PRIMARY KEY,
    camera_id      TEXT NOT NULL,
    timestamp_utc  INTEGER NOT NULL,
    type           TEXT NOT NULL,
    class          TEXT NOT NULL DEFAULT '',
    confidence     REAL,
    bbox           TEXT,                 -- JSON [x,y,w,h] normalized
    zone           TEXT,
    snapshot_ref   TEXT,
    source         TEXT NOT NULL,        -- onvif_events, provision_isr, ...
    raw_ref        TEXT,
    dedupe_key     TEXT NOT NULL UNIQUE,
    protected      INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);
CREATE INDEX events_timeline ON events(camera_id, timestamp_utc);

CREATE TABLE jobs (
    id              TEXT PRIMARY KEY,
    kind            TEXT NOT NULL,       -- export, migrate_segments, sd_recovery, reindex
    state           TEXT NOT NULL CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    payload         TEXT NOT NULL DEFAULT '{}',
    progress        REAL NOT NULL DEFAULT 0,
    error           TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT UNIQUE,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE storage_policies (
    id                     TEXT PRIMARY KEY,
    default_max_percent    REAL NOT NULL DEFAULT 90,
    cleanup_start_percent  REAL NOT NULL DEFAULT 87,
    cleanup_target_percent REAL NOT NULL DEFAULT 84,
    min_free_bytes         INTEGER NOT NULL DEFAULT 100000000000,
    placement              TEXT NOT NULL DEFAULT 'balanced'
                           CHECK (placement IN ('balanced', 'camera_affinity', 'manual')),
    pressure_policy        TEXT NOT NULL DEFAULT 'overwrite_oldest_unprotected',
    updated_at             INTEGER NOT NULL
);

CREATE TABLE storage_alerts (
    id          INTEGER PRIMARY KEY,
    volume_id   TEXT,
    camera_id   TEXT,
    code        TEXT NOT NULL,
    message     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    resolved_at INTEGER
);

CREATE TABLE audit_log (
    id      INTEGER PRIMARY KEY,
    at      INTEGER NOT NULL,
    actor   TEXT NOT NULL,
    action  TEXT NOT NULL,
    target  TEXT NOT NULL DEFAULT '',
    details TEXT NOT NULL DEFAULT '{}'
);
