-- 211_ops_status_down: per-component transitions legitimately record the
-- component-state word 'down' (status_exporter Collect → recordEvent), but
-- the 197 CHECK only admitted the aggregate statuspage vocabulary. Every
-- component outage event therefore failed the constraint and was dropped —
-- a silent audit gap in the ops status ledger. Widen the CHECK to admit the
-- per-component vocabulary alongside the aggregate words.
ALTER TABLE ops_status_events
    DROP CONSTRAINT ops_status_events_to_state_check;
ALTER TABLE ops_status_events
    ADD CONSTRAINT ops_status_events_to_state_check CHECK (to_state IN
        ('operational', 'degraded_performance', 'partial_outage',
         'major_outage', 'maintenance', 'unknown', 'down'));
