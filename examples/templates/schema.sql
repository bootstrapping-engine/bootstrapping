-- The email templates a self-hosted product ships. Each customer's admin
-- may reword them, turn some off, and brand them.
CREATE TABLE templates (
    id      INTEGER PRIMARY KEY,
    code    TEXT NOT NULL UNIQUE,
    subject TEXT NOT NULL,
    body    TEXT NOT NULL,
    footer  TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1
);
