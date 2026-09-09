// Package apidocs serves the OpenAPI document and a browser explorer for it.
//
// Two audiences, one source. The JSON is what Postman, Insomnia, or a code
// generator import; the HTML is Swagger UI pointed at that same document, so a
// person can sign in and fire a request without leaving the page.
//
// The spec is hand-written and embedded. Generating it from the handlers would
// keep it honest automatically, but Go has no annotations here and every
// generator wants the code shaped around it — so this is a file that must be
// updated alongside a route, and Routes says so.
package apidocs

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed openapi.json
var spec []byte

// Routes registers the explorer. Only called when the config allows it.
//
// Adding an endpoint means adding it to openapi.json. Nothing enforces that,
// which is the cost of a hand-written spec: a stale document is worse than no
// document, because it gets believed.
func Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// A cached stale spec after a deploy is exactly the confusion this
		// exists to prevent.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(spec)
	})

	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(page))
	})
}

// page is Swagger UI with the document inlined rather than fetched.
//
// Inlining sidesteps a real problem: this page is reachable at /docs directly,
// at /api/docs through nginx, and at /apitest through an alias. A relative
// spec URL resolves differently at each, and an absolute one is wrong locally.
// The bytes are already here, so there is nothing to resolve.
var page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Velora API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
<style>
  body { margin: 0; background: #fbf7f4; }
  .swagger-ui .topbar { display: none; }
  .velora-note {
    font: 14px/1.6 system-ui, -apple-system, sans-serif;
    background: #fff; color: #4a3129;
    border-bottom: 1px solid #ecdfd8;
    padding: 18px 24px;
  }
  .velora-note h1 { margin: 0 0 6px; font-size: 18px; }
  .velora-note code { background: #f5ece7; padding: 1px 5px; border-radius: 4px; }
  .velora-note p { margin: 6px 0 0; max-width: 74ch; }
</style>
</head>
<body>
<div class="velora-note">
  <h1>Velora API</h1>
  <p>
    To call anything that needs an account: expand
    <code>POST /auth/register</code> or <code>POST /auth/login</code>, run it,
    copy <code>accessToken</code> from the response, then press
    <strong>Authorize</strong> at the top right and paste it.
  </p>
  <p>
    Everything here writes to a real database. Use throwaway addresses, and do
    not paste a password you use anywhere else — this server is plain HTTP.
  </p>
</div>
<div id="swagger"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
<script>
  window.ui = SwaggerUIBundle({
    spec: SPEC_PLACEHOLDER,
    dom_id: "#swagger",
    deepLinking: true,
    persistAuthorization: true,
    tryItOutEnabled: true,
    displayRequestDuration: true,
    defaultModelsExpandDepth: 0,
    docExpansion: "list",
  });
</script>
</body>
</html>`

// init substitutes the embedded document into the page once, at startup,
// rather than on every request.
func init() {
	page = strings.Replace(page, "SPEC_PLACEHOLDER", string(spec), 1)
}
