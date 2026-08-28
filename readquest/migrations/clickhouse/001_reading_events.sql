-- ReadQuest analytical event stream.
--
-- One row per logged reading session, mirrored from Postgres. This table
-- answers the teacher at-risk dashboard and backs ad-hoc natural-language
-- questions asked through ClickHouse Cloud's native MCP.
--
-- Ordering key rationale: the dashboard filters on
--   class_id = ? AND session_date >= today() - 7
-- then groups by student_id. Leading with (class_id, session_date) lets
-- ClickHouse prune on both filter predicates before grouping. No PARTITION BY
-- clause: at classroom scale, partitioning costs more in metadata than it
-- saves in scanning.

-- Names are denormalised alongside their ids on purpose. ClickHouse cannot
-- join against Postgres, so without them every chart, console query and
-- natural-language answer from the ClickHouse MCP is labelled "student_id 1"
-- instead of "Maya Chen". Wide tables over joins is the idiomatic shape for a
-- columnar store, and LowCardinality keeps the repeated strings close to free.
--
-- This does NOT remove the need for the Postgres roster in the dashboard: a
-- student who has never read produces no events here at all, so they can only
-- be found by starting from the roster.

CREATE TABLE IF NOT EXISTS reading_events (
    event_id      UUID     DEFAULT generateUUIDv4(),
    student_id    Int32,
    student_name  LowCardinality(String),
    class_id      Int32,
    class_name    LowCardinality(String),
    book_id       Int32,
    book_title    LowCardinality(String),
    genre         String,
    pages_read    Int32,
    minutes_spent Int32,
    session_date  Date,
    created_at    DateTime DEFAULT now()
)
ENGINE = MergeTree()
ORDER BY (class_id, session_date, student_id)
