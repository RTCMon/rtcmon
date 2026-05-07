-- 000001_initial_schema.down.sql
-- Drops all tables in reverse FK-dependency order.
-- CASCADE handles child partition tables and dependent indexes automatically.

DROP TABLE IF EXISTS share_tokens         CASCADE;
DROP TABLE IF EXISTS session_quality      CASCADE;
DROP TABLE IF EXISTS events               CASCADE;
DROP TABLE IF EXISTS connection_stats     CASCADE;
DROP TABLE IF EXISTS connections          CASCADE;
DROP TABLE IF EXISTS sessions             CASCADE;
DROP TABLE IF EXISTS participants         CASCADE;
DROP TABLE IF EXISTS conferences          CASCADE;
DROP TABLE IF EXISTS apps                 CASCADE;
DROP TABLE IF EXISTS invitations          CASCADE;
DROP TABLE IF EXISTS organization_members CASCADE;
DROP TABLE IF EXISTS organizations        CASCADE;
DROP TABLE IF EXISTS users                CASCADE;
