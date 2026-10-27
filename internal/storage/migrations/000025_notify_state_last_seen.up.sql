-- last_seen: when the container last appeared in a check result; state is
-- pruned only after a long absence. blocked_version: the target that
-- update_blocked was sent for, so a new blocked target re-notifies.
ALTER TABLE notify_container_state ADD COLUMN last_seen TIMESTAMP;
ALTER TABLE notify_container_state ADD COLUMN blocked_version TEXT NOT NULL DEFAULT '';
