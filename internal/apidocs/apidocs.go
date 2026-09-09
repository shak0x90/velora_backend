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
  .velora-detail {
    margin-top: 12px; padding: 14px 16px;
    background: #f8f2ee; border: 1px solid #ecdfd8; border-radius: 12px;
    font: 13px/1.7 ui-monospace, SFMono-Regular, Menlo, monospace;
    color: #4a3129; max-width: 74ch;
  }
  .velora-detail dt {
    color: #8a7168; font-weight: 600; float: left; width: 9em; clear: left;
  }
  .velora-detail dd { margin: 0 0 2px 9em; word-break: break-all; }
  .velora-detail .next {
    margin: 10px 0 0; padding-top: 10px; border-top: 1px solid #ecdfd8;
    font-family: system-ui, sans-serif; line-height: 1.6;
  }
  .velora-warn {
    margin-top: 14px !important; padding: 10px 14px;
    background: #fdf3f1; border-left: 3px solid #c45d4b; border-radius: 4px;
  }
</style>
</head>
<body>
<div class="velora-note">
  <h1>Velora API</h1>
  <p>
    Most endpoints need an account. The button below makes you a complete one —
    a real row in the database, with a filled-in profile — and authorizes this
    page with its token, so every endpoint works immediately.
  </p>
  <div class="velora-actions">
    <button class="velora-btn" id="velora-signin" type="button">
      Create a test account and authorize
    </button>
    <span class="velora-status" id="velora-status"></span>
  </div>
  <div class="velora-detail" id="velora-detail" hidden></div>
  <p>
    Signing in through <code>POST /auth/register</code>, <code>/auth/login</code>
    or <code>/auth/refresh</code> below also authorizes automatically, so you
    never have to copy a token by hand.
  </p>
  <p class="velora-warn">
    <strong>This writes to the real database.</strong> Accounts made here are
    permanent until someone deletes them, and every like, block and report you
    send is a real row. Use it on the test server, not against anything you
    care about — and never paste a password you use elsewhere, because this
    server is plain HTTP.
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

  // A bare account is not much use: almost every endpoint answers 409 until a
  // profile exists, which reads like a broken API rather than an unfinished
  // signup. So this registers *and* onboards, and then says exactly what it
  // made — the point of a test tool is that nothing about it is a mystery.
  var FIRST_NAMES = ["Robin", "Sam", "Alex", "Jordan", "Casey", "Riley", "Quinn"];

  function esc(text) {
    return String(text).replace(/[&<>"]/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c];
    });
  }

  function describe(details) {
    var rows = "";
    for (var i = 0; i < details.rows.length; i++) {
      rows += "<dt>" + esc(details.rows[i][0]) + "</dt><dd>" +
              esc(details.rows[i][1]) + "</dd>";
    }
    var box = document.getElementById("velora-detail");
    box.innerHTML = "<dl>" + rows + "</dl>" +
      "<p class=\"next\">" + details.next + "</p>";
    box.hidden = false;
  }

  function post(path, body, token) {
    var headers = { "content-type": "application/json" };
    if (token) headers.authorization = "Bearer " + token;
    return fetch(API_BASE + path, {
      method: "POST",
      headers: headers,
      body: JSON.stringify(body),
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (parsed) {
        if (!r.ok) {
          throw new Error(parsed.detail || (path + " answered " + r.status));
        }
        return parsed;
      });
    });
  }

  document.getElementById("velora-signin").addEventListener("click", function () {
    var button = this;
    var suffix = Math.random().toString(36).slice(2, 8);
    var email = "try+" + suffix + "@velora.test";
    var name = FIRST_NAMES[Math.floor(Math.random() * FIRST_NAMES.length)];
    var session = null;

    button.disabled = true;
    status.textContent = "Creating " + email + "…";

    post("/auth/register", { email: email, password: "disposable-test-pw" })
      .then(function (registered) {
        session = registered;
        authorize(session.accessToken);
        status.textContent = "Filling in a profile…";
        return post("/me/onboarding", {
          firstName: name,
          dateOfBirth: "1994-06-15",
          gender: "woman",
          pronouns: "she/her",
          city: "Brooklyn",
          neighborhood: "Fort Greene",
          occupation: "Ceramicist",
          education: "NYU",
          bio: "A test account. Kiln on weekends, tacos after, and strong opinions about the G train.",
          interests: ["Books", "Coffee", "Hiking", "Art", "Travel", "Food"],
          relationshipIntent: "longTerm",
          communicationStyle: "thoughtful",
          preferences: {
            interestedIn: ["woman", "man", "nonBinary", "other"],
            minAge: 18,
            maxAge: 99,
            maxDistanceKm: 500,
          },
          lifestyle: { languages: ["English"], heightCm: 170 },
          prompts: [
            { id: "p1", question: "A perfect Sunday", answer: "Kiln, then tacos." },
          ],
        }, session.accessToken);
      })
      .then(function (me) {
        status.innerHTML = "<strong>Authorized.</strong> Every request below now runs as this account.";
        describe({
          rows: [
            ["Name", me.profile.firstName + ", " + me.profile.age],
            ["Email", email],
            ["Password", "disposable-test-pw"],
            ["Profile id", me.profile.id],
            ["Sees", "everyone, 18-99, within 500 km"],
            ["Token expires", new Date(Date.now() + 15 * 60000).toLocaleTimeString()],
          ],
          next:
            "Preferences are deliberately wide open so <code>GET /discover</code> " +
            "returns other test accounts rather than nothing. Try <code>GET /me</code>, " +
            "then <code>GET /discover</code>, then <code>POST /likes</code> with an id " +
            "from the feed. Press the button again for a second account if you want to " +
            "watch two of them match.",
        });
      })
      .catch(function (error) {
        // Say what the server said. This is a debugging tool; a generic
        // failure message defeats the whole point of it.
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
