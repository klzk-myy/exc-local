#!/usr/bin/env bash
# Provision the 7 canonical JetStream streams on the running exc-jetstream
# cluster (Task 1.3.11, spec §2.3.1). Thin wrapper around the testable Go
# implementation: services/internal/nats.EnsureStreams via cmd/natsctl.
#
# Usage:
#   ./deploy/nats/provision.sh            # uses config.yaml / EXC_NATS_URLS
#   EXC_NATS_URLS=nats://127.0.0.1:4222 ./deploy/nats/provision.sh
#
# Idempotent — safe to re-run; existing streams are reconciled, not reset.
set -euo pipefail
cd "$(dirname "$0")/../../services"
exec go run ./cmd/natsctl init
