# Local-drill image for settlement-service — used by
# deploy/scripts/kind_hpa_drill.sh (Phase-09 :63 HPA-on-live-cluster proof,
# Task 9.3.29). The shipped manifest pins
# registry.internal/exchange/settlement-service:CHANGE_ME, which no local
# cluster can pull; the overlay's kustomization substitutes this locally
# built image instead.
#
# Build context is this directory: the drill script compiles
# services/cmd/settlement (CGO_ENABLED=0, static) into ./bin first.
#
# busybox (not scratch) so the drill can `kubectl exec` a CPU stress loop —
# /health/live + /health/ready + /metrics stay served by the Go binary on
# :8083 exactly as the manifest probes expect.
FROM busybox:1.36
COPY bin/settlement /settlement
USER 10001
ENTRYPOINT ["/settlement"]
