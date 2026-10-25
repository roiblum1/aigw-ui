package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Sealer signs small values the browser keeps in cookies: the session of a
// person who signed in, and the state of a sign-in on its way. They are
// signed, not encrypted: they hold a user name and a random state, nothing
// secret, and nothing the browser may change.
type Sealer struct{ key []byte }

// NewSealer derives its key from the hub's encryption key, so sessions
// last across restarts and end when that key changes.
func NewSealer(secret []byte) *Sealer {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("aigw-ui sso cookies"))
	return &Sealer{key: m.Sum(nil)}
}

type sealed struct {
	Value   json.RawMessage `json:"v"`
	Purpose string          `json:"p"`
	Expires int64           `json:"e"`
}

// Seal signs v for one purpose ("session", "state"), valid until expires.
func (s *Sealer) Seal(purpose string, v any, expires time.Time) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(sealed{Value: raw, Purpose: purpose, Expires: expires.Unix()})
	if err != nil {
		return "", err
	}
	b := base64.RawURLEncoding.EncodeToString(body)
	return b + "." + s.sign(b), nil
}

// Open checks a value Seal made for the purpose and reads it into v. False
// for one changed, for another purpose, or expired.
func (s *Sealer) Open(purpose, value string, v any, now time.Time) bool {
	b, sig, ok := strings.Cut(value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.sign(b))) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return false
	}
	var x sealed
	if json.Unmarshal(body, &x) != nil || x.Purpose != purpose || now.Unix() >= x.Expires {
		return false
	}
	return json.Unmarshal(x.Value, v) == nil
}

func (s *Sealer) sign(b string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(b))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
