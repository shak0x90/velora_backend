package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// The checks.
//
// Each one reproduces a behaviour rather than asserting a patch is present: a
// fix that compiles is not the same as a fix that holds. Several exist because
// the behaviour was once wrong — the hidden-profile leak, the refresh race,
// the lost mutual match — and a check that only runs when someone remembers to
// look is not much of a check.

type suite struct {
	name  string
	about string
	run   func(*runner)
}

var suites = []suite{
	{"auth", "register, refresh rotation, token reuse", authSuite},
	{"profile", "onboarding, patching, validation", profileSuite},
	{"discovery", "feed, search, detail, suggestions", discoverySuite},
	{"social", "likes, matches, notifications, races", socialSuite},
	{"safety", "blocking, reporting, unmatching, moderation", safetySuite},
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

type runner struct {
	client *client
	failed []string
	group  string
}

func (r *runner) section(name string) {
	r.group = name
	fmt.Printf("\n  %s\n", name)
}

func (r *runner) check(label string, ok bool, detail ...any) {
	if ok {
		fmt.Printf("    ok    %s\n", label)
		return
	}
	where := label
	if r.group != "" {
		where = r.group + " / " + label
	}
	r.failed = append(r.failed, where)
	fmt.Printf("    FAIL  %s", label)
	if len(detail) > 0 && fmt.Sprint(detail[0]) != "" {
		fmt.Printf("  → %v", detail[0])
	}
	fmt.Println()
}

// fatal aborts one suite without taking the rest down: a suite that cannot set
// itself up says so and yields, rather than reporting twenty cascading
// failures that all mean the same thing.
type fatal struct{ err error }

func (r *runner) must(user *account, err error) *account {
	if err != nil {
		panic(fatal{err})
	}
	return user
}

func runSuites(names []string) int {
	selected := suites
	if len(names) > 0 {
		selected = nil
		for _, want := range names {
			found := false
			for _, s := range suites {
				if s.name == want {
					selected = append(selected, s)
					found = true
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "no suite called %q — try `apitest list`\n", want)
				return 2
			}
		}
	}

	fmt.Printf("apitest → %s\n", baseURL())

	c := newClient()
	if res := c.do("GET", "/healthz", nil, ""); !res.ok() {
		fmt.Fprintf(os.Stderr, "the API is not answering: %s\n", res.short())
		return 1
	}

	started := time.Now()
	total := []string{}
	for _, s := range selected {
		fmt.Printf("\n── %s: %s\n", s.name, s.about)
		r := &runner{client: c}
		runOne(s, r)
		total = append(total, r.failed...)
	}

	fmt.Printf("\n%s\n", strings.Repeat("─", 60))
	if len(total) == 0 {
		fmt.Printf("all checks passed in %s\n", time.Since(started).Round(time.Millisecond))
		return 0
	}
	fmt.Printf("%d failed in %s:\n", len(total), time.Since(started).Round(time.Millisecond))
	for _, name := range total {
		fmt.Printf("  · %s\n", name)
	}
	return 1
}

func runOne(s suite, r *runner) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if stop, isFatal := recovered.(fatal); isFatal {
				r.check("suite setup", false, stop.err)
				return
			}
			panic(recovered)
		}
	}()
	s.run(r)
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func authSuite(r *runner) {
	r.section("registration")
	user := r.must(r.client.register("auth"))
	r.check("register returns a token", user.token != "", user.token)
	r.check("register returns a user id", user.ID != "", user.ID)

	res := user.do("GET", "/me", nil)
	r.check("the token works", res.ok(), res.short())
	r.check("profile is null before onboarding", res.field("profile") == nil, res.short())

	r.section("refresh rotation")
	session := r.client.do("POST", "/auth/login",
		map[string]any{"email": user.email, "password": testPassword}, "")
	r.check("login works", session.ok(), session.short())
	refresh := session.str("refreshToken")

	rotated := r.client.do("POST", "/auth/refresh", map[string]any{"refreshToken": refresh}, "")
	r.check("refresh returns a new session", rotated.ok(), rotated.short())
	r.check("the refresh token rotates", rotated.str("refreshToken") != refresh,
		"the same token came back")

	fresh := r.client.do("GET", "/me", nil, rotated.str("accessToken"))
	r.check("the rotated access token works", fresh.ok(), fresh.short())

	// Spending one refresh token twice is indistinguishable from theft, so the
	// whole family goes.
	replayed := r.client.do("POST", "/auth/refresh", map[string]any{"refreshToken": refresh}, "")
	r.check("replaying a spent token is refused", replayed.status == 401, replayed.short())
	after := r.client.do("POST", "/auth/refresh",
		map[string]any{"refreshToken": rotated.str("refreshToken")}, "")
	r.check("reuse revokes the whole family", after.status == 401, after.short())

	r.section("concurrent refresh")
	// Two callers, one token. Exactly one may win, or a token buys two
	// sessions and reuse detection means nothing.
	racer := r.must(r.client.register("race"))
	pair := r.client.do("POST", "/auth/login",
		map[string]any{"email": racer.email, "password": testPassword}, "")
	shared := pair.str("refreshToken")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			codes[slot] = r.client.do("POST", "/auth/refresh",
				map[string]any{"refreshToken": shared}, "").status
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, code := range codes {
		if code == 200 {
			wins++
		}
	}
	r.check("exactly one concurrent refresh succeeds", wins == 1, codes)

	r.section("rejections")
	anon := r.client.do("GET", "/me", nil, "")
	r.check("no token is 401", anon.status == 401, anon.short())
	junk := r.client.do("GET", "/me", nil, "garbage")
	r.check("a junk token is 401", junk.status == 401, junk.short())
	dup := r.client.do("POST", "/auth/register",
		map[string]any{"email": user.email, "password": testPassword}, "")
	r.check("registering the same address twice is refused", !dup.ok(), dup.short())
	wrong := r.client.do("POST", "/auth/login",
		map[string]any{"email": user.email, "password": "not-the-password"}, "")
	r.check("a wrong password is refused", wrong.status == 401, wrong.short())
}

// ---------------------------------------------------------------------------
// Profile
// ---------------------------------------------------------------------------

func profileSuite(r *runner) {
	r.section("before onboarding")
	user := r.must(r.client.register("prof"))
	patch := user.do("PATCH", "/me", map[string]any{"bio": "too early"})
	r.check("patching with no profile is 409", patch.status == 409, patch.short())
	feed := user.do("GET", "/discover", nil)
	r.check("discovering with no profile is 409", feed.status == 409, feed.short())

	r.section("onboarding")
	created := user.do("POST", "/me/onboarding", map[string]any{
		"firstName":   "Api Prof",
		"dateOfBirth": "1994-03-12",
		"gender":      "woman",
		"occupation":  "Ceramicist",
		"bio":         "Kiln on weekends, tacos after, and strong opinions about the G train.",
		"interests":   []string{"Books", "Coffee", "Hiking"},
		"prompts": []map[string]any{
			{"id": "local1", "question": "A perfect Sunday", "answer": "Kiln, then tacos."},
		},
		"personalityAnswers": []map[string]any{
			{"questionId": "q1", "question": "Recharge how?", "answer": "Alone"},
		},
		"lifestyle": map[string]any{"heightCm": 170, "languages": []string{"English"}},
	})
	r.check("onboarding is 201", created.status == 201, created.short())
	r.check("age is derived from the birth date", created.num("profile.age") == 32,
		created.field("profile.age"))
	r.check("completion is computed", created.num("completion.percent") > 0,
		created.field("completion"))
	r.check("photos is an empty list, not null",
		created.field("profile.photos") != nil &&
			len(toList(created.field("profile.photos"))) == 0,
		created.field("profile.photos"))

	prompts := toList(created.field("profile.prompts"))
	mintedID := stringAt(prompts, 0, "id")
	r.check("the server mints a real prompt id",
		len(prompts) == 1 && mintedID != "" && mintedID != "local1", mintedID)

	r.section("patching")
	updated := user.do("PATCH", "/me", map[string]any{
		"bio":       "Updated bio, still about kilns.",
		"interests": []string{"Books", "  ", "Books", "Chess"},
	})
	r.check("patch is 200", updated.ok(), updated.short())
	r.check("the bio changed", updated.str("bio") == "Updated bio, still about kilns.",
		updated.field("bio"))
	r.check("tags are trimmed and deduped",
		fmt.Sprint(updated.field("interests")) == "[Books Chess]", updated.field("interests"))

	// A patch touching one field must not blank the rest.
	unchanged := user.do("PATCH", "/me", map[string]any{"pronouns": "she/her"})
	r.check("an untouched field survives a patch",
		unchanged.str("bio") == "Updated bio, still about kilns.", unchanged.field("bio"))

	r.section("validation")
	for _, bad := range []struct {
		label string
		body  map[string]any
	}{
		{"an unknown gender", map[string]any{"gender": "androgynous"}},
		{"an unknown intent", map[string]any{"relationshipIntent": "marriage"}},
		{"an inverted age range", map[string]any{
			"preferences": map[string]any{"minAge": 40, "maxAge": 30}}},
		{"an age range below 18", map[string]any{
			"preferences": map[string]any{"minAge": 15}}},
		{"an implausible height", map[string]any{
			"lifestyle": map[string]any{"heightCm": 12}}},
		{"a photo that is not yours", map[string]any{
			"photos": []string{"http://example.com/not-yours.jpg"}}},
		{"an empty first name", map[string]any{"firstName": "   "}},
	} {
		res := user.do("PATCH", "/me", bad.body)
		r.check(bad.label+" is refused", res.status == 400, res.short())
	}

	under := user.do("POST", "/me/onboarding", map[string]any{
		"firstName": "Kid", "dateOfBirth": "2015-01-01", "gender": "woman",
	})
	r.check("under 18 is refused", under.status == 400 && strings.Contains(under.raw, "18"),
		under.short())

	// A rejected patch must leave the row exactly as it was.
	after := user.do("GET", "/me", nil)
	r.check("a rejected patch changed nothing",
		after.str("profile.bio") == "Updated bio, still about kilns.",
		after.field("profile.bio"))
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func discoverySuite(r *runner) {
	viewer := r.must(r.client.onboarded("dva", "woman", []string{"man"}, nil))
	other := r.must(r.client.onboarded("dvb", "man", []string{"woman"},
		map[string]any{
			"occupation": "Distinctive Luthier",
			"interests":  []string{"Books", "Art"},
		}))

	r.section("the feed")
	feed := viewer.do("GET", "/discover", nil)
	r.check("discover is 200", feed.ok(), feed.short())
	r.check("the other profile appears", containsProfile(feed.list(), other.ID), len(feed.list()))

	if entry := findProfile(feed.list(), other.ID); entry != nil {
		r.check("compatibility is scored",
			numberAt(entry, "compatibility", "overallScore") > 0, entry)
		r.check("insights are generated",
			len(listAt(entry, "compatibility", "insights")) > 0, entry)
		r.check("the shared interest is found",
			len(listAt(entry, "compatibility", "commonInterests")) > 0, entry)
	}

	picks := viewer.do("GET", "/discover/daily-picks", nil)
	r.check("daily picks is 200", picks.ok(), picks.short())

	r.section("search")
	found := viewer.do("GET", "/search?q=Luthier", nil)
	r.check("search finds by occupation", containsProfile(found.list(), other.ID), found.short())
	quoted := viewer.do("GET", "/search?q=o%27brien", nil)
	r.check("a quote in the query is data, not syntax", quoted.ok(), quoted.short())
	empty := viewer.do("GET", "/search?q=zzzznobodyzzz", nil)
	r.check("a hopeless search is an empty list, not an error", empty.ok(), empty.short())

	r.section("one profile")
	detail := viewer.do("GET", "/profiles/"+other.ID, nil)
	r.check("profile detail is 200", detail.ok(), detail.short())
	r.check("it carries a compatibility read",
		detail.num("compatibility.overallScore") > 0, detail.short())

	suggestions := viewer.do("GET", "/profiles/"+other.ID+"/suggestions", nil)
	r.check("suggestions are returned",
		suggestions.ok() && len(suggestions.list()) > 0, suggestions.short())

	batch := viewer.do("GET", "/profiles?ids="+viewer.ID+","+other.ID, nil)
	r.check("batch lookup returns both", len(batch.list()) == 2, batch.short())

	missing := viewer.do("GET", "/profiles/not-a-uuid", nil)
	r.check("a malformed id is 404, not 500", missing.status == 404, missing.short())

	r.section("filters")
	// A filter nobody satisfies should empty the feed rather than error.
	narrow := viewer.do("GET", `/discover?filters={"minAge":99,"maxAge":100}`, nil)
	r.check("an impossible filter yields nothing",
		narrow.ok() && len(narrow.list()) == 0, narrow.short())
	broken := viewer.do("GET", "/discover?filters=not-json", nil)
	r.check("malformed filters are refused", broken.status == 400, broken.short())
}

// ---------------------------------------------------------------------------
// Social
// ---------------------------------------------------------------------------

func socialSuite(r *runner) {
	alice := r.must(r.client.onboarded("sca", "woman", []string{"man"}, nil))
	bob := r.must(r.client.onboarded("scb", "man", []string{"woman"}, nil))

	r.section("a one-sided like")
	liked := alice.do("POST", "/likes", map[string]any{
		"profileId": bob.ID, "kind": "prompt", "label": "Liked your answer",
	})
	r.check("liking is 200", liked.ok(), liked.short())
	r.check("it does not match yet", !liked.boolean("matched"), liked.short())

	feed := alice.do("GET", "/discover", nil)
	r.check("a liked profile leaves the feed", !containsProfile(feed.list(), bob.ID), feed.short())

	incoming := bob.do("GET", "/likes/incoming", nil)
	r.check("the recipient sees one incoming like", len(incoming.list()) == 1, incoming.short())

	notes := bob.do("GET", "/notifications", nil)
	r.check("the recipient is notified", strings.Contains(notes.raw, "liked you"), notes.short())
	// Who liked you is what the likes screen is for; a notification that names
	// the sender gives away the thing the screen exists to reveal.
	r.check("the notification does not name the sender",
		!strings.Contains(notes.raw, alice.name), notes.short())

	r.section("matching")
	back := bob.do("POST", "/likes", map[string]any{"profileId": alice.ID})
	r.check("liking back matches", back.boolean("matched"), back.short())

	mine := alice.do("GET", "/matches", nil)
	theirs := bob.do("GET", "/matches", nil)
	r.check("both sides have the match",
		len(mine.list()) == 1 && len(theirs.list()) == 1,
		fmt.Sprint(mine.short(), " / ", theirs.short()))
	r.check("it is one match, not two",
		stringAt(mine.list(), 0, "id") == stringAt(theirs.list(), 0, "id"),
		fmt.Sprint(mine.raw, theirs.raw))
	r.check("the consumed like is gone",
		len(bob.do("GET", "/likes/incoming", nil).list()) == 0, "")

	r.section("simultaneous mutual likes")
	// Both sides tapping at once must still produce exactly one match. Without
	// serialisation neither transaction sees the other and the match is lost.
	carol := r.must(r.client.onboarded("scc", "woman", []string{"man"}, nil))
	dave := r.must(r.client.onboarded("scd", "man", []string{"woman"}, nil))

	var wg sync.WaitGroup
	results := make([]response, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = carol.do("POST", "/likes", map[string]any{"profileId": dave.ID})
	}()
	go func() {
		defer wg.Done()
		results[1] = dave.do("POST", "/likes", map[string]any{"profileId": carol.ID})
	}()
	wg.Wait()

	matched := 0
	for _, res := range results {
		if res.boolean("matched") {
			matched++
		}
	}
	r.check("exactly one side reports the match", matched == 1, matched)
	r.check("carol ends up matched", len(carol.do("GET", "/matches", nil).list()) == 1, "")
	r.check("dave ends up matched", len(dave.do("GET", "/matches", nil).list()) == 1, "")

	r.section("passes and self-directed actions")
	eve := r.must(r.client.onboarded("sce", "woman", []string{"man"}, nil))
	pass := alice.do("POST", "/passes/"+eve.ID, nil)
	r.check("passing is 204", pass.status == 204, pass.short())
	again := alice.do("POST", "/passes/"+eve.ID, nil)
	r.check("passing twice is still fine", again.status == 204, again.short())
	stranger := alice.do("POST", "/passes/00000000-0000-0000-0000-000000000000", nil)
	r.check("passing a stranger is 404", stranger.status == 404, stranger.short())
	self := alice.do("POST", "/likes", map[string]any{"profileId": alice.ID})
	r.check("liking yourself is refused", self.status == 400, self.short())

	r.section("notifications")
	list := alice.do("GET", "/notifications", nil)
	r.check("notifications load", list.ok(), list.short())
	if items := list.list(); len(items) > 0 {
		id := stringAt(items, 0, "id")
		read := alice.do("POST", "/notifications/"+id+"/read", nil)
		r.check("marking read is 204", read.status == 204, read.short())
		r.check("it comes back read",
			boolAt(alice.do("GET", "/notifications", nil).list(), 0, "read"), "")
	}
}

// ---------------------------------------------------------------------------
// Safety
// ---------------------------------------------------------------------------

func safetySuite(r *runner) {
	watcher := r.must(r.client.onboarded("sfa", "woman", []string{"man"},
		map[string]any{"occupation": "Unmistakable Ceramicist"}))
	target := r.must(r.client.onboarded("sfb", "man", []string{"woman"},
		map[string]any{"occupation": "Unmistakable Luthier"}))

	r.section("hidden profiles")
	hide := target.do("PATCH", "/me", map[string]any{"hidden": true})
	r.check("a profile can hide itself", hide.ok(), hide.short())
	one := watcher.do("GET", "/profiles/"+target.ID, nil)
	r.check("single lookup refuses a hidden profile", one.status == 404, one.short())
	// The batch form was once a way around the single form's check.
	many := watcher.do("GET", "/profiles?ids="+target.ID, nil)
	r.check("batch lookup refuses it too", len(many.list()) == 0, many.short())
	own := target.do("GET", "/profiles?ids="+target.ID, nil)
	r.check("you can still see your own hidden profile", len(own.list()) == 1, own.short())
	_ = target.do("PATCH", "/me", map[string]any{"hidden": false})

	r.section("blocking")
	_ = watcher.do("POST", "/likes", map[string]any{"profileId": target.ID})
	paired := target.do("POST", "/likes", map[string]any{"profileId": watcher.ID})
	r.check("they match first", paired.boolean("matched"), paired.short())

	blocked := watcher.do("POST", "/blocks/"+target.ID, map[string]any{"reason": "apitest"})
	r.check("blocking is 204", blocked.status == 204, blocked.short())

	// A block that holds in three places out of four is not a block.
	r.check("gone from the blocker's feed",
		!containsProfile(watcher.do("GET", "/discover", nil).list(), target.ID), "")
	r.check("gone from the blocker's search",
		!containsProfile(watcher.do("GET", "/search?q=Luthier", nil).list(), target.ID), "")
	r.check("the blocker cannot open the profile",
		watcher.do("GET", "/profiles/"+target.ID, nil).status == 404, "")
	r.check("the blocker cannot batch-fetch it",
		len(watcher.do("GET", "/profiles?ids="+target.ID, nil).list()) == 0, "")
	r.check("the match is dissolved for the blocker",
		len(watcher.do("GET", "/matches", nil).list()) == 0, "")

	r.check("gone from the blocked person's feed",
		!containsProfile(target.do("GET", "/discover", nil).list(), watcher.ID), "")
	r.check("gone from the blocked person's search",
		!containsProfile(target.do("GET", "/search?q=Ceramicist", nil).list(), watcher.ID), "")
	r.check("the blocked person cannot open the profile",
		target.do("GET", "/profiles/"+watcher.ID, nil).status == 404, "")
	r.check("the match is dissolved for them too",
		len(target.do("GET", "/matches", nil).list()) == 0, "")

	refused := target.do("POST", "/likes", map[string]any{"profileId": watcher.ID})
	r.check("the blocked person cannot like back", refused.status == 404, refused.short())
	// Telling someone they were blocked is the one thing a block must not do.
	r.check("and is not told why",
		!strings.Contains(strings.ToLower(refused.raw), "block"), refused.short())

	list := watcher.do("GET", "/blocks", nil)
	r.check("the blocker sees their block list", len(list.list()) == 1, list.short())
	r.check("the blocked person's own list stays empty",
		len(target.do("GET", "/blocks", nil).list()) == 0, "")

	r.section("unblocking")
	lifted := watcher.do("DELETE", "/blocks/"+target.ID, nil)
	r.check("unblocking is 204", lifted.status == 204, lifted.short())
	r.check("the profile is reachable again",
		watcher.do("GET", "/profiles/"+target.ID, nil).ok(), "")
	r.check("the dissolved match stays dissolved",
		len(watcher.do("GET", "/matches", nil).list()) == 0, "")

	r.section("reporting")
	subject := r.must(r.client.onboarded("sfc", "man", []string{"woman"}, nil))
	filed := watcher.do("POST", "/reports", map[string]any{
		"profileId": subject.ID, "reason": "harassment",
		"detail": "filed by apitest", "context": "message",
	})
	r.check("reporting is 204", filed.status == 204, filed.short())
	twice := watcher.do("POST", "/reports", map[string]any{
		"profileId": subject.ID, "reason": "spam",
	})
	r.check("re-reporting updates rather than duplicating", twice.status == 204, twice.short())
	nonsense := watcher.do("POST", "/reports", map[string]any{
		"profileId": subject.ID, "reason": "vibes",
	})
	r.check("an unknown reason is refused", nonsense.status == 400, nonsense.short())
	itself := watcher.do("POST", "/reports", map[string]any{
		"profileId": watcher.ID, "reason": "spam",
	})
	r.check("reporting yourself is refused", itself.status == 400, itself.short())

	withBlock := target.do("POST", "/reports", map[string]any{
		"profileId": subject.ID, "reason": "harassment", "block": true,
	})
	r.check("report-and-block is 204", withBlock.status == 204, withBlock.short())
	r.check("report-and-block actually blocked",
		target.do("GET", "/profiles/"+subject.ID, nil).status == 404, "")

	r.section("unmatching")
	first := r.must(r.client.onboarded("sfd", "woman", []string{"man"}, nil))
	second := r.must(r.client.onboarded("sfe", "man", []string{"woman"}, nil))
	_ = first.do("POST", "/likes", map[string]any{"profileId": second.ID})
	_ = second.do("POST", "/likes", map[string]any{"profileId": first.ID})

	matchID := stringAt(first.do("GET", "/matches", nil).list(), 0, "id")
	dropped := first.do("DELETE", "/matches/"+matchID, nil)
	r.check("unmatching is 204", dropped.status == 204, dropped.short())
	r.check("gone for the one who unmatched",
		len(first.do("GET", "/matches", nil).list()) == 0, "")
	r.check("gone for the other side too",
		len(second.do("GET", "/matches", nil).list()) == 0, "")
	// An unmatch that puts them back in your feed tomorrow is not an unmatch.
	r.check("they do not return to the feed",
		!containsProfile(first.do("GET", "/discover", nil).list(), second.ID), "")
	repeated := first.do("DELETE", "/matches/"+matchID, nil)
	r.check("unmatching twice is a clean 404", repeated.status == 404, repeated.short())

	r.section("the moderation queue")
	// A 403 would confirm to whoever is probing that there is a moderation
	// surface here worth attacking.
	closed := watcher.do("GET", "/admin/reports", nil)
	r.check("an ordinary account gets 404, not 403", closed.status == 404, closed.short())
	anon := r.client.do("GET", "/admin/reports", nil, "")
	r.check("an unauthenticated caller gets nothing",
		anon.status == 401 || anon.status == 404, anon.short())
}

// ---------------------------------------------------------------------------
// Small helpers for poking at decoded JSON
// ---------------------------------------------------------------------------

func toList(value any) []any {
	items, _ := value.([]any)
	return items
}

func objectAt(items []any, index int) map[string]any {
	if index >= len(items) {
		return nil
	}
	object, _ := items[index].(map[string]any)
	return object
}

func stringAt(items []any, index int, key string) string {
	value, _ := objectAt(items, index)[key].(string)
	return value
}

func boolAt(items []any, index int, key string) bool {
	value, _ := objectAt(items, index)[key].(bool)
	return value
}

func numberAt(entry map[string]any, path ...string) float64 {
	var current any = entry
	for _, key := range path {
		object, isObject := current.(map[string]any)
		if !isObject {
			return 0
		}
		current = object[key]
	}
	value, _ := current.(float64)
	return value
}

func listAt(entry map[string]any, path ...string) []any {
	var current any = entry
	for _, key := range path {
		object, isObject := current.(map[string]any)
		if !isObject {
			return nil
		}
		current = object[key]
	}
	return toList(current)
}

// findProfile locates a ranked entry by profile id.
func findProfile(items []any, id string) map[string]any {
	for _, item := range items {
		entry, isObject := item.(map[string]any)
		if !isObject {
			continue
		}
		if profile, isProfile := entry["profile"].(map[string]any); isProfile &&
			profile["id"] == id {
			return entry
		}
	}
	return nil
}

func containsProfile(items []any, id string) bool {
	return findProfile(items, id) != nil
}
