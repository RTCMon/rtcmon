-- 000004_connection_stats_source.down.sql
ALTER TABLE connection_stats DROP COLUMN IF EXISTS source;
