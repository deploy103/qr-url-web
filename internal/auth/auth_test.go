package auth_test

import (
	"strings"
	"testing"

	"github.com/mvtp/qrurl/internal/auth"
)

// fastParams keeps the tests quick without weakening what they check.
func fastParams() auth.Argon2idParams {
	p := auth.DefaultParams()
	p.Memory = 8 * 1024
	p.Iterations = 1
	return p
}

func TestHashAndVerify(t *testing.T) {
	const password = "correct horse battery staple"
	hash, err := auth.HashPassword(password, fastParams())
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Fatalf("hash is not in the PHC argon2id format: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("the hash contains the plaintext password")
	}

	ok, err := auth.VerifyPassword(hash, password)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("the correct password was rejected")
	}

	for _, wrong := range []string{"", "correct horse battery stapl", "Correct horse battery staple", " " + password} {
		ok, err := auth.VerifyPassword(hash, wrong)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): %v", wrong, err)
		}
		if ok {
			t.Errorf("wrong password %q was accepted", wrong)
		}
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, err := auth.HashPassword("same password twice", fastParams())
	if err != nil {
		t.Fatal(err)
	}
	b, err := auth.HashPassword("same password twice", fastParams())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical, so the salt is not random")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=16$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=0,t=0,p=0$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!$aGFzaA",
		"$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
	}
	for _, c := range cases {
		if _, err := auth.VerifyPassword(c, "whatever"); err == nil {
			t.Errorf("malformed hash %q was accepted", c)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := auth.ValidatePassword("short"); err == nil {
		t.Error("a short password should be rejected")
	}
	if err := auth.ValidatePassword(strings.Repeat("a", auth.MaxPasswordLength+1)); err == nil {
		t.Error("an oversized password should be rejected")
	}
	if err := auth.ValidatePassword("일이삼사오육칠팔구십십일십이"); err != nil {
		t.Errorf("a multibyte password of sufficient length should be accepted: %v", err)
	}
	if err := auth.ValidatePassword("correct horse battery staple"); err != nil {
		t.Errorf("a good password was rejected: %v", err)
	}
}

func TestTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		token, err := auth.NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if len(token) < 40 {
			t.Fatalf("token %q is shorter than expected", token)
		}
		if seen[token] {
			t.Fatal("NewToken produced a duplicate")
		}
		seen[token] = true
	}
}

func TestHashToken(t *testing.T) {
	h := auth.HashToken("abc")
	if len(h) != 64 {
		t.Fatalf("HashToken returned %d characters, want 64", len(h))
	}
	if h == auth.HashToken("abd") {
		t.Fatal("different tokens hash to the same value")
	}
	if h != auth.HashToken("abc") {
		t.Fatal("HashToken is not deterministic")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !auth.ConstantTimeEqual("token", "token") {
		t.Error("identical tokens should compare equal")
	}
	if auth.ConstantTimeEqual("token", "tokeN") {
		t.Error("different tokens should not compare equal")
	}
	if auth.ConstantTimeEqual("token", "") {
		t.Error("an empty token should never match")
	}
}

func TestNewPassword(t *testing.T) {
	p, err := auth.NewPassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.ValidatePassword(p); err != nil {
		t.Fatalf("the generated password fails the policy: %v", err)
	}
}

// TestDummyHash checks the record used to equalise login timing when no
// account exists: it must parse and verify like a real one, and never match.
func TestDummyHash(t *testing.T) {
	h := auth.DummyHash()
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("DummyHash is not an argon2id record: %q", h)
	}
	if h != auth.DummyHash() {
		t.Fatal("DummyHash should be computed once and reused")
	}
	ok, err := auth.VerifyPassword(h, "any password an attacker might try")
	if err != nil {
		t.Fatalf("the dummy hash does not parse: %v", err)
	}
	if ok {
		t.Fatal("the dummy hash matched a guess")
	}
}
