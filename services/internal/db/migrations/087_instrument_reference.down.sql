-- 087_instrument_reference.down.sql
-- Reverses 087: auction_calendar, instruments_reference, the seeded §2.2
-- symbols, the new reference columns, and restores USD/JPY's
-- migration-001 tick. (The listing_proposals extension moved to 223 —
-- it alters a table 091 owns.)

BEGIN;

DROP TABLE IF EXISTS auction_calendar;
DROP TABLE IF EXISTS instruments_reference;

DELETE FROM instruments
 WHERE symbol IN ('EUR/GBP','EUR/JPY','EUR/CHF','USD/BRL');

UPDATE instruments SET tick_size = 0.00001 WHERE symbol = 'USD/JPY';
UPDATE instruments SET min_notional = 0
 WHERE symbol IN ('EUR/USD','GBP/USD','USD/JPY','AUD/USD',
                  'USD/CAD','USD/CHF','NZD/USD','USD/MXN');

DROP TRIGGER IF EXISTS instruments_reference_defaults_trg ON instruments;
DROP FUNCTION IF EXISTS instruments_reference_defaults();

ALTER TABLE instruments
    DROP CONSTRAINT IF EXISTS instruments_contract_size_positive,
    DROP CONSTRAINT IF EXISTS instruments_decimal_places_range,
    DROP CONSTRAINT IF EXISTS instruments_pip_size_positive,
    DROP COLUMN IF EXISTS pip_size,
    DROP COLUMN IF EXISTS decimal_places,
    DROP COLUMN IF EXISTS contract_size;

COMMIT;
