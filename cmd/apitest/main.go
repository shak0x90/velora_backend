// Command apitest exercises a running Velora API over HTTP.
//
// It exists because verifying the server needs to happen against the server:
// unit tests cover the pure logic, but nothing in `go test` touches Postgres,
// and the interesting failures — a block that leaks, a race that loses a
// match, a query that only misbehaves with real rows — live on the other side
// of the network.
//
//	apitest suite                 run every check
//	apitest suite safety social   run only those
//	apitest list                  show the suites
//	apitest call GET /me --as someone@velora.test
//	apitest call POST /likes '{"profileId":"..."}' --as someone@velora.test
//	apitest login someone@velora.test
//
// `call` signs in for you and prints the response, so poking one endpoint does
// not mean copying a bearer token around by hand.
//
// Every account it creates is a throwaway on whatever database the API is
// pointed at. Point it at something you would not mind filling with rows
// named "Api A".
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// baseURL defaults to the loopback address the API listens on, so running this
// on the box needs no arguments and never reaches the public interface.
func baseURL() string {
	if url := os.Getenv("VELORA_API_URL"); url != "" {
		return strings.TrimRight(url, "/")
	}
	return "http://127.0.0.1:8080"
}

// testPassword is deliberately not a secret. Every account this creates is
// disposable, and the test server is plain HTTP anyway.
const testPassword = "disposable-test-pw"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	switch args[0] {
	case "suite":
		os.Exit(runSuites(args[1:]))
	case "list":
		for _, s := range suites {
			fmt.Printf("  %-12s %s\n", s.name, s.about)
		}
	case "call":
		os.Exit(runCall(args[1:]))
	case "login":
		os.Exit(runLogin(args[1:]))
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `apitest — exercise a running Velora API

Usage:
  apitest suite [name...]              Run checks (default: all)
  apitest list                         List the suites
  apitest call METHOD PATH [json]      One request, signed in
  apitest login EMAIL [password]       Print a bearer token

Flags for call:
  --as EMAIL        Sign in as this account first (registers it if new)
  --token TOKEN     Use this token instead of signing in
  --anon            Send no Authorization header

Environment:
  VELORA_API_URL    Defaults to %s

Every account created is a throwaway with the password %q.
`, baseURL(), testPassword)
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type client struct {
	http *http.Client
}

func newClient() *client {
	return &client{http: &http.Client{Timeout: 30 * time.Second}}
}

type response struct {
	status int
	body   any
	raw    string
}

// ok reports a 2xx. Checks read better as `res.ok()` than as arithmetic.
func (r response) ok() bool { return r.status >= 200 && r.status < 300 }

// list returns the body as a slice, or nil when it is not one. Checks call
// this constantly, and a type assertion at every site drowns out the assertion.
func (r response) list() []any {
	items, _ := r.body.([]any)
	return items
}

// field walks a dotted path: res.field("user.id"). Missing anything yields nil
// rather than panicking, because a check should fail with a message rather
// than a stack trace.
func (r response) field(path string) any {
	var current any = r.body
	for _, part := range strings.Split(path, ".") {
		object, isObject := current.(map[string]any)
		if !isObject {
			return nil
		}
		current = object[part]
	}
	return current
}

func (r response) str(path string) string {
	value, _ := r.field(path).(string)
	return value
}

func (r response) num(path string) float64 {
	value, _ := r.field(path).(float64)
	return value
}

func (r response) boolean(path string) bool {
	value, _ := r.field(path).(bool)
	return value
}

// short is a one-line rendering for failure messages, long bodies trimmed.
func (r response) short() string {
	text := strings.TrimSpace(r.raw)
	if len(text) > 220 {
		text = text[:220] + "…"
	}
	return fmt.Sprintf("%d %s", r.status, text)
}

func (c *client) do(method, path string, body any, token string) response {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return response{status: -1, raw: err.Error()}
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, baseURL()+path, reader)
	if err != nil {
		return response{status: -1, raw: err.Error()}
	}
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return response{status: -1, raw: err.Error()}
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, raw: string(raw)}
	if len(bytes.TrimSpace(raw)) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out
}

// account is a throwaway signed-in user, with the token already in hand.
type account struct {
	client *client
	email  string
	ID     string
	token  string
	name   string
}

func (a *account) do(method, path string, body any) response {
	return a.client.do(method, path, body, a.token)
}

// register makes a fresh account. The suffix keeps concurrent runs from
// colliding; nothing here cleans up after itself, because inspecting the
// wreckage of a failed run is usually how you find out what happened.
func (c *client) register(tag string) (*account, error) {
	email := fmt.Sprintf("apitest+%s%d@velora.test", tag, time.Now().UnixNano()%1_000_000_000)
	res := c.do("POST", "/auth/register", map[string]any{
		"email": email, "password": testPassword,
	}, "")
	if !res.ok() {
		return nil, fmt.Errorf("register %s: %s", tag, res.short())
	}
	return &account{
		client: c,
		email:  email,
		ID:     res.str("user.id"),
		token:  res.str("accessToken"),
	}, nil
}

// onboarded registers and completes onboarding, which most checks need before
// they can do anything at all.
func (c *client) onboarded(tag, gender string, interestedIn []string, extra map[string]any) (*account, error) {
	user, err := c.register(tag)
	if err != nil {
		return nil, err
	}
	user.name = "Api " + strings.ToUpper(tag)

	body := map[string]any{
		"firstName":   user.name,
		"dateOfBirth": "1993-06-15",
		"gender":      gender,
		"occupation":  "Tester",
		"interests":   []string{"Books", "Coffee"},
		"preferences": map[string]any{
			"interestedIn":  interestedIn,
			"minAge":        18,
			"maxAge":        99,
			"maxDistanceKm": 500,
		},
	}
	for key, value := range extra {
		body[key] = value
	}

	if res := user.do("POST", "/me/onboarding", body); !res.ok() {
		return nil, fmt.Errorf("onboard %s: %s", tag, res.short())
	}
	return user, nil
}

func (c *client) signIn(email, password string) (string, error) {
	res := c.do("POST", "/auth/login", map[string]any{
		"email": email, "password": password,
	}, "")
	if !res.ok() {
		return "", fmt.Errorf("login: %s", res.short())
	}
	return res.str("accessToken"), nil
}

// signInOrRegister is what `--as` uses: an unknown address becomes a new
// throwaway rather than an error, so poking an endpoint never has to start
// with "first create an account".
func (c *client) signInOrRegister(email string) (string, error) {
	if token, err := c.signIn(email, testPassword); err == nil {
		return token, nil
	}
	res := c.do("POST", "/auth/register", map[string]any{
		"email": email, "password": testPassword,
	}, "")
	if !res.ok() {
		return "", fmt.Errorf("could not sign in or register %s: %s", email, res.short())
	}
	return res.str("accessToken"), nil
}

// ---------------------------------------------------------------------------
// One-off requests
// ---------------------------------------------------------------------------

func runCall(args []string) int {
	var as, token string
	anon := false
	positional := []string{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--as":
			if i+1 < len(args) {
				i++
				as = args[i]
			}
		case "--token":
			if i+1 < len(args) {
				i++
				token = args[i]
			}
		case "--anon":
			anon = true
		default:
			positional = append(positional, args[i])
		}
	}
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "usage: apitest call METHOD PATH [json] [--as EMAIL]")
		return 2
	}

	method, path := strings.ToUpper(positional[0]), positional[1]
	body := ""
	if len(positional) > 2 {
		body = positional[2]
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	c := newClient()
	if !anon && token == "" {
		if as == "" {
			as = fmt.Sprintf("apitest+cli%d@velora.test", time.Now().UnixNano()%1_000_000)
			fmt.Fprintf(os.Stderr, "no --as given; using a fresh account %s\n", as)
		}
		signed, err := c.signInOrRegister(as)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		token = signed
	}

	var payload any
	if body != "" {
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			fmt.Fprintf(os.Stderr, "the body is not valid JSON: %v\n", err)
			return 2
		}
	}

	res := c.do(method, path, payload, token)
	fmt.Printf("%s %s → %d\n", method, path, res.status)
	if res.body != nil {
		if pretty, err := json.MarshalIndent(res.body, "", "  "); err == nil {
			fmt.Println(string(pretty))
		} else {
			fmt.Println(res.raw)
		}
	} else if res.raw != "" {
		fmt.Println(res.raw)
	}

	if res.ok() {
		return 0
	}
	return 1
}

func runLogin(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: apitest login EMAIL [password]")
		return 2
	}
	password := testPassword
	if len(args) > 1 {
		password = args[1]
	}
	token, err := newClient().signIn(args[0], password)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(token)
	return 0
}
