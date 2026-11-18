-- Phase 4: Standalone optional adapter configuration & sync state for OpenVMS Core.

CREATE TABLE IF NOT EXISTS core_adapter_config (
    id                       INTEGER PRIMARY KEY CHECK (id = 1),
    enabled                  INTEGER NOT NULL DEFAULT 0,
    core_url                 TEXT NOT NULL DEFAULT '',
    node_id                  TEXT NOT NULL DEFAULT '',
    site_id                  TEXT NOT NULL DEFAULT '',
    token_enc                TEXT NOT NULL DEFAULT '',
    sync_interval_seconds    INTEGER NOT NULL DEFAULT 30,
    push_events              INTEGER NOT NULL DEFAULT 1,
    push_health              INTEGER NOT NULL DEFAULT 1,
    tls_insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
    updated_at               INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS core_adapter_state (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    last_event_ms  INTEGER NOT NULL DEFAULT 0,
    last_event_id  TEXT NOT NULL DEFAULT '',
    events_synced  INTEGER NOT NULL DEFAULT 0,
    last_sync_utc  INTEGER NOT NULL DEFAULT 0,
    status         TEXT NOT NULL DEFAULT 'disabled',
    last_error     TEXT NOT NULL DEFAULT ''
);
