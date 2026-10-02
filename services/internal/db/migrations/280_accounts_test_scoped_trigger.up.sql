-- 280_accounts_test_scoped_trigger.up.sql
-- Durable guard for migration 278: harness-created accounts must be
-- test_scoped from the moment they exist, not just after a one-time
-- backfill. Test suites create accounts under dozens of distinct call
-- sites; stamping every INSERT is unmaintainable, so the classifier
-- lives in the database — a BEFORE INSERT trigger marks the account
-- when the owning user's email matches the harness patterns.
--
-- Cost: one users PK lookup per accounts INSERT — negligible against
-- account-creation volume.

BEGIN;

CREATE OR REPLACE FUNCTION trg_accounts_mark_test_scoped()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.test_scoped THEN
        RETURN NEW;
    END IF;
    IF EXISTS (
        SELECT 1 FROM users u
         WHERE u.id = NEW.user_id
           AND (u.email LIKE 'acct\_it\_%'
             OR u.email LIKE 'nbp-itest-%'
             OR u.email LIKE 'chaos\_%'
             OR u.email LIKE 't737-%'
             OR u.email LIKE 'gdpr\_it\_%'
             OR u.email LIKE 'itest-%@exc.local'
             OR u.email LIKE '%@example.test'
             OR u.email LIKE '%@x.test')
    ) THEN
        NEW.test_scoped := TRUE;
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER accounts_mark_test_scoped
    BEFORE INSERT ON accounts
    FOR EACH ROW EXECUTE FUNCTION trg_accounts_mark_test_scoped();

COMMIT;
