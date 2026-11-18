-- Add multi-object detections JSON array column to events so a single frame/event
-- can record all classified targets (e.g. person + vehicle) with individual bboxes.
ALTER TABLE events ADD COLUMN detections TEXT NOT NULL DEFAULT '';
