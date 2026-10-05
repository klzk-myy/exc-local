FROM postgres:16-bookworm

# pg_partman for trades daily partitioning (spec §5, Phase-01 Task 1.3.3).
# --only-upgrade libexpat1: upstream postgres:16-bookworm still ships
# deb12u3 (6 HIGH/CRITICAL CVEs, fixed in deb12u4); upgrade in the layer so
# the gate passes regardless of base-tag republish lag. Same for
# libpcre2-8-0 (CVE-2026-103111, fixed 10.42-1+deb12u2) and the perl 5.36
# family (CVE-2026-13221, fixed 5.36.0-7+deb12u4) — --only-upgrade never
# installs new packages, so this stays a patch-level bump of what the base
# image already ships.
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-16-partman \
    && apt-get install -y --only-upgrade --no-install-recommends \
       libexpat1 libpcre2-8-0 perl perl-base perl-modules-5.36 libperl5.36 \
    && rm -rf /var/lib/apt/lists/*

# Auto-enable extensions on first init
RUN printf '%s\n' \
    'CREATE EXTENSION IF NOT EXISTS pg_partman;' \
    'CREATE EXTENSION IF NOT EXISTS pgcrypto;' \
    > /docker-entrypoint-initdb.d/00-extensions.sql
