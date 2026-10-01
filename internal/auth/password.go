// Package auth handles password hashing, password policy, sessions and
// login rate limiting.
package auth

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters. They are encoded into every hash so
// they can be raised later without breaking existing hashes.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams is above the OWASP minimum (19 MiB, t=2, p=1).
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

var (
	ErrInvalidHash         = errors.New("auth: invalid hash format")
	ErrIncompatibleVersion = errors.New("auth: incompatible argon2 version")
)

// HashPassword returns a PHC-formatted argon2id hash.
func HashPassword(password string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches encoded, and whether the
// hash should be upgraded to the current parameters.
func VerifyPassword(password, encoded string, current Params) (ok, needsRehash bool, err error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	other := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	if subtle.ConstantTimeCompare(key, other) != 1 {
		return false, false, nil
	}
	needsRehash = p.Memory < current.Memory || p.Time < current.Time ||
		p.Threads < current.Threads || p.KeyLen < current.KeyLen
	return true, needsRehash, nil
}

func decodeHash(encoded string) (p Params, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, ErrIncompatibleVersion
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	// Refuse absurd parameters so a corrupted row can't be used to exhaust memory.
	if p.Memory == 0 || p.Memory > 1024*1024 || p.Time == 0 || p.Time > 20 || p.Threads == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) < 8 {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 {
		return p, nil, nil, ErrInvalidHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}

const (
	MinPasswordLen = 10
	MaxPasswordLen = 128
)

//go:embed common-passwords.txt
var commonPasswordsTxt []byte

var commonPasswords = func() map[string]struct{} {
	m := make(map[string]struct{}, 10000)
	sc := bufio.NewScanner(bytes.NewReader(commonPasswordsTxt))
	for sc.Scan() {
		if w := strings.TrimSpace(sc.Text()); w != "" {
			m[strings.ToLower(w)] = struct{}{}
		}
	}
	return m
}()

// CheckPasswordPolicy returns a user-facing error if password is unacceptable.
func CheckPasswordPolicy(password, username, email string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < MinPasswordLen:
		return fmt.Errorf("Password must be at least %d characters.", MinPasswordLen)
	case n > MaxPasswordLen:
		return fmt.Errorf("Password must be at most %d characters.", MaxPasswordLen)
	}
	lower := strings.ToLower(password)
	if lower == strings.ToLower(username) || lower == strings.ToLower(email) {
		return errors.New("Password must not be your username or email.")
	}
	if _, ok := commonPasswords[lower]; ok {
		return errors.New("That password is too common. Pick something less guessable.")
	}
	return nil
}
