-- Berth initial schema. Timestamps are UTC text in "YYYY-MM-DD HH:MM:SS" form
-- (fixed width, so SQL string comparison orders them correctly).

CREATE TABLE users (
    id            INTEGER  PRIMARY KEY,
    username      TEXT     NOT NULL UNIQUE,
    password_hash TEXT     NOT NULL,
    is_admin      INTEGER  NOT NULL DEFAULT 0 CHECK (is_admin IN (0, 1)),
    created_at    DATETIME NOT NULL
);

-- Server-side sessions. Only sha256(token) is stored, never the token itself.
CREATE TABLE sessions (
    token_hash TEXT     PRIMARY KEY,
    user_id    INTEGER  NOT NULL REFERENCES users(id),
    created_at DATETIME NOT NULL,
    expires_at DATETIME NOT NULL
);
CREATE INDEX sessions_user_id    ON sessions(user_id);
CREATE INDEX sessions_expires_at ON sessions(expires_at);

-- One workspace per user. container_id NULL means the container was removed
-- and only the data volume remains.
CREATE TABLE workspaces (
    user_id         INTEGER  PRIMARY KEY REFERENCES users(id),
    container_id    TEXT,
    state           TEXT     NOT NULL DEFAULT 'offline'
                             CHECK (state IN ('offline', 'starting', 'running', 'stopped')),
    volume_name     TEXT     NOT NULL UNIQUE,
    mem_limit_mb    INTEGER  NOT NULL DEFAULT 2048 CHECK (mem_limit_mb > 0),
    cpu_limit       REAL     NOT NULL DEFAULT 2.0   CHECK (cpu_limit > 0),
    last_active_at  DATETIME,
    last_stopped_at DATETIME,
    updated_at      DATETIME NOT NULL
);
