-- 000003_server_api_key.down.sql
ALTER TABLE apps
    DROP COLUMN IF EXISTS server_key_created_at,
    DROP COLUMN IF EXISTS server_api_secret_enc,
    DROP COLUMN IF EXISTS server_api_key;
