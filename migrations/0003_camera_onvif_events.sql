-- ONVIF device service and event collection for cameras created through the
-- API. Mirrors config.Camera.ONVIFURL / Events.
ALTER TABLE cameras ADD COLUMN onvif_url TEXT NOT NULL DEFAULT '';
ALTER TABLE cameras ADD COLUMN events TEXT NOT NULL DEFAULT '';
