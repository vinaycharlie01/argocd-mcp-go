package deploy

import (
	"errors"
	"testing"
	"time"
)

func TestConfirmations(t *testing.T) {
	now := time.Now()
	c := NewConfirmations(time.Minute, func() time.Time { return now })
	tok, _, err := c.Issue("sync", "/web", "fp1")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Consume(tok, "sync", "/web", "fp1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Consume(tok, "sync", "/web", "fp1"); !errors.Is(err, ErrConfirmation) {
		t.Fatal("token must be single use")
	}

	for name, f := range map[string]func(string) error{
		"wrong action":  func(tk string) error { return c.Consume(tk, "update", "/web", "fp1") },
		"wrong app":     func(tk string) error { return c.Consume(tk, "sync", "/api", "fp1") },
		"state changed": func(tk string) error { return c.Consume(tk, "sync", "/web", "fp2") },
		"unknown":       func(string) error { return c.Consume("nope", "sync", "/web", "fp1") },
	} {
		tk, _, _ := c.Issue("sync", "/web", "fp1")
		if err := f(tk); !errors.Is(err, ErrConfirmation) {
			t.Errorf("%s: %v", name, err)
		}
	}

	tk, _, _ := c.Issue("sync", "/web", "fp1")
	now = now.Add(2 * time.Minute)
	if err := c.Consume(tk, "sync", "/web", "fp1"); !errors.Is(err, ErrConfirmation) {
		t.Fatal("expired token accepted")
	}
	if Fingerprint("a", 1) == Fingerprint("a", 2) || Fingerprint("a", 1) != Fingerprint("a", 1) {
		t.Fatal("fingerprint")
	}
}
