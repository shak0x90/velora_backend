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
  .velora-actions { margin-top: 14px; display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
  .velora-btn {
    font: 600 14px system-ui, sans-serif; cursor: pointer;
    background: #c45d4b; color: #fff; border: 0;
    border-radius: 999px; padding: 10px 18px;
  }
  .velora-btn:hover { background: #a94a3a; }
  .velora-btn[disabled] { opacity: .6; cursor: default; }
  .velora-status { font: 14px/1.5 system-ui, sans-serif; color: #6b544c; }
  .velora-status strong { color: #2e7d5b; }
  .velora-status code {
    background: #f5ece7; padding: 1px 5px; border-radius: 4px;
    word-break: break-all;
  }
</style>
</head>
<body>
<div class="velora-note">
  <h1>Velora API</h1>
  <p>
    Most endpoints need an account. Press the button and you will have one —
    it registers a throwaway address and authorizes this page with the token.
  </p>
  <p>
    Signing in through <code>POST /auth/register</code>, <code>/auth/login</code>
    or <code>/auth/refresh</code> below also authorizes automatically, so you
    never have to copy a token by hand.
  </p>
  <div class="velora-actions">
    <button class="velora-btn" id="velora-signin" type="button">
      Create a test account and authorize
    </button>
    <span class="velora-status" id="velora-status"></span>
  </div>
  <p>
    Everything here writes to a real database. Use throwaway addresses, and do
    not paste a password you use anywhere else — this server is plain HTTP.
  </p>
</div>
<div id="swagger"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
<script>
  // The page is served at /docs, at /api/docs, and at /apitest. Only the
  // middle two sit behind the nginx prefix, so the button has to reach the
  // same base URL the spec's server dropdown defaults to.
  var API_BASE =
    location.pathname.indexOf("/apitest") === 0 || location.pathname.indexOf("/api/") === 0
      ? "/api"
      : "";

  var status = document.getElementById("velora-status");

  function authorize(token) {
    window.ui.authActions.authorize({
      bearerAuth: {
        name: "bearerAuth",
        schema: { type: "http", scheme: "bearer" },
        value: token,
      },
    });
  }

  function tokenFrom(response) {
    try {
      var body = response.body;
      if (!body && response.text) body = JSON.parse(response.text);
      return body && body.accessToken ? body.accessToken : null;
    } catch (e) {
      return null;
    }
  }

  window.ui = SwaggerUIBundle({
    spec: SPEC_PLACEHOLDER,
    dom_id: "#swagger",
    deepLinking: true,
    persistAuthorization: true,
    tryItOutEnabled: true,
    displayRequestDuration: true,
    defaultModelsExpandDepth: 0,
    docExpansion: "list",
    // Signing in from any of the auth endpoints authorizes the page. Copying a
    // token by hand is a step with no purpose beyond giving someone the chance
    // to paste the wrong one.
    responseInterceptor: function (response) {
      if (
        response.status >= 200 && response.status < 300 &&
        /\/auth\/(register|login|refresh|social)(\?|$)/.test(response.url || "")
      ) {
        var token = tokenFrom(response);
        if (token) {
          authorize(token);
          status.innerHTML = "<strong>Authorized.</strong> Every endpoint below will use this token.";
        }
      }
      return response;
    },
  });

  document.getElementById("velora-signin").addEventListener("click", function () {
    var button = this;
    var email = "try+" + Math.random().toString(36).slice(2, 10) + "@velora.test";
    button.disabled = true;
    status.textContent = "Creating " + email + "…";

    fetch(API_BASE + "/auth/register", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ email: email, password: "disposable-test-pw" }),
    })
      .then(function (r) {
        return r.json().then(function (body) {
          return { ok: r.ok, body: body };
        });
      })
      .then(function (result) {
        if (!result.ok || !result.body.accessToken) {
          // Show what the server actually said rather than a generic failure;
          // this button is a debugging tool and hiding the reason defeats it.
          throw new Error(result.body.detail || "the server refused the request");
        }
        authorize(result.body.accessToken);
        status.innerHTML =
          "<strong>Authorized as " + email + "</strong> — " +
          "the account has no profile yet, so start with <code>POST /me/onboarding</code>.";
      })
      .catch(function (error) {
        status.textContent = "That did not work: " + error.message;
      })
      .finally(function () {
        button.disabled = false;
      });
  });
</script>
</body>
</html>`

// init substitutes the embedded document into the page once, at startup,
// rather than on every request.
func init() {
	page = strings.Replace(page, "SPEC_PLACEHOLDER", string(spec), 1)
}
