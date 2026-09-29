ALTER TABLE ops_status_events
    DROP CONSTRAINT ops_status_events_to_state_check;
ALTER TABLE ops_status_events
    ADD CONSTRAINT ops_status_events_to_state_check CHECK (to_state IN
        ('operational', 'degraded_performance', 'partial_outage',
         'major_outage', 'maintenance', 'unknown'));
