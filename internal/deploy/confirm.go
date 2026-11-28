package deploy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrConfirmation means the confirmation token is unknown, expired, already
// used, or was issued for a different action, application or state.
var ErrConfirmation = errors.New("invalid confirmation")

const maxPending = 1000

// Confirmations issues single-use tokens that bind a developer's approval to
// one action on one application in one exact state (fingerprint). If the
// application changes after the preview, the token no longer matches.
type Confirmations struct {
	mu    sync.Mutex
	ttl   time.Duration
	now   func() time.Time
	items map[string]pendingConfirmation
}

type pendingConfirmation struct {
	action, app, fingerprint string
	expires                  time.Time
}

// NewConfirmations returns a store whose tokens live for ttl.
func NewConfirmations(ttl time.Duration, now func() time.Time) *Confirmations {
	if now == nil {
		now = time.Now
	}

	return &Confirmations{ttl: ttl, now: now, items: map[string]pendingConfirmation{}}
}

// Issue returns a new token for (action, app, fingerprint).
func (c *Confirmations) Issue(action, app, fingerprint string) (string, time.Time, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for k, v := range c.items {
		if now.After(v.expires) {
			delete(c.items, k)
		}
	}
	if len(c.items) >= maxPending {
		return "", time.Time{}, errors.New("too many pending confirmations; try again later")
	}
	exp := now.Add(c.ttl)
	c.items[token] = pendingConfirmation{action: action, app: app, fingerprint: fingerprint, expires: exp}

	return token, exp, nil
}

// Consume validates and invalidates a token (single use, even on mismatch).
func (c *Confirmations) Consume(token, action, app, fingerprint string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.items[token]
	delete(c.items, token)
	switch {
	case !ok:
		return fmt.Errorf("%w: unknown or already used confirmation token; request a new preview", ErrConfirmation)
	case c.now().After(p.expires):
		return fmt.Errorf("%w: confirmation token expired; request a new preview", ErrConfirmation)
	case p.action != action || p.app != app:
		return fmt.Errorf("%w: confirmation token was issued for a different action or application", ErrConfirmation)
	case p.fingerprint != fingerprint:
		return fmt.Errorf("%w: the application changed since the preview; review the new preview and confirm again", ErrConfirmation)
	}

	return nil
}

// Fingerprint hashes the given values (JSON-encoded) into a short hex digest.
func Fingerprint(values ...any) string {
	h := sha256.New()
	for _, v := range values {
		b, _ := json.Marshal(v)
		h.Write(b)
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))[:32]
}
