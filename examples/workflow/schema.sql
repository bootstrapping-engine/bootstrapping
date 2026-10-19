-- A helpdesk's ticket states. Each state names the one that usually comes
-- next. Tickets are the application's own data and point at states by id.
CREATE TABLE states (
    id      INTEGER PRIMARY KEY,
    code    TEXT NOT NULL UNIQUE,
    label   TEXT NOT NULL,
    next_id INTEGER REFERENCES states (id),
    closed  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE tickets (
    id       INTEGER PRIMARY KEY,
    title    TEXT NOT NULL,
    state_id INTEGER NOT NULL REFERENCES states (id)
);
