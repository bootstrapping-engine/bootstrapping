-- Countries and their currencies, as an application ships them. Customer
-- addresses point at countries; they're the application's own data.
CREATE TABLE currencies (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    minor_units INTEGER NOT NULL
);

CREATE TABLE countries (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    currency_id INTEGER REFERENCES currencies (id)
);

CREATE TABLE addresses (
    id         INTEGER PRIMARY KEY,
    line       TEXT NOT NULL,
    country_id INTEGER NOT NULL REFERENCES countries (id)
);
