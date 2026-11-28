package argocd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticNormalizes(t *testing.T) {
	for in, want := range map[string]string{
		"abc":               "abc",
		"  abc\n":           "abc",
		"argocd.token=abc;": "abc",
		"Bearer abc":        "abc",
		`"abc"`:             "abc",
	} {
		got, err := Static(in).Token()
		if err != nil || got != want {
			t.Errorf("Static(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Static("  ").Token(); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty token: want ErrNoToken, got %v", err)
	}
}

func TestFileIsReReadForRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := New("ignored", p)
	if got, _ := src.Token(); got != "first" {
		t.Fatalf("got %q", got)
	}
	if err := os.WriteFile(p, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := src.Token(); got != "second" {
		t.Fatalf("rotation not picked up: %q", got)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty file: %v", err)
	}
}

func TestFileMissing(t *testing.T) {
	if _, err := (File{Path: filepath.Join(t.TempDir(), "nope")}).Token(); err == nil {
		t.Fatal("expected error")
	}
}
