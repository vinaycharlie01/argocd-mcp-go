// Package auth implements per-user OpenID Connect login (Authorization Code
// flow with PKCE) against Microsoft Entra ID, either directly or through a
// federating broker such as the Dex instance bundled with Argo CD.
//
// The package never holds a shared credential: every MCP session logs in on
// its own, and only that user's tokens are ever presented to the downstream
// platform, so the platform's own RBAC decides what the user may do.
package auth

import (
	"context"
	"errors"
	"time"
)

// LocalSessionKey is the session key used by the stdio transport, where the
// process serves exactly one user.
const LocalSessionKey = "local"

var (
	// ErrNotAuthenticated is returned when a session has no valid login.
	ErrNotAuthenticated = errors.New("not authenticated: run the login tool first")
	// ErrLoginExpired is returned when a callback arrives for a login attempt
	// that has timed out or was already used.
	ErrLoginExpired = errors.New("login attempt expired or already completed; start a new login")
)

// TokenKind selects which token obtained from the identity provider is sent to
// the downstream platform as the Bearer credential.
type TokenKind string

const (
	// TokenKindIDToken sends the OIDC ID token. This is what Argo CD expects:
	// it validates the token's issuer and audience and reads the groups claim.
	TokenKindIDToken TokenKind = "id_token"
	// TokenKindAccessToken sends the OAuth2 access token.
	TokenKindAccessToken TokenKind = "access_token"
)

// ProviderConfig describes the OIDC client used for interactive login.
type ProviderConfig struct {
	// IssuerURL is the OIDC issuer, e.g.
	// https://login.microsoftonline.com/<tenant-id>/v2.0 or
	// https://argocd.example.com/api/dex.
	IssuerURL string
	// ClientID is the OAuth2 client (Entra application) ID.
	ClientID string
	// ClientSecret is optional. Leave it empty for public clients, which rely
	// on PKCE alone.
	ClientSecret string
	// Scopes requested at the authorize endpoint. "openid" is always added.
	Scopes []string
	// RedirectURL is the OAuth2 callback URL registered on the client. It is
	// NOT the platform's logout URL.
	RedirectURL string
	// AuthParams are extra authorize-request parameters, e.g. Dex's
	// connector_id or Entra's domain_hint.
	AuthParams map[string]string
	// ExtraAudiences are additional ID token audiences to accept besides
	// ClientID.
	ExtraAudiences []string
}

// ConfigResolver resolves the provider configuration. It is called lazily on
// the first login so the server can start while the platform is unreachable.
type ConfigResolver func(ctx context.Context) (ProviderConfig, error)

// Identity is the authenticated user as described by the ID token claims.
type Identity struct {
	Subject           string    `json:"subject"`
	Issuer            string    `json:"issuer"`
	Email             string    `json:"email,omitempty"`
	Name              string    `json:"name,omitempty"`
	PreferredUsername string    `json:"preferredUsername,omitempty"`
	Groups            []string  `json:"groups,omitempty"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

// DisplayName returns the most human-friendly identifier available.
func (i Identity) DisplayName() string {
	for _, v := range []string{i.PreferredUsername, i.Email, i.Name} {
		if v != "" {
			return v
		}
	}

	return i.Subject
}

// LoginAttempt is an in-flight browser login.
type LoginAttempt struct {
	// AuthURL is the URL the user must open in a browser.
	AuthURL string
	// Done is closed when the callback completes (successfully or not).
	Done <-chan struct{}

	p *pendingLogin
}

// Result returns the outcome once Done is closed.
func (a *LoginAttempt) Result() (*Identity, error) {
	return a.p.identity, a.p.err
}
