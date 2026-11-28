// Package argocd provides the Argo CD bearer-token credential.
//
// The token is the user's own Argo CD session token (the value of the
// argocd.token cookie after browser SSO, or a token from `argocd account
// generate-token`). Argo CD applies that user's RBAC to every call. No OAuth
// flow or callback is implemented here.
package argocd

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoToken means no token is configured or the token file is empty.
var ErrNoToken = errors.New("no Argo CD token configured: set ARGOCD_AUTH_TOKEN or ARGOCD_AUTH_TOKEN_FILE")

// TokenSource returns the current bearer token.
type TokenSource interface {
	Token() (string, error)
}

// Static is a token fixed at startup (ARGOCD_AUTH_TOKEN).
type Static string

// Token implements TokenSource.
func (s Static) Token() (string, error) {
	t := normalize(string(s))
	if t == "" {
		return "", ErrNoToken
	}

	return t, nil
}

// File re-reads the token from a file on every call, so rotating the token is
// just overwriting the file.
type File struct{ Path string }

// Token implements TokenSource.
func (f File) Token() (string, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		// Do not include file contents; the path is operator config.
		return "", fmt.Errorf("read ARGOCD_AUTH_TOKEN_FILE: %w", err)
	}
	t := normalize(string(b))
	if t == "" {
		return "", ErrNoToken
	}

	return t, nil
}

// New picks the file source when a path is given, else the static token.
func New(token, file string) TokenSource {
	if file != "" {
		return File{Path: file}
	}

	return Static(token)
}

// normalize trims whitespace and tolerates a pasted "argocd.token=<value>" or
// "Bearer <value>" form.
func normalize(t string) string {
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "argocd.token=")
	t = strings.TrimPrefix(t, "Bearer ")
	t = strings.TrimSuffix(t, ";")

	return strings.Trim(strings.TrimSpace(t), `"`)
}
