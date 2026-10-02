-- PostgreSQL cannot drop a single enum value; DOWN re-creates the
-- type without PARKED and rewrites any parked rows to FORCE_CASH so
-- the column cast succeeds.
UPDATE liquidation_auctions SET phase = 'FORCE_CASH' WHERE phase = 'PARKED';

ALTER TABLE liquidation_auctions ALTER COLUMN phase DROP DEFAULT;

CREATE TYPE auction_phase_enum_old AS ENUM ('CALL', 'FILL', 'EXTEND', 'FORCE_CASH');

ALTER TABLE liquidation_auctions
    ALTER COLUMN phase TYPE auction_phase_enum_old
    USING phase::text::auction_phase_enum_old;

DROP TYPE auction_phase_enum;
ALTER TYPE auction_phase_enum_old RENAME TO auction_phase_enum;

ALTER TABLE liquidation_auctions ALTER COLUMN phase SET DEFAULT 'CALL'::auction_phase_enum;
