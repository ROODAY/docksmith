-- Notification routing is global: every enabled service receives the same
-- events with the same timing, so per-service delivery and event lists go away.
ALTER TABLE notify_services DROP COLUMN delivery;
ALTER TABLE notify_services DROP COLUMN events;
