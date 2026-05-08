-- 000002_add_external_id.up.sql
-- Adds an external_id column to sessions and connections so that SDK-supplied
-- string identifiers can be mapped to DB bigint PKs across batches.

ALTER TABLE sessions    ADD COLUMN external_id text UNIQUE;
ALTER TABLE connections ADD COLUMN external_id text UNIQUE;
