-- A SaaS product's plan tiers and the features each tier includes. The
-- product ships them as reference data; subscriptions are the customers'.
CREATE TABLE plans (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    price_cents INTEGER,
    seats       INTEGER,
    retired_at  TEXT
);

CREATE TABLE features (
    id          INTEGER PRIMARY KEY,
    code        TEXT NOT NULL UNIQUE,
    description TEXT
);

CREATE TABLE plan_features (
    plan_id    INTEGER NOT NULL REFERENCES plans (id),
    feature_id INTEGER NOT NULL REFERENCES features (id),
    PRIMARY KEY (plan_id, feature_id)
);

CREATE TABLE subscriptions (
    id       INTEGER PRIMARY KEY,
    customer TEXT NOT NULL,
    plan_id  INTEGER NOT NULL REFERENCES plans (id)
);
