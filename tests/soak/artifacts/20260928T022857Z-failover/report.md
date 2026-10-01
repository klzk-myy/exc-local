# Phase-02.5 warm-failover benchmark

- engine: `core/build/matching_engine`  shard=0 instrument=7
- trials: 2   warmup: 12s @ 8000/s   seed-wal: none
- target: recovery < 3000ms, fingerprint parity 100%

## Results

- fingerprint parity: 2/2
- max recovery_ms: 116

```json
{"trial":1,"ok":true,"recovery_ms":116,"fp_primary":"f53ff560ebdc1b58","fp_standby":"f53ff560ebdc1b58","parity":1,"determinism":1,"pre_tail":158321,"post_tail":284739,"post_status":"ok","post_dup_trade_ids":0,"post_seq_gaps":0}
{"trial":2,"ok":true,"recovery_ms":114,"fp_primary":"9daf133702762330","fp_standby":"9daf133702762330","parity":1,"determinism":1,"pre_tail":158851,"post_tail":285003,"post_status":"ok","post_dup_trade_ids":0,"post_seq_gaps":0}
```


## Verdict: **PASS**
