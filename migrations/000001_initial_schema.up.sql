-- 000001_initial_schema.up.sql
-- Creates the full initial schema in FK-dependency order.

-- ─────────────────────────────────────────────────────────────────────────────
-- Users
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
    id            bigserial PRIMARY KEY,
    email         text UNIQUE NOT NULL,
    name          text NOT NULL,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Organizations
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE organizations (
    id         bigserial PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Team membership
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE organization_members (
    org_id     bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    bigint NOT NULL REFERENCES users(id)         ON DELETE CASCADE,
    role       text   NOT NULL DEFAULT 'member',   -- 'admin' | 'member'
    invited_by bigint          REFERENCES users(id),
    joined_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE invitations (
    token       text PRIMARY KEY,
    org_id      bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       text   NOT NULL,
    role        text   NOT NULL DEFAULT 'member',
    invited_by  bigint REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz
);

CREATE INDEX idx_invitations_org_id ON invitations (org_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Apps
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE apps (
    id                 bigserial PRIMARY KEY,
    org_id             bigint  NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name               text    NOT NULL,
    api_key_hash       text    NOT NULL,
    retention_days     int     NOT NULL DEFAULT 90,
    observation_config jsonb,
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_apps_org_id ON apps (org_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Conferences
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE conferences (
    id                bigserial PRIMARY KEY,
    app_id            bigint NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    external_id       text   NOT NULL,
    started_at        timestamptz NOT NULL DEFAULT now(),
    ended_at          timestamptz,
    participant_count int    NOT NULL DEFAULT 0,
    UNIQUE (app_id, external_id)
);

CREATE INDEX idx_conferences_app_started ON conferences (app_id, started_at);

-- ─────────────────────────────────────────────────────────────────────────────
-- Participants
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE participants (
    id            bigserial PRIMARY KEY,
    conference_id bigint NOT NULL REFERENCES conferences(id) ON DELETE CASCADE,
    user_id       text   NOT NULL,
    display_name  text   NOT NULL DEFAULT '',
    joined_at     timestamptz NOT NULL DEFAULT now(),
    left_at       timestamptz,
    UNIQUE (conference_id, user_id)
);

CREATE INDEX idx_participants_conference_user ON participants (conference_id, user_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Sessions
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE sessions (
    id             bigserial PRIMARY KEY,
    participant_id bigint NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    browser        text NOT NULL DEFAULT '',
    os             text NOT NULL DEFAULT '',
    country        text NOT NULL DEFAULT '',
    city           text NOT NULL DEFAULT '',
    network_type   text NOT NULL DEFAULT '',
    sdk_version    text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_browser  ON sessions (browser);
CREATE INDEX idx_sessions_os       ON sessions (os);
CREATE INDEX idx_sessions_country  ON sessions (country);

-- ─────────────────────────────────────────────────────────────────────────────
-- Connections
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE connections (
    id          bigserial PRIMARY KEY,
    session_id  bigint NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    peer_id     text   NOT NULL DEFAULT '',
    started_at  timestamptz NOT NULL DEFAULT now(),
    ended_at    timestamptz,
    ice_state   text NOT NULL DEFAULT '',
    dtls_state  text NOT NULL DEFAULT '',
    codec_audio text NOT NULL DEFAULT '',
    codec_video text NOT NULL DEFAULT ''
);

CREATE INDEX idx_connections_session_id ON connections (session_id);

-- ─────────────────────────────────────────────────────────────────────────────
-- Connection stats (partitioned by month)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE connection_stats (
    id                     bigserial,
    connection_id          bigint      NOT NULL REFERENCES connections(id),
    ts                     timestamptz NOT NULL,
    -- network (raw)
    packets_lost_rate      float4,
    jitter_ms              float4,
    rtt_ms                 float4,
    bitrate_in_kbps        int4,
    bitrate_out_kbps       int4,
    -- network (EWMA-smoothed, α=0.2, computed at ingest)
    ewma_packets_lost_rate float4,
    ewma_jitter_ms         float4,
    ewma_rtt_ms            float4,
    ewma_bitrate_in_kbps   float4,
    ewma_bitrate_out_kbps  float4,
    -- media
    fps                    int4,
    frame_width            int4,
    frame_height           int4,
    audio_level            float4,
    concealment_ratio      float4,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);

-- Create monthly partitions: current month through the next 11 months (12 total).
-- New months beyond this window will need additional migrations or a background job.
DO $$
DECLARE
    start_date     date;
    end_date       date;
    partition_name text;
BEGIN
    FOR i IN 0..11 LOOP
        start_date     := date_trunc('month', now()) + (i || ' months')::interval;
        end_date       := start_date + '1 month'::interval;
        partition_name := 'connection_stats_' || to_char(start_date, 'YYYY_MM');

        IF NOT EXISTS (
            SELECT 1 FROM pg_class WHERE relname = partition_name
        ) THEN
            EXECUTE format(
                'CREATE TABLE %I PARTITION OF connection_stats FOR VALUES FROM (%L) TO (%L)',
                partition_name, start_date, end_date
            );
        END IF;
    END LOOP;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────────
-- Events
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE events (
    id            bigserial PRIMARY KEY,
    connection_id bigint REFERENCES connections(id) ON DELETE CASCADE,
    session_id    bigint REFERENCES sessions(id)    ON DELETE CASCADE,
    ts            timestamptz NOT NULL,
    event_type    text        NOT NULL,
    payload       jsonb
);

CREATE INDEX idx_events_connection_ts ON events (connection_id, ts);
CREATE INDEX idx_events_session_ts    ON events (session_id,    ts);

-- ─────────────────────────────────────────────────────────────────────────────
-- Session quality (written post-call by eMOS job)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE session_quality (
    session_id  bigint PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    emos        float4      NOT NULL,
    outcome     text        NOT NULL,   -- 'success' | 'degraded' | 'failed'
    computed_at timestamptz NOT NULL DEFAULT now()
);

-- ─────────────────────────────────────────────────────────────────────────────
-- Share tokens (permanent team-scoped links, no expiry)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE share_tokens (
    token         text PRIMARY KEY,
    conference_id bigint NOT NULL REFERENCES conferences(id)   ON DELETE CASCADE,
    org_id        bigint NOT NULL REFERENCES organizations(id)  ON DELETE CASCADE,
    created_by    bigint          REFERENCES users(id)          ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    view_count    int         NOT NULL DEFAULT 0
);

CREATE INDEX idx_share_tokens_conference ON share_tokens (conference_id);
CREATE INDEX idx_share_tokens_org        ON share_tokens (org_id);
