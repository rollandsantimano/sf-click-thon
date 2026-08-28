-- ReadQuest relational schema.
--
-- Every statement is idempotent so migrations can re-run on every startup.
-- That replaces a schema_migrations tracking table, which would be more
-- machinery than a one-day build justifies, and it makes repeated restarts
-- during a demo safe.

CREATE TABLE IF NOT EXISTS classes (
    id   SERIAL PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS teachers (
    id       SERIAL PRIMARY KEY,
    name     TEXT NOT NULL UNIQUE,
    class_id INT REFERENCES classes(id)
);

CREATE TABLE IF NOT EXISTS students (
    id                SERIAL PRIMARY KEY,
    name              TEXT NOT NULL,
    class_id          INT REFERENCES classes(id),
    xp                INT NOT NULL DEFAULT 0,

    -- streak_days is authoritative: it is maintained transactionally at log
    -- time and ClickHouse is never consulted for it. last_session_date is
    -- what lets that update happen without an analytical read.
    streak_days       INT NOT NULL DEFAULT 0,
    last_session_date DATE,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (class_id, name)
);

-- Case-insensitive lookup supports resolveStudent(), which takes a name
-- because chat users never know their integer id.
CREATE INDEX IF NOT EXISTS idx_students_name ON students (lower(name));

CREATE TABLE IF NOT EXISTS books (
    id          SERIAL PRIMARY KEY,
    title       TEXT NOT NULL,
    author      TEXT NOT NULL DEFAULT 'Unknown',

    -- 'Unknown' marks a row auto-created by resolveBook() on a catalogue
    -- miss. The Genre Explorer badge excludes it so auto-creation cannot
    -- inflate the distinct-genre count.
    genre       TEXT NOT NULL DEFAULT 'Unknown',

    age_min     INT,
    age_max     INT,
    description TEXT
);

-- UNIQUE, not merely indexed: resolveBook() inserts on a miss, and this is
-- what stops a race or a retry from creating a duplicate title.
CREATE UNIQUE INDEX IF NOT EXISTS idx_books_title ON books (lower(title));

CREATE TABLE IF NOT EXISTS reading_sessions (
    id            SERIAL PRIMARY KEY,
    student_id    INT REFERENCES students(id),
    book_id       INT REFERENCES books(id),
    pages_read    INT NOT NULL,
    minutes_spent INT NOT NULL,
    session_date  DATE NOT NULL DEFAULT CURRENT_DATE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_sessions_student ON reading_sessions (student_id, session_date DESC);

CREATE TABLE IF NOT EXISTS badges (
    id              SERIAL PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL,
    condition_type  TEXT NOT NULL,  -- first_session | pages_total | streak | genres
    condition_value INT NOT NULL
);

CREATE TABLE IF NOT EXISTS student_badges (
    id         SERIAL PRIMARY KEY,
    student_id INT REFERENCES students(id),
    badge_id   INT REFERENCES badges(id),
    earned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (student_id, badge_id)
);
