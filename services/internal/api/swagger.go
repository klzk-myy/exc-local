// Task 5.3.8/5.3.16 — Swagger UI at GET /developer.
//
// The page loads swagger-ui-dist from the unpkg CDN and points it at the
// live registry-generated document (GET /api/v1/openapi.json). No
// bundled assets — the portal is a thin shell over the generated spec.
package api

import "net/http"

// swaggerPage is the developer-portal shell.
const swaggerPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Exchange API — Developer Portal</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; }
    .top-banner { background:#0f1622; color:#dfe6f0; padding:.6rem 1rem;
      font-family:system-ui,sans-serif; font-size:.85rem; }
    .top-banner a { color:#7ab8ff; }
  </style>
</head>
<body>
<div class="top-banner">
  Exchange Order Gateway API — OpenAPI 3.1 generated live from the route registry
  (<a href="/api/v1/openapi.json">openapi.json</a>) ·
  <a href="/developer/migration">Deprecation &amp; migration guide</a>
</div>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  window.ui = SwaggerUIBundle({
    url: "/api/v1/openapi.json",
    dom_id: "#swagger-ui",
    deepLinking: true,
    docExpansion: "none",
    defaultModelsExpandDepth: -1
  });
</script>
</body>
</html>`

// DeveloperPortal serves GET /developer — Swagger UI bound to the live
// OpenAPI document.
func DeveloperPortal() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(swaggerPage))
	}
}
