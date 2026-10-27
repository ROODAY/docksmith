ALTER TABLE notify_services ADD COLUMN delivery TEXT NOT NULL DEFAULT 'immediate';
ALTER TABLE notify_services ADD COLUMN events TEXT NOT NULL DEFAULT '[]';
