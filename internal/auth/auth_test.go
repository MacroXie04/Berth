package auth

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestValidUsername(t *testing.T) {
	valid := []string{"abc", "alice", "a1b", "a-b", "user-42", "a1-b2-c3", strings.Repeat("a", 32)}
	invalid := []string{
		"", "a", "ab", strings.Repeat("a", 33),
		"Alice", "ALICE", // uppercase
		"1abc", "-abc", "abc-", "a--b", // must start with a letter; single inner hyphens only
		"a_b", "a.b", "a b", "a/b", "a\\b", "a:b", "a;b", "a%b", // characters unsafe in names/paths/labels
		"abc\n", " abc", "abc ", "a\x00b", // whitespace and control characters (note: Go's $ does not match before a trailing \n)
		"аlice", "ünï", "用户名", // non-ASCII, including a Cyrillic homoglyph of 'a'
	}
	for _, s := range valid {
		if !ValidUsername(s) || ValidateUsername(s) != nil {
			t.Errorf("ValidUsername(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidUsername(s) || ValidateUsername(s) != ErrInvalidUsername {
			t.Errorf("ValidUsername(%q) = true, want false", s)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"empty", "", false},
		{"7 bytes", "1234567", false},
		{"8 bytes", "12345678", true},
		{"72 bytes", strings.Repeat("a", 72), true},
		{"73 bytes", strings.Repeat("a", 73), false},
		{"spaces allowed", "correct horse battery", true},
		{"25 emoji is 100 bytes", strings.Repeat("🔑", 25), false}, // limit is bytes, matching bcrypt
		{"18 emoji is 72 bytes", strings.Repeat("🔑", 18), true},
	}
	for _, tc := range cases {
		if got := ValidatePassword(tc.pw) == nil; got != tc.ok {
			t.Errorf("%s: ValidatePassword ok=%v, want %v", tc.name, got, tc.ok)
		}
	}
}

func newTestHasher(t *testing.T) *Hasher {
	t.Helper()
	h, err := NewHasher(bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHashAndVerify(t *testing.T) {
	h := newTestHasher(t)
	hash, err := h.Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if hash == "correct horse" || !strings.HasPrefix(hash, "$2") {
		t.Fatalf("hash does not look like bcrypt: %q", hash)
	}
	if !h.Verify(hash, "correct horse") {
		t.Error("correct password rejected")
	}
	if h.Verify(hash, "correct horsf") || h.Verify(hash, "") {
		t.Error("wrong password accepted")
	}
	if h.Verify("not-a-hash", "correct horse") {
		t.Error("malformed hash accepted")
	}
	hash2, _ := h.Hash("correct horse")
	if hash == hash2 {
		t.Error("two hashes of the same password are identical: salt missing")
	}
}

// bcrypt reads only the first 72 bytes. Registration forbids longer passwords,
// so Verify must not let a login attempt with extra trailing bytes through.
func TestVerifyRejectsOverlongPasswordsInsteadOfTruncating(t *testing.T) {
	h := newTestHasher(t)
	pw := strings.Repeat("a", 72)
	hash, err := h.Hash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Verify(hash, pw) {
		t.Fatal("72-byte password rejected")
	}
	if h.Verify(hash, pw+"EXTRA") {
		t.Fatal("password with the right 72-byte prefix and extra bytes was accepted")
	}
	h.VerifyDummy(pw + "EXTRA") // must not panic
}

func TestNewHasherRejectsBadCost(t *testing.T) {
	for _, c := range []int{bcrypt.MinCost - 1, bcrypt.MaxCost + 1, -1} {
		if _, err := NewHasher(c); err == nil {
			t.Errorf("NewHasher(%d) accepted", c)
		}
	}
}

func TestSessionTokens(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		token, hash, err := NewSessionToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(token) != 43 { // 32 bytes, unpadded base64url
			t.Fatalf("token length %d, want 43", len(token))
		}
		if strings.ContainsAny(token, "+/= ") {
			t.Fatalf("token %q is not cookie-safe base64url", token)
		}
		if len(hash) != 64 || hash == token || hash != HashToken(token) {
			t.Fatalf("bad hash %q for token %q", hash, token)
		}
		if seen[token] {
			t.Fatalf("duplicate token after %d draws", i)
		}
		seen[token] = true
	}
	if HashToken("a") == HashToken("b") || HashToken("a") != HashToken("a") {
		t.Fatal("HashToken is not a deterministic injective-looking function")
	}
}
