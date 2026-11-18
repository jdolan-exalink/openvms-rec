-- ONVIF port and PullPoint event polling flag for API-managed cameras.
ALTER TABLE cameras ADD COLUMN onvif_port INTEGER NOT NULL DEFAULT 80;
ALTER TABLE cameras ADD COLUMN onvif_events INTEGER NOT NULL DEFAULT 1;
