package auth

import (
	"strings"
	"testing"
)

var testParams = Params{Memory: 8 * 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Fatalf("unexpected encoding %q", h)
	}
	ok, rehash, err := VerifyPassword("correct horse battery", h, testParams)
	if err != nil || !ok || rehash {
		t.Fatalf("verify = %v, %v, %v", ok, rehash, err)
	}
	ok, _, err = VerifyPassword("wrong horse battery", h, testParams)
	if err != nil || ok {
		t.Fatalf("wrong password verified: %v, %v", ok, err)
	}
	// Stronger current params should request a rehash.
	_, rehash, _ = VerifyPassword("correct horse battery", h, DefaultParams)
	if !rehash {
		t.Error("expected needsRehash with stronger params")
	}
	// Two hashes of the same password use different salts.
	h2, _ := HashPassword("correct horse battery", testParams)
	if h == h2 {
		t.Error("hashes should differ due to salt")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	good, _ := HashPassword("correct horse battery", testParams)
	parts := strings.Split(good, "$")
	bad := []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=18$" + strings.Join(parts[3:], "$"),
		"$argon2id$v=19$m=99999999,t=1,p=1$" + strings.Join(parts[4:], "$"),
		"$argon2id$v=19$m=8192,t=1,p=1$!!!$" + parts[5],
	}
	for _, b := range bad {
		if ok, _, err := VerifyPassword("x", b, testParams); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = %v, %v; want error", b, ok, err)
		}
	}
	// A tampered key must fail verification.
	tampered := strings.Join(append(parts[:5:5], "AAAA"+parts[5][4:]), "$")
	if ok, _, _ := VerifyPassword("correct horse battery", tampered, testParams); ok {
		t.Error("tampered hash verified")
	}
}

func TestPasswordPolicy(t *testing.T) {
	cases := []struct {
		pw   string
		okay bool
	}{
		{"short", false},
		{strings.Repeat("a", 129), false},
		{"1234567890", false},   // common
		{"QWERTYUIOP", false},   // common, case-insensitive
		{"alice_wonder", false}, // equals username
		{"vinyl crackle at dawn", true},
	}
	for _, c := range cases {
		err := CheckPasswordPolicy(c.pw, "alice_wonder", "alice@example.com")
		if (err == nil) != c.okay {
			t.Errorf("CheckPasswordPolicy(%q) err=%v, want ok=%v", c.pw, err, c.okay)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(1e9*60, 3)
	for i := range 3 {
		if !l.Allow("ip") {
			t.Fatalf("attempt %d blocked", i)
		}
	}
	if l.Allow("ip") {
		t.Error("4th attempt allowed")
	}
	if !l.Allow("other") {
		t.Error("separate key should have its own bucket")
	}
}
