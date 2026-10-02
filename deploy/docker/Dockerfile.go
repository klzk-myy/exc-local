# Dockerfile.go — Go service image (prod docker redesign, 2026-10-02).
#
# One image carries every built daemon in /app/bin (bridge, oracle,
# marketdata, risk, settlement, compliance, analytics, recovery-orchestrator,
# sentinel_exporter, admin, gateway, fix, watchdogd); docker-compose.app.yml
# picks the binary per service via `command:`. Canonical Go deployment is
# Kubernetes (spec §19.1, Task 9.3.2, deploy/k8s/); this image serves the
# single-host compose alternative driven by deploy/scripts/dev_stack.sh
# (EXC_GO_MODE=docker / EXC_APP_MODE=docker).
#
# Build (from repo root):
#   docker build -f deploy/docker/Dockerfile.go -t exc-go-service:local .

FROM golang:1.26-bookworm AS builder

WORKDIR /src/services
COPY services/go.mod services/go.sum ./
RUN go mod download
COPY services/ ./
# cgo Aeron client links the vendored static lib via a repo-relative
# #cgo path (services/internal/ipc/aeron → core/third_party/aeron).
COPY core/ ../core/
RUN go build -o /out/ ./cmd/...

FROM debian:bookworm-slim AS runtime

# curl: compose healthchecks below probe /health* endpoints from inside the
# container. ca-certificates: outbound TLS (feeds, banking rails, SES).
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/ /app/bin/
RUN chmod +x /app/bin/*

ENV PATH="/app/bin:${PATH}"

# Overridden per service in docker-compose.app.yml; bare default runs gateway.
CMD ["gateway"]
