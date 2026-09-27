FROM postgres:16-bookworm

# pg_partman for trades daily partitioning (spec §5, Phase-01 Task 1.3.3)
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-16-partman \
    && rm -rf /var/lib/apt/lists/*

# Auto-enable extensions on first init
RUN printf '%s\n' \
    'CREATE EXTENSION IF NOT EXISTS pg_partman;' \
    'CREATE EXTENSION IF NOT EXISTS pgcrypto;' \
    > /docker-entrypoint-initdb.d/00-extensions.sql
