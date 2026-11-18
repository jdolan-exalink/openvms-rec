-- Camera brand/listener hint and local NVR motion analytics toggle.
ALTER TABLE cameras ADD COLUMN brand TEXT NOT NULL DEFAULT '';
ALTER TABLE cameras ADD COLUMN nvr_motion INTEGER NOT NULL DEFAULT 0;
