package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Access-token parsing is the boundary every authenticated route stands
// behind, and it had no tests. None of it needs a database, so there was never
// a good reason for that.

var testKey = []byte("a-test-signing-key-of-at-least-32-bytes")

func testService() *Service {
	return &Service{cfg: Config{SigningKey: testKey, AccessTokenTTL: 15 * time.Minute}}
}

func mint(t *testing.T, key []byte, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("could not mint a test token: %v", err)
	}
	return signed
}

func TestParseAccessTokenAcceptsOurOwn(t *testing.T) {
	s := testService()
	raw := mint(t, testKey, jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "a4f1c0de-0000-4000-8000-000000000001",
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Minute).Unix(),
	})

	sub, err := s.ParseAccessToken(raw)
	if err != nil {
		t.Fatalf("a valid token was rejected: %v", err)
	}
	if sub != "a4f1c0de-0000-4000-8000-000000000001" {
		t.Errorf("subject = %q, want the token's sub claim", sub)
	}
}

func TestParseAccessTokenRejectsTheDangerousCases(t *testing.T) {
	s := testService()
	now := time.Now()

	cases := []struct {
		name string
		raw  string
	}{
		{
			// The whole point of signing: a token minted elsewhere must not
			// authenticate here.
			name: "signed with another key",
			raw: mint(t, []byte("a-different-key-entirely-32-bytes!!"), jwt.SigningMethodHS256,
				jwt.MapClaims{"sub": "intruder", "exp": now.Add(time.Minute).Unix()}),
		},
		{
			// alg=none is the classic JWT forgery: drop the signature and tell
			// the verifier not to expect one.
			name: "unsigned, alg none",
			raw: func() string {
				token := jwt.NewWithClaims(jwt.SigningMethodNone,
					jwt.MapClaims{"sub": "intruder", "exp": now.Add(time.Minute).Unix()})
				signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
				if err != nil {
					t.Fatalf("could not mint the none-alg token: %v", err)
				}
				return signed
			}(),
		},
		{
			name: "expired",
			raw: mint(t, testKey, jwt.SigningMethodHS256, jwt.MapClaims{
				"sub": "someone", "iat": now.Add(-time.Hour).Unix(),
				"exp": now.Add(-time.Minute).Unix(),
			}),
		},
		{
			// An empty subject would authenticate the request as nobody, which
			// downstream code reads as a valid user id of "".
			name: "no subject",
			raw: mint(t, testKey, jwt.SigningMethodHS256, jwt.MapClaims{
				"exp": now.Add(time.Minute).Unix(),
			}),
		},
		{name: "not a token at all", raw: "nonsense"},
		{name: "empty", raw: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub, err := s.ParseAccessToken(c.raw)
			if err == nil {
				t.Fatalf("accepted a token that should be refused (subject %q)", sub)
			}
			if !errors.Is(err, ErrInvalidToken) {
				t.Errorf("error = %v, want ErrInvalidToken", err)
			}
			if sub != "" {
				t.Errorf("a refused token still yielded subject %q", sub)
			}
		})
	}
}

// A tampered payload must fail even though the header and signature are ours,
// because the signature covers the payload.
func TestParseAccessTokenRejectsATamperedPayload(t *testing.T) {
	s := testService()
	raw := mint(t, testKey, jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "victim", "exp": time.Now().Add(time.Minute).Unix(),
	})

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("expected three JWT segments, got %d", len(parts))
	}
	// Swap in a payload naming a different subject, keeping our signature.
	other := mint(t, []byte("irrelevant-key-for-payload-shape!!!!"), jwt.SigningMethodHS256,
		jwt.MapClaims{"sub": "attacker", "exp": time.Now().Add(time.Minute).Unix()})
	forged := parts[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]

	if _, err := s.ParseAccessToken(forged); err == nil {
		t.Error("a token with a swapped payload was accepted")
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "  Someone@Example.COM  ", want: "someone@example.com"},
		{in: "someone@example.com", want: "someone@example.com"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "not-an-email", wantErr: true},
		{in: "@example.com", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := normalizeEmail(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("normalizeEmail(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeEmail(%q) errored: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("normalizeEmail(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Case folding is what stops one address becoming two accounts.
func TestNormalizeEmailFoldsCaseSoOneAddressIsOneAccount(t *testing.T) {
	first, err := normalizeEmail("Person@Example.com")
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeEmail("PERSON@EXAMPLE.COM")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("the same address normalised two ways: %q and %q", first, second)
	}
}

func TestCheckPassword(t *testing.T) {
	if err := checkPassword(strings.Repeat("a", minPasswordLength)); err != nil {
		t.Errorf("a password at the minimum length was refused: %v", err)
	}
	if err := checkPassword(strings.Repeat("a", minPasswordLength-1)); !errors.Is(err, ErrPasswordTooShort) {
		t.Errorf("a short password gave %v, want ErrPasswordTooShort", err)
	}
	// bcrypt silently truncates past 72 bytes, so the ceiling is a correctness
	// rule rather than a nicety: without it two different long passwords can
	// open the same account.
	if err := checkPassword(strings.Repeat("a", maxPasswordBytes+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Errorf("an over-long password gave %v, want ErrPasswordTooLong", err)
	}
}

// The floor counts runes and the ceiling counts bytes, so a passphrase of
// multi-byte characters must not be refused for being "short".
func TestCheckPasswordCountsRunesNotBytesAtTheFloor(t *testing.T) {
	passphrase := strings.Repeat("é", minPasswordLength)
	if err := checkPassword(passphrase); err != nil {
		t.Errorf("a %d-character passphrase was refused: %v", minPasswordLength, err)
	}
}
