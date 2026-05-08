-- 000004_connection_stats_source.up.sql
-- Adds a source column to connection_stats to record which SDK sent each row.
-- DEFAULT 'browser' retroactively labels all pre-BE-018 rows as browser-originated.
-- NOT NULL because every stat row must be attributable to a source.
-- On Postgres 16, ADD COLUMN with a non-volatile DEFAULT is an O(1) metadata operation.

ALTER TABLE connection_stats ADD COLUMN source text NOT NULL DEFAULT 'browser';
