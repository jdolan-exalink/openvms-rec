-- 3-stage Avigilon-style retention aging policy (full -> half FPS -> events only).
CREATE TABLE IF NOT EXISTS retention_policy (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    full_days         INTEGER NOT NULL DEFAULT 7,
    half_fps_days     INTEGER NOT NULL DEFAULT 14,
    events_only_days  INTEGER NOT NULL DEFAULT 9,
    updated_at        INTEGER NOT NULL
);
