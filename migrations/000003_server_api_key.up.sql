-- 000003_server_api_key.up.sql
-- Adds server SDK API key columns to apps. All three columns are nullable so
-- existing rows are unaffected until a key is explicitly generated for each app.

ALTER TABLE apps
    ADD COLUMN server_api_key         text UNIQUE,
    ADD COLUMN server_api_secret_enc  text,
    ADD COLUMN server_key_created_at  timestamptz;
