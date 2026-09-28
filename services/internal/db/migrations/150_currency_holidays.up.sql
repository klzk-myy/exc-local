-- 150_currency_holidays.up.sql
-- Task 3.3.8 Multi-Currency Holiday Calendar Engine (spec §17.5, §24 #123).
--
-- Per-currency banking holidays keyed by (currency, holiday_date). The
-- HolidayCalendarService loads this table at startup/reload and applies the
-- ISDA Modified Following Business Day convention for T+0/T+1/T+2 spot value
-- dates, with split-holiday roll-forward across both currency centers (plus
-- USD for cross pairs).
--
-- Sources (deterministic, publicly published calendars):
--   USD  Federal Reserve Board holiday schedule
--   EUR  TARGET2 closing days (ECB)
--   GBP  Bank of England bank holidays (England & Wales)
--   JPY  Bank of Japan / Japanese national holidays
--   CHF  SIX SIC / SNB settlement holidays
--   AUD  Australian settlement holidays (Sydney)
--   NZD  New Zealand settlement holidays (Wellington)
--   CAD  Bank of Canada settlement holidays
--   MXN  Banco de México banking holidays
-- Weekend-observed substitute dates are recorded as the actual closed date
-- (e.g. US Independence Day 2026-07-04 Sat -> observed 2026-07-03 Fri).
-- Coverage window: 2025-01-01 .. 2027-12-31. Refresh annually by appending
-- rows; the service re-reads the table on LoadCalendar reload.

BEGIN;

CREATE TABLE currency_holidays (
    id           BIGSERIAL PRIMARY KEY,
    currency     VARCHAR(3)   NOT NULL,
    holiday_date DATE         NOT NULL,
    name         VARCHAR(128) NOT NULL,
    source       VARCHAR(64)  NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (currency, holiday_date)
);

CREATE INDEX idx_currency_holidays_date ON currency_holidays (holiday_date);

-- ============================ USD (Federal Reserve) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('USD', '2025-01-01', 'New Year''s Day',              'FEDERAL_RESERVE'),
    ('USD', '2025-01-20', 'Martin Luther King Jr. Day',   'FEDERAL_RESERVE'),
    ('USD', '2025-02-17', 'Washington''s Birthday',       'FEDERAL_RESERVE'),
    ('USD', '2025-05-26', 'Memorial Day',                 'FEDERAL_RESERVE'),
    ('USD', '2025-06-19', 'Juneteenth',                   'FEDERAL_RESERVE'),
    ('USD', '2025-07-04', 'Independence Day',             'FEDERAL_RESERVE'),
    ('USD', '2025-09-01', 'Labor Day',                    'FEDERAL_RESERVE'),
    ('USD', '2025-10-13', 'Columbus Day',                 'FEDERAL_RESERVE'),
    ('USD', '2025-11-11', 'Veterans Day',                 'FEDERAL_RESERVE'),
    ('USD', '2025-11-27', 'Thanksgiving Day',             'FEDERAL_RESERVE'),
    ('USD', '2025-12-25', 'Christmas Day',                'FEDERAL_RESERVE'),
    ('USD', '2026-01-01', 'New Year''s Day',              'FEDERAL_RESERVE'),
    ('USD', '2026-01-19', 'Martin Luther King Jr. Day',   'FEDERAL_RESERVE'),
    ('USD', '2026-02-16', 'Washington''s Birthday',       'FEDERAL_RESERVE'),
    ('USD', '2026-05-25', 'Memorial Day',                 'FEDERAL_RESERVE'),
    ('USD', '2026-06-19', 'Juneteenth',                   'FEDERAL_RESERVE'),
    ('USD', '2026-07-03', 'Independence Day (observed)',  'FEDERAL_RESERVE'),
    ('USD', '2026-09-07', 'Labor Day',                    'FEDERAL_RESERVE'),
    ('USD', '2026-10-12', 'Columbus Day',                 'FEDERAL_RESERVE'),
    ('USD', '2026-11-11', 'Veterans Day',                 'FEDERAL_RESERVE'),
    ('USD', '2026-11-26', 'Thanksgiving Day',             'FEDERAL_RESERVE'),
    ('USD', '2026-12-25', 'Christmas Day',                'FEDERAL_RESERVE'),
    ('USD', '2027-01-01', 'New Year''s Day',              'FEDERAL_RESERVE'),
    ('USD', '2027-01-18', 'Martin Luther King Jr. Day',   'FEDERAL_RESERVE'),
    ('USD', '2027-02-15', 'Washington''s Birthday',       'FEDERAL_RESERVE'),
    ('USD', '2027-05-31', 'Memorial Day',                 'FEDERAL_RESERVE'),
    ('USD', '2027-06-18', 'Juneteenth (observed)',        'FEDERAL_RESERVE'),
    ('USD', '2027-07-05', 'Independence Day (observed)',  'FEDERAL_RESERVE'),
    ('USD', '2027-09-06', 'Labor Day',                    'FEDERAL_RESERVE'),
    ('USD', '2027-10-11', 'Columbus Day',                 'FEDERAL_RESERVE'),
    ('USD', '2027-11-11', 'Veterans Day',                 'FEDERAL_RESERVE'),
    ('USD', '2027-11-25', 'Thanksgiving Day',             'FEDERAL_RESERVE'),
    ('USD', '2027-12-24', 'Christmas Day (observed)',     'FEDERAL_RESERVE');

-- ============================ EUR (TARGET2) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('EUR', '2025-01-01', 'New Year''s Day',   'TARGET2'),
    ('EUR', '2025-04-18', 'Good Friday',       'TARGET2'),
    ('EUR', '2025-04-21', 'Easter Monday',     'TARGET2'),
    ('EUR', '2025-05-01', 'Labour Day',        'TARGET2'),
    ('EUR', '2025-12-25', 'Christmas Day',     'TARGET2'),
    ('EUR', '2025-12-26', 'St Stephen''s Day', 'TARGET2'),
    ('EUR', '2026-01-01', 'New Year''s Day',   'TARGET2'),
    ('EUR', '2026-04-03', 'Good Friday',       'TARGET2'),
    ('EUR', '2026-04-06', 'Easter Monday',     'TARGET2'),
    ('EUR', '2026-05-01', 'Labour Day',        'TARGET2'),
    ('EUR', '2026-12-25', 'Christmas Day',     'TARGET2'),
    ('EUR', '2026-12-26', 'St Stephen''s Day', 'TARGET2'),
    ('EUR', '2027-01-01', 'New Year''s Day',   'TARGET2'),
    ('EUR', '2027-03-26', 'Good Friday',       'TARGET2'),
    ('EUR', '2027-03-29', 'Easter Monday',     'TARGET2'),
    ('EUR', '2027-05-01', 'Labour Day',        'TARGET2'),
    ('EUR', '2027-12-25', 'Christmas Day',     'TARGET2'),
    ('EUR', '2027-12-26', 'St Stephen''s Day', 'TARGET2');

-- ============================ GBP (Bank of England) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('GBP', '2025-01-01', 'New Year''s Day',                 'BANK_OF_ENGLAND'),
    ('GBP', '2025-04-18', 'Good Friday',                     'BANK_OF_ENGLAND'),
    ('GBP', '2025-04-21', 'Easter Monday',                   'BANK_OF_ENGLAND'),
    ('GBP', '2025-05-05', 'Early May Bank Holiday',          'BANK_OF_ENGLAND'),
    ('GBP', '2025-05-26', 'Spring Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2025-08-25', 'Summer Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2025-12-25', 'Christmas Day',                   'BANK_OF_ENGLAND'),
    ('GBP', '2025-12-26', 'Boxing Day',                      'BANK_OF_ENGLAND'),
    ('GBP', '2026-01-01', 'New Year''s Day',                 'BANK_OF_ENGLAND'),
    ('GBP', '2026-04-03', 'Good Friday',                     'BANK_OF_ENGLAND'),
    ('GBP', '2026-04-06', 'Easter Monday',                   'BANK_OF_ENGLAND'),
    ('GBP', '2026-05-04', 'Early May Bank Holiday',          'BANK_OF_ENGLAND'),
    ('GBP', '2026-05-25', 'Spring Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2026-08-31', 'Summer Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2026-12-25', 'Christmas Day',                   'BANK_OF_ENGLAND'),
    ('GBP', '2026-12-28', 'Boxing Day (substitute)',         'BANK_OF_ENGLAND'),
    ('GBP', '2027-01-01', 'New Year''s Day',                 'BANK_OF_ENGLAND'),
    ('GBP', '2027-03-26', 'Good Friday',                     'BANK_OF_ENGLAND'),
    ('GBP', '2027-03-29', 'Easter Monday',                   'BANK_OF_ENGLAND'),
    ('GBP', '2027-05-03', 'Early May Bank Holiday',          'BANK_OF_ENGLAND'),
    ('GBP', '2027-05-31', 'Spring Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2027-08-30', 'Summer Bank Holiday',             'BANK_OF_ENGLAND'),
    ('GBP', '2027-12-27', 'Christmas Day (substitute)',      'BANK_OF_ENGLAND'),
    ('GBP', '2027-12-28', 'Boxing Day (substitute)',         'BANK_OF_ENGLAND');

-- ============================ JPY (Bank of Japan) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('JPY', '2025-01-01', 'New Year''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2025-01-02', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2025-01-03', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2025-01-13', 'Coming of Age Day',                'BANK_OF_JAPAN'),
    ('JPY', '2025-02-11', 'National Foundation Day',          'BANK_OF_JAPAN'),
    ('JPY', '2025-02-24', 'Emperor''s Birthday (observed)',   'BANK_OF_JAPAN'),
    ('JPY', '2025-03-20', 'Vernal Equinox',                   'BANK_OF_JAPAN'),
    ('JPY', '2025-04-29', 'Showa Day',                        'BANK_OF_JAPAN'),
    ('JPY', '2025-05-05', 'Children''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2025-05-06', 'Constitution Day (substitute)',    'BANK_OF_JAPAN'),
    ('JPY', '2025-07-21', 'Marine Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2025-08-11', 'Mountain Day',                     'BANK_OF_JAPAN'),
    ('JPY', '2025-09-15', 'Respect for the Aged Day',         'BANK_OF_JAPAN'),
    ('JPY', '2025-09-23', 'Autumnal Equinox',                 'BANK_OF_JAPAN'),
    ('JPY', '2025-10-13', 'Sports Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2025-11-03', 'Culture Day',                      'BANK_OF_JAPAN'),
    ('JPY', '2025-11-24', 'Labor Thanksgiving (observed)',    'BANK_OF_JAPAN'),
    ('JPY', '2026-01-01', 'New Year''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2026-01-02', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2026-01-03', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2026-01-12', 'Coming of Age Day',                'BANK_OF_JAPAN'),
    ('JPY', '2026-02-11', 'National Foundation Day',          'BANK_OF_JAPAN'),
    ('JPY', '2026-02-23', 'Emperor''s Birthday',              'BANK_OF_JAPAN'),
    ('JPY', '2026-03-20', 'Vernal Equinox',                   'BANK_OF_JAPAN'),
    ('JPY', '2026-04-29', 'Showa Day',                        'BANK_OF_JAPAN'),
    ('JPY', '2026-05-04', 'Greenery Day',                     'BANK_OF_JAPAN'),
    ('JPY', '2026-05-05', 'Children''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2026-05-06', 'Constitution Day (substitute)',    'BANK_OF_JAPAN'),
    ('JPY', '2026-07-20', 'Marine Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2026-08-11', 'Mountain Day',                     'BANK_OF_JAPAN'),
    ('JPY', '2026-09-21', 'Respect for the Aged Day',         'BANK_OF_JAPAN'),
    ('JPY', '2026-09-22', 'Citizens'' Holiday (bridge)',      'BANK_OF_JAPAN'),
    ('JPY', '2026-09-23', 'Autumnal Equinox',                 'BANK_OF_JAPAN'),
    ('JPY', '2026-10-12', 'Sports Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2026-11-03', 'Culture Day',                      'BANK_OF_JAPAN'),
    ('JPY', '2026-11-23', 'Labor Thanksgiving',               'BANK_OF_JAPAN'),
    ('JPY', '2027-01-01', 'New Year''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2027-01-02', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2027-01-03', 'New Year Bank Holiday',            'BANK_OF_JAPAN'),
    ('JPY', '2027-01-11', 'Coming of Age Day',                'BANK_OF_JAPAN'),
    ('JPY', '2027-02-11', 'National Foundation Day',          'BANK_OF_JAPAN'),
    ('JPY', '2027-02-23', 'Emperor''s Birthday',              'BANK_OF_JAPAN'),
    ('JPY', '2027-03-22', 'Vernal Equinox (observed)',        'BANK_OF_JAPAN'),
    ('JPY', '2027-04-29', 'Showa Day',                        'BANK_OF_JAPAN'),
    ('JPY', '2027-05-03', 'Constitution Memorial Day',        'BANK_OF_JAPAN'),
    ('JPY', '2027-05-04', 'Greenery Day',                     'BANK_OF_JAPAN'),
    ('JPY', '2027-05-05', 'Children''s Day',                  'BANK_OF_JAPAN'),
    ('JPY', '2027-07-19', 'Marine Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2027-08-11', 'Mountain Day',                     'BANK_OF_JAPAN'),
    ('JPY', '2027-09-20', 'Respect for the Aged Day',         'BANK_OF_JAPAN'),
    ('JPY', '2027-09-23', 'Autumnal Equinox',                 'BANK_OF_JAPAN'),
    ('JPY', '2027-10-11', 'Sports Day',                       'BANK_OF_JAPAN'),
    ('JPY', '2027-11-03', 'Culture Day',                      'BANK_OF_JAPAN'),
    ('JPY', '2027-11-23', 'Labor Thanksgiving',               'BANK_OF_JAPAN');

-- ============================ CHF (SNB / SIX SIC) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('CHF', '2025-01-01', 'New Year''s Day',         'SNB_SIC'),
    ('CHF', '2025-01-02', 'Berchtold''s Day',        'SNB_SIC'),
    ('CHF', '2025-04-18', 'Good Friday',             'SNB_SIC'),
    ('CHF', '2025-04-21', 'Easter Monday',           'SNB_SIC'),
    ('CHF', '2025-05-29', 'Ascension Day',           'SNB_SIC'),
    ('CHF', '2025-06-09', 'Whit Monday',             'SNB_SIC'),
    ('CHF', '2025-08-01', 'National Day',            'SNB_SIC'),
    ('CHF', '2025-12-25', 'Christmas Day',           'SNB_SIC'),
    ('CHF', '2025-12-26', 'St Stephen''s Day',       'SNB_SIC'),
    ('CHF', '2026-01-01', 'New Year''s Day',         'SNB_SIC'),
    ('CHF', '2026-01-02', 'Berchtold''s Day',        'SNB_SIC'),
    ('CHF', '2026-04-03', 'Good Friday',             'SNB_SIC'),
    ('CHF', '2026-04-06', 'Easter Monday',           'SNB_SIC'),
    ('CHF', '2026-05-14', 'Ascension Day',           'SNB_SIC'),
    ('CHF', '2026-05-25', 'Whit Monday',             'SNB_SIC'),
    ('CHF', '2026-08-01', 'National Day',            'SNB_SIC'),
    ('CHF', '2026-12-25', 'Christmas Day',           'SNB_SIC'),
    ('CHF', '2026-12-26', 'St Stephen''s Day',       'SNB_SIC'),
    ('CHF', '2027-01-01', 'New Year''s Day',         'SNB_SIC'),
    ('CHF', '2027-05-06', 'Ascension Day',           'SNB_SIC'),
    ('CHF', '2027-05-17', 'Whit Monday',             'SNB_SIC'),
    ('CHF', '2027-12-25', 'Christmas Day',           'SNB_SIC'),
    ('CHF', '2027-12-26', 'St Stephen''s Day',       'SNB_SIC');

-- ============================ AUD (Australia / Sydney) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('AUD', '2025-01-01', 'New Year''s Day',               'AU_SETTLEMENT'),
    ('AUD', '2025-01-27', 'Australia Day (observed)',      'AU_SETTLEMENT'),
    ('AUD', '2025-04-18', 'Good Friday',                   'AU_SETTLEMENT'),
    ('AUD', '2025-04-21', 'Easter Monday',                 'AU_SETTLEMENT'),
    ('AUD', '2025-04-25', 'ANZAC Day',                     'AU_SETTLEMENT'),
    ('AUD', '2025-06-09', 'King''s Birthday',              'AU_SETTLEMENT'),
    ('AUD', '2025-12-25', 'Christmas Day',                 'AU_SETTLEMENT'),
    ('AUD', '2025-12-26', 'Boxing Day',                    'AU_SETTLEMENT'),
    ('AUD', '2026-01-01', 'New Year''s Day',               'AU_SETTLEMENT'),
    ('AUD', '2026-01-26', 'Australia Day',                 'AU_SETTLEMENT'),
    ('AUD', '2026-04-03', 'Good Friday',                   'AU_SETTLEMENT'),
    ('AUD', '2026-04-06', 'Easter Monday',                 'AU_SETTLEMENT'),
    ('AUD', '2026-04-25', 'ANZAC Day',                     'AU_SETTLEMENT'),
    ('AUD', '2026-06-08', 'King''s Birthday',              'AU_SETTLEMENT'),
    ('AUD', '2026-12-25', 'Christmas Day',                 'AU_SETTLEMENT'),
    ('AUD', '2026-12-28', 'Boxing Day (substitute)',       'AU_SETTLEMENT'),
    ('AUD', '2027-01-01', 'New Year''s Day',               'AU_SETTLEMENT'),
    ('AUD', '2027-01-26', 'Australia Day',                 'AU_SETTLEMENT'),
    ('AUD', '2027-03-26', 'Good Friday',                   'AU_SETTLEMENT'),
    ('AUD', '2027-03-29', 'Easter Monday',                 'AU_SETTLEMENT'),
    ('AUD', '2027-04-25', 'ANZAC Day',                     'AU_SETTLEMENT'),
    ('AUD', '2027-06-14', 'King''s Birthday',              'AU_SETTLEMENT'),
    ('AUD', '2027-12-27', 'Christmas Day (substitute)',    'AU_SETTLEMENT'),
    ('AUD', '2027-12-28', 'Boxing Day (substitute)',       'AU_SETTLEMENT');

-- ============================ NZD (New Zealand / Wellington) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('NZD', '2025-01-01', 'New Year''s Day',               'NZ_SETTLEMENT'),
    ('NZD', '2025-01-02', 'Day after New Year''s Day',     'NZ_SETTLEMENT'),
    ('NZD', '2025-02-06', 'Waitangi Day',                  'NZ_SETTLEMENT'),
    ('NZD', '2025-04-18', 'Good Friday',                   'NZ_SETTLEMENT'),
    ('NZD', '2025-04-21', 'Easter Monday',                 'NZ_SETTLEMENT'),
    ('NZD', '2025-04-25', 'ANZAC Day',                     'NZ_SETTLEMENT'),
    ('NZD', '2025-06-02', 'King''s Birthday',              'NZ_SETTLEMENT'),
    ('NZD', '2025-06-20', 'Matariki',                      'NZ_SETTLEMENT'),
    ('NZD', '2025-10-27', 'Labour Day',                    'NZ_SETTLEMENT'),
    ('NZD', '2025-12-25', 'Christmas Day',                 'NZ_SETTLEMENT'),
    ('NZD', '2025-12-26', 'Boxing Day',                    'NZ_SETTLEMENT'),
    ('NZD', '2026-01-01', 'New Year''s Day',               'NZ_SETTLEMENT'),
    ('NZD', '2026-01-02', 'Day after New Year''s Day',     'NZ_SETTLEMENT'),
    ('NZD', '2026-02-06', 'Waitangi Day',                  'NZ_SETTLEMENT'),
    ('NZD', '2026-04-03', 'Good Friday',                   'NZ_SETTLEMENT'),
    ('NZD', '2026-04-06', 'Easter Monday',                 'NZ_SETTLEMENT'),
    ('NZD', '2026-06-01', 'King''s Birthday',              'NZ_SETTLEMENT'),
    ('NZD', '2026-07-10', 'Matariki',                      'NZ_SETTLEMENT'),
    ('NZD', '2026-10-26', 'Labour Day',                    'NZ_SETTLEMENT'),
    ('NZD', '2026-12-25', 'Christmas Day',                 'NZ_SETTLEMENT'),
    ('NZD', '2026-12-28', 'Boxing Day (substitute)',       'NZ_SETTLEMENT'),
    ('NZD', '2027-01-01', 'New Year''s Day',               'NZ_SETTLEMENT'),
    ('NZD', '2027-02-08', 'Waitangi Day (observed)',       'NZ_SETTLEMENT'),
    ('NZD', '2027-03-26', 'Good Friday',                   'NZ_SETTLEMENT'),
    ('NZD', '2027-03-29', 'Easter Monday',                 'NZ_SETTLEMENT'),
    ('NZD', '2027-06-07', 'King''s Birthday',              'NZ_SETTLEMENT'),
    ('NZD', '2027-06-25', 'Matariki',                      'NZ_SETTLEMENT'),
    ('NZD', '2027-10-25', 'Labour Day',                    'NZ_SETTLEMENT'),
    ('NZD', '2027-12-27', 'Christmas Day (substitute)',    'NZ_SETTLEMENT'),
    ('NZD', '2027-12-28', 'Boxing Day (substitute)',       'NZ_SETTLEMENT');

-- ============================ CAD (Bank of Canada) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('CAD', '2025-01-01', 'New Year''s Day',               'BANK_OF_CANADA'),
    ('CAD', '2025-04-18', 'Good Friday',                   'BANK_OF_CANADA'),
    ('CAD', '2025-05-19', 'Victoria Day',                  'BANK_OF_CANADA'),
    ('CAD', '2025-07-01', 'Canada Day',                    'BANK_OF_CANADA'),
    ('CAD', '2025-08-04', 'Civic Holiday',                 'BANK_OF_CANADA'),
    ('CAD', '2025-09-01', 'Labour Day',                    'BANK_OF_CANADA'),
    ('CAD', '2025-09-30', 'National Day for Truth and Reconciliation', 'BANK_OF_CANADA'),
    ('CAD', '2025-10-13', 'Thanksgiving Day',              'BANK_OF_CANADA'),
    ('CAD', '2025-12-25', 'Christmas Day',                 'BANK_OF_CANADA'),
    ('CAD', '2025-12-26', 'Boxing Day',                    'BANK_OF_CANADA'),
    ('CAD', '2026-01-01', 'New Year''s Day',               'BANK_OF_CANADA'),
    ('CAD', '2026-04-03', 'Good Friday',                   'BANK_OF_CANADA'),
    ('CAD', '2026-05-18', 'Victoria Day',                  'BANK_OF_CANADA'),
    ('CAD', '2026-07-01', 'Canada Day',                    'BANK_OF_CANADA'),
    ('CAD', '2026-08-03', 'Civic Holiday',                 'BANK_OF_CANADA'),
    ('CAD', '2026-09-07', 'Labour Day',                    'BANK_OF_CANADA'),
    ('CAD', '2026-09-30', 'National Day for Truth and Reconciliation', 'BANK_OF_CANADA'),
    ('CAD', '2026-10-12', 'Thanksgiving Day',              'BANK_OF_CANADA'),
    ('CAD', '2026-12-25', 'Christmas Day',                 'BANK_OF_CANADA'),
    ('CAD', '2026-12-28', 'Boxing Day (substitute)',       'BANK_OF_CANADA'),
    ('CAD', '2027-01-01', 'New Year''s Day',               'BANK_OF_CANADA'),
    ('CAD', '2027-03-26', 'Good Friday',                   'BANK_OF_CANADA'),
    ('CAD', '2027-05-24', 'Victoria Day',                  'BANK_OF_CANADA'),
    ('CAD', '2027-07-01', 'Canada Day',                    'BANK_OF_CANADA'),
    ('CAD', '2027-08-02', 'Civic Holiday',                 'BANK_OF_CANADA'),
    ('CAD', '2027-09-06', 'Labour Day',                    'BANK_OF_CANADA'),
    ('CAD', '2027-09-30', 'National Day for Truth and Reconciliation', 'BANK_OF_CANADA'),
    ('CAD', '2027-10-11', 'Thanksgiving Day',              'BANK_OF_CANADA'),
    ('CAD', '2027-12-27', 'Christmas Day (substitute)',    'BANK_OF_CANADA'),
    ('CAD', '2027-12-28', 'Boxing Day (substitute)',       'BANK_OF_CANADA');

-- ============================ MXN (Banco de México) ============================
INSERT INTO currency_holidays (currency, holiday_date, name, source) VALUES
    ('MXN', '2025-01-01', 'New Year''s Day',               'BANXICO'),
    ('MXN', '2025-02-03', 'Constitution Day',              'BANXICO'),
    ('MXN', '2025-03-17', 'Benito Juárez Birthday',        'BANXICO'),
    ('MXN', '2025-04-17', 'Maundy Thursday',               'BANXICO'),
    ('MXN', '2025-04-18', 'Good Friday',                   'BANXICO'),
    ('MXN', '2025-05-01', 'Labour Day',                    'BANXICO'),
    ('MXN', '2025-09-16', 'Independence Day',              'BANXICO'),
    ('MXN', '2025-11-17', 'Revolution Day',                'BANXICO'),
    ('MXN', '2025-12-25', 'Christmas Day',                 'BANXICO'),
    ('MXN', '2026-01-01', 'New Year''s Day',               'BANXICO'),
    ('MXN', '2026-02-02', 'Constitution Day',              'BANXICO'),
    ('MXN', '2026-03-16', 'Benito Juárez Birthday',        'BANXICO'),
    ('MXN', '2026-04-02', 'Maundy Thursday',               'BANXICO'),
    ('MXN', '2026-04-03', 'Good Friday',                   'BANXICO'),
    ('MXN', '2026-05-01', 'Labour Day',                    'BANXICO'),
    ('MXN', '2026-09-16', 'Independence Day',              'BANXICO'),
    ('MXN', '2026-11-16', 'Revolution Day',                'BANXICO'),
    ('MXN', '2026-12-25', 'Christmas Day',                 'BANXICO'),
    ('MXN', '2027-01-01', 'New Year''s Day',               'BANXICO'),
    ('MXN', '2027-02-01', 'Constitution Day',              'BANXICO'),
    ('MXN', '2027-03-15', 'Benito Juárez Birthday',        'BANXICO'),
    ('MXN', '2027-03-25', 'Maundy Thursday',               'BANXICO'),
    ('MXN', '2027-03-26', 'Good Friday',                   'BANXICO'),
    ('MXN', '2027-05-01', 'Labour Day',                    'BANXICO'),
    ('MXN', '2027-09-16', 'Independence Day',              'BANXICO'),
    ('MXN', '2027-11-15', 'Revolution Day',                'BANXICO'),
    ('MXN', '2027-12-25', 'Christmas Day',                 'BANXICO');

COMMIT;
