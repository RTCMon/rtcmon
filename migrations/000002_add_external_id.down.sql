-- 000002_add_external_id.down.sql

ALTER TABLE connections DROP COLUMN IF EXISTS external_id;
ALTER TABLE sessions    DROP COLUMN IF EXISTS external_id;
