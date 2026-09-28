# deploy/otel — tracing pipeline (Task 9.3.11, spec §19.12, §24 #112)

trace_id continuity **HTTP → Aeron → C++ core → Aeron → Go**, exported to
Jaeger (dev) / Tempo (prod) via an OTLP-compatible in-repo model — the
module graph deliberately carries **no OpenTelemetry SDK**; services emit
spec-compliant OTLP/HTTP JSON that any collector/backend ingests
unchanged.

## Pieces

| Component | Where | Role |
|---|---|---|
| W3C propagation | `services/internal/middleware/tracing.go` | parse/continue inbound `traceparent`, mint hop span id, `X-Trace-ID` |
| Span model + sampling | `services/internal/tracing/` (`tracer.go`, `span.go`) | OTel-compatible SpanData; head 1% + tail on errors/slow (spec §19.12.3) |
| HTTP server spans | `services/internal/tracing/http.go` | `tracing.Middleware` adopts the hop identity → SERVER span per request |
| Aeron seam | `services/internal/tracing/aeron.go` | 64-byte `EXCTRACE` block; C++ echoes the field on response frames |
| Exporters | `services/internal/tracing/exporter.go` | `OTLPHTTPExporter` (collector), `FileExporter` (JSONL), `BatchExporter` (bounded, drop-counted) |
| Collector | `deploy/otel/otel-collector.yaml` | OTLP/HTTP+gRPC in → Jaeger + Tempo out |
| Dev backend | `deploy/otel/jaeger-compose.yaml` | Jaeger all-in-one + collector sidecar |

## Span vocabulary (Task 9.3.11 AC)

`order.submit` (gateway → Aeron offer) · `order.match` (C++ matching,
remote-parented onto the frame's trace block) · `settlement.t+1`
(settlement leg) · `marketdata.tick` (MD publish hop). Constants live in
`tracer.go` (`SpanOrderSubmit` …); the C++ core joins by echoing the
`EXCTRACE` header block verbatim on every frame derived from a command.

## Aeron byte contract (§19.12 "Aeron trace_id header format")

```
offset 0   8B  magic "EXCTRACE"
offset 8   55B W3C traceparent ASCII "00-{32hex}-{16hex}-{2hex}"
offset 63  1B  reserved (0)
```

Go side: `tracing.InjectAeronTrace(ctx, buf)` before `offer()`.
Consumer side: `tracing.ContextFromAeron(ctx, frame)` →
`tracer.Start(ctx, "order.match", tracing.KindConsumer, …)`.
Absent/invalid block ⇒ untraced frame, never a fault.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `EXC_OTLP_ENDPOINT` | — | Collector URL, e.g. `http://otel-collector:4318/v1/traces` |
| `EXC_OTLP_FILE` | — | JSONL file exporter (node-local scrape / dev) |
| `EXC_TRACE_SAMPLE_ALL` | `0` | `1` disables sampling (dev/staging) |
| `EXC_TRUST_PROXY` | `0` | (existing) — unrelated to tracing internals |

Metrics on `/metrics`: `tracing_spans_exported`,
`tracing_spans_dropped` (bounded-queue drop counter — alert on
sustained > 0; see `deploy/prometheus/rules/exchange-alerts.yml`).

## Dev quickstart

```bash
docker compose -f deploy/otel/jaeger-compose.yaml up -d
EXC_OTLP_ENDPOINT=http://127.0.0.1:14318/v1/traces \
EXC_TRACE_SAMPLE_ALL=1 go run ./services/cmd/gateway
# Jaeger UI: http://127.0.0.1:16686  (service: order-gateway)
```

## Failure posture (spec §2.7)

Tracing never backpressures the order path: `BatchExporter` drops
(counted) when the queue is full or the collector is down; a malformed
`traceparent`/frame block starts a fresh root span instead of erroring.
A collector outage therefore degrades observability only — no request
impact.
