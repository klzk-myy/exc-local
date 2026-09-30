-- 000_database.sql — Phase-20 Task 20.3.1 (spec §16)
-- Analytics database. Apply order: files in lexical order, all idempotent
-- (IF NOT EXISTS everywhere) so re-application is safe.
CREATE DATABASE IF NOT EXISTS exchange_analytics;
