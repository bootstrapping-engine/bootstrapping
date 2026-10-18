-- The demo application's tables. Bootstrapping fills the reference data;
-- creating the tables is the application's own migrations' job.
CREATE TABLE roles (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    description TEXT,
    system      INTEGER NOT NULL DEFAULT 0,
    retired_at  TEXT
);

CREATE TABLE permissions (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    description TEXT,
    retired_at  TEXT
);

CREATE TABLE role_permissions (
    role_id       INTEGER NOT NULL REFERENCES roles (id),
    permission_id INTEGER NOT NULL REFERENCES permissions (id),
    PRIMARY KEY (role_id, permission_id)
);
