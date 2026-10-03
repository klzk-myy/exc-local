# frontend.headers.inc — SPA/static-asset security header set.
# Canonical contract: frontend/public/_headers (Phase-10 Task 10.3.1
# item 8). Included per-location because nginx add_header replaces the
# inherited set wholesale — locations that add Cache-Control must still
# emit these. /api and /ws responses carry the gateway's own Task
# 5.3.29 hardening set, so this file is only included on static
# locations.
add_header X-Frame-Options DENY always;
add_header Referrer-Policy no-referrer always;
add_header X-Content-Type-Options nosniff always;
add_header Content-Security-Policy "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' wss:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; upgrade-insecure-requests" always;
add_header Cross-Origin-Opener-Policy same-origin always;
add_header Cross-Origin-Resource-Policy same-origin always;
add_header Permissions-Policy "geolocation=(), camera=(), microphone=(), payment=(), usb=(), interest-cohort=()" always;
