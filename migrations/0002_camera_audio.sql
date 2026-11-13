-- Cameras created through the API (cameras in the YAML file are not stored
-- here). audio mirrors config.Camera.Audio.
ALTER TABLE cameras ADD COLUMN audio TEXT NOT NULL DEFAULT 'auto';
