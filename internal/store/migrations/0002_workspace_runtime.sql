-- Milestone 2: workspace runtime data and workspace-scoped session tokens.
-- SQLite cannot ADD COLUMN ... UNIQUE (or NOT NULL without a default), so every
-- column is nullable and uniqueness comes from partial unique indexes.

-- A second, independent token per login, sent only on /ws/ paths (cookie
-- berth_ws) and accepted only by the ForwardAuth check. The API token
-- (berth_session, /api only) is therefore never forwarded to a workspace
-- container, where code running as the user could read it.
ALTER TABLE sessions ADD COLUMN ws_token_hash TEXT;
CREATE UNIQUE INDEX sessions_ws_token_hash ON sessions(ws_token_hash)
    WHERE ws_token_hash IS NOT NULL;

-- Per-container UDP port range for neko's WebRTC media (inclusive), assigned
-- when the container is created and released when it is removed.
ALTER TABLE workspaces ADD COLUMN udp_port_start INTEGER;
ALTER TABLE workspaces ADD COLUMN udp_port_end   INTEGER;
CREATE UNIQUE INDEX workspaces_udp_port_start ON workspaces(udp_port_start)
    WHERE udp_port_start IS NOT NULL;

-- Password the neko web UI is configured with. It is generated once per
-- workspace and handed only to the owner, so the user is not asked to log in a
-- second time. (It is also visible in the container's environment.)
ALTER TABLE workspaces ADD COLUMN neko_password TEXT;
