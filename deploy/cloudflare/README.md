# deploy/cloudflare — L3/L4 + managed-WAF provider config (Task 9.3.13)

Provider-equivalent reference config. Cloudflare is the documented
provider; AWS Shield Advanced + AWS WAF maps 1:1 (Spectrum ↔ GA/Shield,
WAF managed rules ↔ AWS Managed Rules + challenge action). **Pending
infra**: real zone provisioning requires the production account — this
directory holds the intended state, not applied state.

## Zones

| Hostname | Product | Purpose |
|---|---|---|
| `api.exc.local` | proxied HTTP + WAF + challenge | REST API |
| `ws.exc.local` | proxied HTTP + WAF + challenge | WebSocket edge |
| `fix.exc.local` | **absent** — FIX is private connectivity / cross-connects; never proxied through the WAF per task text |

## terraform sketch (provider-agnostic intent)

```hcl
# Spectrum (TCP) covers nothing here — api/ws are HTTPS; FIX is private.
resource "cloudflare_zone_settings_override" "api" {
  zone_id = var.zone_id
  settings {
    ssl                      = "strict"
    min_tls_version          = "1.3"
    always_use_https         = "on"
    security_level           = "medium"   # → "under_attack" on incident
    challenge_ttl            = 1800
  }
}

resource "cloudflare_ruleset" "waf" {
  zone_id = var.zone_id
  kind    = "zone"
  phase   = "http_request_firewall_managed"
  rules {
    action = "execute"
    action_parameters { id = cloudflare_managed_ruleset.owasp.id }
    expression = "(http.host eq \"api.exc.local\" or http.host eq \"ws.exc.local\")"
  }
}

# L7 challenge mode — toggled per incident (ddos-playbook step 2).
resource "cloudflare_ruleset" "challenge" {
  zone_id = var.zone_id
  kind    = "zone"
  phase   = "http_request_firewall_custom"
  rules {
    action = "managed_challenge"
    expression = "(http.host eq \"api.exc.local\" and not ip.src in $whitelist_trading)"
    enabled    = var.challenge_mode   # false normal / true under attack
  }
}
```

## Event export

Logpush job `waf-events` → object storage → SIEM; a small exporter
(`waf_block`/`waf_challenge` counters) feeds the `edge.*` Prometheus
rules in `deploy/prometheus/rules/exchange-alerts.yml` and PagerDuty P2.

## Origin lockdown

HAProxy is the only listener the edge IPs reach: origin firewall drops
everything else (cloudflare IPs allowlist), TLS verify=strict requires a
valid origin cert from the secrets pipeline (Phase-13.5 Task 13.5.3.5).
Direct-origin probes never bypass the WAF.
