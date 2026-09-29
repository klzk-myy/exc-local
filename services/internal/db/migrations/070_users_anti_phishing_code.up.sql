-- 070_users_anti_phishing_code.up.sql
-- Phase-12 Task 12.3.8: anti-phishing code.
--
-- users.anti_phishing_code was explicitly deferred by migration 002
-- ("owned by later"); this is that column. Length window is 4–32 chars,
-- aligned with the Phase-10 settings UI and spec §5.16 (remediation #35 —
-- supersedes the earlier 20-char note). NULL = unset; the notification
-- service renders a "configure your anti-phishing code" banner when the
-- lookup returns NULL.

BEGIN;

ALTER TABLE users
    ADD COLUMN anti_phishing_code VARCHAR(32);

ALTER TABLE users
    ADD CONSTRAINT users_anti_phishing_code_len
    CHECK (anti_phishing_code IS NULL
           OR char_length(anti_phishing_code) BETWEEN 4 AND 32);

COMMIT;
