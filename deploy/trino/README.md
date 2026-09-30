# Trino federated cold-query layer (dev)

Phase-09 Task 9.3.24 AC: *"Cold data queryable via Presto/Trino federated
query"* — `docs/ops/data-tiering-policy.md` §5 option 2.

## Topology

| Container | Role | Endpoint |
|---|---|---|
| `exc-dev-trino` | `trinodb/trino:476` coordinator+worker | `http://127.0.0.1:18443` (container :8080) |
| `exc-archive-s3gw` | versitygw posix S3 gateway (busybox + static binary — same pattern as `ch_restore_drill.sh`) | `http://127.0.0.1:17072`, alias `archive-s3` on `exc-dev_default` |
| `exc-dev-postgres-1` | existing dev PG (hot+warm+ledger) | `postgres:5432` on `exc-dev_default` |

## Catalogs (`deploy/trino/catalog/`)

- **`exchange_pg`** — `postgresql` connector → `exchange` DB. Covers the
  **warm tier** (`warm.*` detached partitions, `archive_restore.*`
  re-materialized partitions) and the **archive ledger**
  (`public.partition_archive_log` / `partition_tier_state` /
  `partition_tier_log`). Production equivalent: point at the read replica.
- **`archive`** — `hive` connector + `fs.native-s3` over
  `s3://exchange-partition-archive` — the **cold tier**, bucket name =
  `internal/archiver.DefaultPartitionBucket`. Metastore is the
  (undocumented, testing-grade) `hive.metastore=file` on `local:///` rooted
  at `/var/trino` (`trino-metastore` volume). Production swaps only the
  metastore block for `hive.metastore.uri=thrift://…` or Glue — the S3
  filesystem config and `external_location` layout are unchanged.

## Run it

```bash
deploy/scripts/trino_federation.sh        # up + seed + query (full demo)
deploy/scripts/trino_federation.sh up     # s3gw + bucket + trino only
deploy/scripts/trino_federation.sh seed   # export->PUT->register external table
deploy/scripts/trino_federation.sh query  # proof queries via /v1/statement
deploy/scripts/trino_federation.sh down   # remove containers + volumes
```

`docker-compose.dev.yml` also carries a `trino` service for `docker compose
-f docker-compose.dev.yml up -d trino`; the archive-S3 gateway and the
metastore chmod are still done by the script (busybox+static-binary pattern
doesn't fit compose builds). For the compose-managed volume:

```bash
docker run --rm -v exc-dev_trino-metastore:/var/trino busybox \
  sh -c 'mkdir -p /var/trino/metastore && chmod -R 0777 /var/trino'
```

The trino server runs as uid 1000; without this, `CREATE SCHEMA` on the
`archive` catalog fails with AccessDenied on the file metastore. Queries on
`exchange_pg` work regardless.

## Seed data lineage

`seed` exports `archive_restore.itest_orders_p2020_01` — a partition the
real archiver restored from an archive — to parquet+zstd via a throwaway
`clickhouse-local` (reads PG through `postgresql()`, writes
`FORMAT Parquet` + `output_format_parquet_compression_method=zstd`), PUTs it
to `s3://exchange-partition-archive/itest_orders/itest_orders_p2020_01/itest_orders_p2020_01.parquet`
(the archiver `s3Keys()` layout: `{parent}/{partition}/{partition}.parquet`),
and registers it as `archive.cold.itest_orders_p2020_01`.

## Layout note (was: known gap — now resolved)

Hive treats **every non-hidden file** under `external_location` as table
data. The archiver originally wrote `{partition}.manifest.json` beside
`{partition}.parquet`, which broke scans with "not a Parquet file".
Resolved at the source: `s3Keys()` now writes manifests under
`{parent}/{partition}/_manifests/` — engines skip `_`- and `.`-prefixed
paths, so a table registered on the partition prefix scans parquet only.
The seed in `trino_federation.sh` writes the real post-fix layout.

Column typing: `pg timestamptz` archives to `TIMESTAMP(MICROS, utc)`; the
Hive catalog has no `timestamptz` column type, so cold tables declare
`timestamp(6)` (`hive.timestamp-precision=MICROSECONDS`). Instants are
preserved; zone rendering is the Trino session zone.
