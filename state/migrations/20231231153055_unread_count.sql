-- +goose Up
ALTER TABLE IF EXISTS syncv3_unread
    ADD COLUMN IF NOT EXISTS unread_count BIGINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE IF EXISTS syncv3_events
    DROP COLUMN IF EXISTS unread_count;
