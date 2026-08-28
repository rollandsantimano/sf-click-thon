-- Brings an already-created reading_events table up to the denormalised shape
-- in 001.
--
-- 001 is CREATE TABLE IF NOT EXISTS, so it is a no-op against a table that
-- already exists — a deployment created before the name columns were added
-- would never gain them. This ALTER covers that case, and ADD COLUMN IF NOT
-- EXISTS makes it a no-op on a fresh install where 001 already did the work.
--
-- Rows written before this ran keep empty names. Since demo data is recreated
-- by mini-demo.sh on every run, backfilling them is not worth the machinery.

ALTER TABLE reading_events
    ADD COLUMN IF NOT EXISTS student_name LowCardinality(String) AFTER student_id,
    ADD COLUMN IF NOT EXISTS class_name   LowCardinality(String) AFTER class_id,
    ADD COLUMN IF NOT EXISTS book_title   LowCardinality(String) AFTER book_id
