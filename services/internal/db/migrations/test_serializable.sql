-- test_serializable.sql — SERIALIZABLE isolation proof for balances (spec §5.3, §5.40)
--
-- Demonstrates that two concurrent SERIALIZABLE balance mutations on the same
-- row cannot both commit: the second transaction receives SQLSTATE 40001
-- (serialization_failure), and a retry — per the §5.40 retry handler —
-- produces the correct final balance.
--
-- Run inside the postgres container (psql must be on PATH, socket auth trust):
--   docker exec -i exc-dev-postgres-1 psql -v ON_ERROR_STOP=0 -U exchange -d exchange -f /dev/stdin < test_serializable.sql
--
-- Scenario (locking protocol per spec §5.3: SERIALIZABLE + SELECT ... FOR UPDATE):
--   Session A: locks row, holds it ~3s, applies -10, commits.
--   Session B (spawned via \! ~0.5s later): blocks on FOR UPDATE; when A
--              commits, PostgreSQL SSI aborts B with 40001.
--   Retry:     B's work (-20) is re-run in a fresh SERIALIZABLE txn → commits.
--   Expected final available = 100 - 10 - 20 = 70, version = 2.

\echo '========== PART 0: setup synthetic balance row =========='
-- balances has no FK on account_id (spec §5.3 annotates none); synthetic test row.
INSERT INTO balances (account_id, currency, available, locked)
VALUES (999999001, 'USD', 100, 0)
ON CONFLICT (account_id, currency)
DO UPDATE SET available = 100, locked = 0, version = 0;
SELECT account_id, currency, available, locked, total, version
FROM balances WHERE account_id = 999999001 AND currency = 'USD';

\echo '========== PART 1: launch concurrent Session B (background) =========='
\! rm -f /tmp/ser_b.log
\! (sleep 0.5; psql -U exchange -d exchange -v ON_ERROR_STOP=1 -c "BEGIN ISOLATION LEVEL SERIALIZABLE; SELECT available FROM balances WHERE account_id = 999999001 AND currency = 'USD' FOR UPDATE; UPDATE balances SET available = available - 20, version = version + 1 WHERE account_id = 999999001 AND currency = 'USD'; COMMIT;") > /tmp/ser_b.log 2>&1 &

\echo '========== PART 2: Session A — SERIALIZABLE + FOR UPDATE, hold ~3s =========='
BEGIN ISOLATION LEVEL SERIALIZABLE;
SELECT available FROM balances WHERE account_id = 999999001 AND currency = 'USD' FOR UPDATE;
SELECT pg_sleep(3);
UPDATE balances SET available = available - 10, version = version + 1
WHERE account_id = 999999001 AND currency = 'USD';
COMMIT;

\echo '========== PART 3: Session B output — expect SQLSTATE 40001 =========='
\! sleep 2; cat /tmp/ser_b.log

\echo '========== PART 4: retry of failed mutation (§5.40 retry handler) =========='
BEGIN ISOLATION LEVEL SERIALIZABLE;
SELECT available FROM balances WHERE account_id = 999999001 AND currency = 'USD' FOR UPDATE;
UPDATE balances SET available = available - 20, version = version + 1
WHERE account_id = 999999001 AND currency = 'USD';
COMMIT;

\echo '========== PART 5: final state — expect available=70, version=2 =========='
SELECT account_id, currency, available, locked, total, version
FROM balances WHERE account_id = 999999001 AND currency = 'USD';

\echo '========== PART 6: cleanup test row =========='
DELETE FROM balances WHERE account_id = 999999001 AND currency = 'USD';
