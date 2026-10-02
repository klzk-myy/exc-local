-- 285_dora_ict_register
-- Phase-09 Task 9.3.15 (item 4) / spec §19.5 / §24 #171 — DORA Art. 28
-- ICT third-party register as data, not prose. Mirrors and supersedes
-- the register table in docs/ops/dora-third-party-register.md (the doc
-- stays the human narrative; this table is the queryable, alertable,
-- audit-trailed source of truth).
--
-- ict_providers: one row per registered ICT provider/service pair —
-- provider → service → functions supported → locations/subcontractors →
-- concentration → audit/access/termination terms → exit strategy →
-- substitution/insourcing test state → renewal/review cadence.
--
-- ict_provider_reviews: append-only review/renewal/substitution-test/
-- concentration-reassessment events — the "review on onboarding,
-- renewal, annually; reassess on outage" audit trail.

BEGIN;

CREATE TABLE ict_providers (
    id                        BIGSERIAL PRIMARY KEY,
    name                      TEXT        NOT NULL,
    ict_service               TEXT        NOT NULL,
    functions_supported       TEXT        NOT NULL,
    locations_subcontractors  TEXT        NOT NULL DEFAULT '',
    concentration             VARCHAR(6)  NOT NULL DEFAULT 'MEDIUM'
                              CHECK (concentration IN ('LOW','MEDIUM','HIGH')),
    -- audit/access/termination terms summary + termination notice days
    contract_terms            TEXT        NOT NULL DEFAULT '',
    termination_notice_days   INT,
    exit_strategy             TEXT        NOT NULL DEFAULT '',
    substitution_plan         TEXT        NOT NULL DEFAULT '',
    last_substitution_test_at TIMESTAMPTZ,
    renewal_at                TIMESTAMPTZ,
    next_review_at            TIMESTAMPTZ,   -- annual review cadence
    owner                     TEXT        NOT NULL DEFAULT '',
    status                    VARCHAR(10) NOT NULL DEFAULT 'ACTIVE'
                              CHECK (status IN ('ACTIVE','EXITING','RETIRED')),
    notes                     TEXT        NOT NULL DEFAULT '',
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (name, ict_service)
);
CREATE INDEX ict_providers_due_ix ON ict_providers (next_review_at)
    WHERE status = 'ACTIVE';
CREATE INDEX ict_providers_renewal_ix ON ict_providers (renewal_at)
    WHERE status = 'ACTIVE' AND renewal_at IS NOT NULL;

CREATE TABLE ict_provider_reviews (
    id           BIGSERIAL PRIMARY KEY,
    provider_id  BIGINT      NOT NULL REFERENCES ict_providers(id),
    kind         VARCHAR(24) NOT NULL
                 CHECK (kind IN ('REVIEW','RENEWAL','SUBSTITUTION_TEST',
                                 'CONCENTRATION_REASSESS','EXIT_PLAN')),
    outcome      VARCHAR(10) NOT NULL
                 CHECK (outcome IN ('PASS','FAIL','ACCEPTED','DEFERRED')),
    evidence_ref TEXT        NOT NULL DEFAULT '',  -- drill report row / artifact ref
    notes        TEXT        NOT NULL DEFAULT '',
    actor        BIGINT,
    reviewed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ict_provider_reviews_provider_ix
    ON ict_provider_reviews (provider_id, reviewed_at DESC);

-- Seed the documented register (docs/ops/dora-third-party-register.md
-- §1) so the data store starts in sync with the published contract.
INSERT INTO ict_providers (name, ict_service, functions_supported,
    locations_subcontractors, concentration, contract_terms,
    exit_strategy, substitution_plan, owner, notes) VALUES
('Cloud edge/WAF (Cloudflare/Fastly-class)',
 'Edge ingress, DDoS, Anycast DNS reroute',
 'all public endpoints; DR edge reroute (§18.6.4)',
 'global PoPs', 'HIGH',
 'contract SLA + status page; 30d termination',
 'secondary DNS provider pre-staged; direct-origin IP allowlist emergency mode',
 'annual D1-adjacent substitution test (pending Task 9.3.13 deploy)',
 'Vendor/Procurement', ''),
('PagerDuty',
 'On-call paging (P0–P2 receivers)',
 'incident paging',
 'US/EU processing', 'MEDIUM',
 'standard SaaS terms',
 'SMS/phone-tree fallback (runbooks/incident-escalation.md)',
 'quarterly paging test',
 'Vendor/Procurement', ''),
('Slack',
 'Ops comms (#exchange-ops + incident channels)',
 'ops comms, P2/P3 alerts',
 'US/EU', 'MEDIUM',
 'standard terms',
 'email bridge + PagerDuty notes fallback',
 'N/A (comms only)',
 'Vendor/Procurement', ''),
('AWS S3 (or S3-compatible)',
 'WAL archive (WORM), CH backup, PG PITR archive, Parquet cold tier',
 'DR replay, backups, retention',
 'region-pinned + CRR to secondary', 'HIGH',
 'Object Lock compliance mode; contract exit 90d',
 'second provider bucket mirror (CRR target); devs3 proves portability',
 'D1/D3/D4 drills verify restore in-region',
 'Vendor/Procurement', ''),
('Refinitiv',
 'Price oracle source A',
 'mark/index pricing, liquidations',
 'vendor DCs', 'MEDIUM',
 'feed license',
 'second feed mandatory (<2 feeds → PRICE_ORACLE_UNAVAILABLE)',
 'oracle feed-loss test (pending Phase-19.5)',
 'Vendor/Procurement', ''),
('Bloomberg BFIX',
 'Price oracle source B',
 'mark/index pricing, liquidations',
 'vendor DCs', 'MEDIUM',
 'feed license',
 'second feed mandatory (<2 feeds → PRICE_ORACLE_UNAVAILABLE)',
 'oracle feed-loss test (pending Phase-19.5)',
 'Vendor/Procurement', ''),
('ECB reference rates',
 'Price oracle source C',
 'mark/index pricing, liquidations',
 'ECB', 'LOW',
 'public reference',
 'second feed mandatory (<2 feeds → PRICE_ORACLE_UNAVAILABLE)',
 'oracle feed-loss test (pending Phase-19.5)',
 'Vendor/Procurement', ''),
('CLS Bank',
 'PvP settlement (§17.6)',
 'FX settlement finality',
 'CLS network', 'HIGH',
 'CLS member terms',
 'bilateral gross settlement fallback (§17.7 netting/SSIs)',
 'settlement-failure drill (pending Phase-24)',
 'Vendor/Procurement', 'High for settlement path'),
('Banking rails (SWIFT/SEPA/FedNow/ACH/CHAPS/TARGET2)',
 'Funding in/out, nostro',
 'client money movement',
 'per rail', 'MEDIUM',
 'per-rail agreements',
 'alternate rail routing per currency (Phase-11)',
 'rail-down scenario in DR catalog (pending Phase-11)',
 'Vendor/Procurement', 'concentration medium per rail'),
('Kubernetes/cloud or bare-metal DC operator',
 'Hosts for Go services + metal',
 'compute substrate',
 'primary+secondary regions', 'HIGH',
 'DC contract + audit rights',
 'multi-region (§18.4), bare-metal deploy docs',
 'D1 drill',
 'Vendor/Procurement', ''),
('Grafana/Prometheus (self-hosted)',
 'Metrics/dashboards/alertmanager',
 'observability',
 'self-hosted', 'LOW',
 'OSS',
 'ops.alerts.* NATS fail-safe path independent',
 'covered by AlertDispatchErrors checks',
 'Vendor/Procurement', ''),
('Vault/KMS (HashiCorp Vault or cloud KMS)',
 'Secrets store',
 'all credentials',
 'primary+secondary region replicas', 'MEDIUM',
 'license/cloud terms',
 'secondary-region copies, break-glass procedure (secrets-inventory.md)',
 'D7 decrypt-in-secondary drill',
 'Vendor/Procurement', ''),
('Sanctions/PEP screening vendor',
 'Real-time screening',
 'compliance gate',
 'vendor SaaS', 'MEDIUM',
 'vendor SLA',
 'scoped degradation mode (SANCTIONS_SERVICE_UNAVAILABLE) keeps venue up (Phase-21 AC #43)',
 'provider-outage test (pending Phase-21)',
 'Vendor/Procurement', '');

COMMIT;
