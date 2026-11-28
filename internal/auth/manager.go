package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	defaultLoginTTL = 10 * time.Minute
	// expirySkew renews tokens slightly before they expire so a request does
	// not race the expiry.
	expirySkew = 60 * time.Second
)

// Options configures a Manager.
type Options struct {
	// Resolver returns the provider configuration (required).
	Resolver ConfigResolver
	// HTTPClient is used for discovery, JWKS and token requests.
	HTTPClient *http.Client
	// Present selects the token sent downstream (default: ID token).
	Present TokenKind
	// LoginTTL bounds how long a started login stays valid (default 10m).
	LoginTTL time.Duration
	Logger   *slog.Logger
	// Now is a clock override for tests.
	Now func() time.Time
}

// Manager runs logins and keeps each session's tokens in memory. Tokens are
// never written to disk or logs.
type Manager struct {
	opts Options
	log  *slog.Logger

	initMu sync.Mutex
	client *oidcClient

	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]*pendingLogin // keyed by OAuth2 state
}

type oidcClient struct {
	cfg        ProviderConfig
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	audiences  []string
	endSession string
}

type session struct {
	mu       sync.Mutex
	token    *oauth2.Token
	idToken  string
	identity Identity
}

type pendingLogin struct {
	sessionKey string
	verifier   string
	nonce      string
	authURL    string
	expires    time.Time
	done       chan struct{}
	identity   *Identity
	err        error
}

// NewManager returns a Manager. It performs no network I/O.
func NewManager(opts Options) (*Manager, error) {
	if opts.Resolver == nil {
		return nil, errors.New("auth: resolver is required")
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.Present == "" {
		opts.Present = TokenKindIDToken
	}
	if opts.LoginTTL <= 0 {
		opts.LoginTTL = defaultLoginTTL
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Manager{
		opts:     opts,
		log:      opts.Logger.With("component", "auth"),
		sessions: map[string]*session{},
		pending:  map[string]*pendingLogin{},
	}, nil
}

func (m *Manager) ctx(ctx context.Context) context.Context {
	ctx = oidc.ClientContext(ctx, m.opts.HTTPClient)

	return context.WithValue(ctx, oauth2.HTTPClient, m.opts.HTTPClient)
}

// getClient resolves and caches the provider configuration on first use.
func (m *Manager) getClient(ctx context.Context) (*oidcClient, error) {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	if m.client != nil {
		return m.client, nil
	}

	cfg, err := m.opts.Resolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve OIDC configuration: %w", err)
	}
	if cfg.IssuerURL == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("OIDC configuration incomplete: issuer, client ID and redirect URL are required")
	}

	provider, err := oidc.NewProvider(m.ctx(ctx), cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer %q: %w", cfg.IssuerURL, err)
	}

	var meta struct {
		EndSession      string   `json:"end_session_endpoint"`
		ScopesSupported []string `json:"scopes_supported"`
	}
	_ = provider.Claims(&meta)

	scopes := []string{oidc.ScopeOpenID}
	for _, s := range cfg.Scopes {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	// Ask for a refresh token when the provider supports it, as the Argo CD CLI does.
	if (len(meta.ScopesSupported) == 0 || slices.Contains(meta.ScopesSupported, oidc.ScopeOfflineAccess)) &&
		!slices.Contains(scopes, oidc.ScopeOfflineAccess) {
		scopes = append(scopes, oidc.ScopeOfflineAccess)
	}

	audiences := append([]string{cfg.ClientID}, cfg.ExtraAudiences...)
	m.client = &oidcClient{
		cfg: cfg,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		// Audience is checked manually so several audiences can be accepted.
		verifier:   provider.Verifier(&oidc.Config{SkipClientIDCheck: true, Now: m.opts.Now}),
		audiences:  audiences,
		endSession: meta.EndSession,
	}
	m.log.Info("OIDC provider configured", "issuer", cfg.IssuerURL, "client_id", cfg.ClientID,
		"redirect_url", cfg.RedirectURL, "scopes", scopes)

	return m.client, nil
}

// RedirectURL returns the configured callback URL (resolving the provider
// configuration if needed).
func (m *Manager) RedirectURL(ctx context.Context) (string, error) {
	c, err := m.getClient(ctx)
	if err != nil {
		return "", err
	}

	return c.cfg.RedirectURL, nil
}

// StartLogin begins a browser login for sessionKey. A still-valid attempt for
// the same session is reused so repeated calls do not mint new states.
func (m *Manager) StartLogin(ctx context.Context, sessionKey string) (*LoginAttempt, error) {
	c, err := m.getClient(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	for _, p := range m.pending {
		if p.sessionKey == sessionKey {
			return &LoginAttempt{AuthURL: p.authURL, Done: p.done, p: p}, nil
		}
	}

	state, err := randomString()
	if err != nil {
		return nil, err
	}
	nonce, err := randomString()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()

	opts := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
	}
	for k, v := range c.cfg.AuthParams {
		if v != "" {
			opts = append(opts, oauth2.SetAuthURLParam(k, v))
		}
	}

	p := &pendingLogin{
		sessionKey: sessionKey,
		verifier:   verifier,
		nonce:      nonce,
		authURL:    c.oauth.AuthCodeURL(state, opts...),
		expires:    m.opts.Now().Add(m.opts.LoginTTL),
		done:       make(chan struct{}),
	}
	m.pending[state] = p
	m.log.Info("login started", "session", redactKey(sessionKey))

	return &LoginAttempt{AuthURL: p.authURL, Done: p.done, p: p}, nil
}

// gcLocked drops expired pending logins. Caller holds m.mu.
func (m *Manager) gcLocked() {
	now := m.opts.Now()
	for state, p := range m.pending {
		if now.After(p.expires) {
			p.err = ErrLoginExpired
			close(p.done)
			delete(m.pending, state)
		}
	}
}

// takePending removes and returns the pending login for state (single use).
func (m *Manager) takePending(state string) (*pendingLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcLocked()
	p, ok := m.pending[state]
	if !ok {
		return nil, ErrLoginExpired
	}
	delete(m.pending, state)

	return p, nil
}

// complete finishes an authorization-code callback.
func (m *Manager) complete(ctx context.Context, state, code, errCode, errDesc string) (*Identity, error) {
	if state == "" {
		return nil, errors.New("missing state parameter")
	}
	p, err := m.takePending(state)
	if err != nil {
		return nil, err
	}

	identity, err := m.exchange(ctx, p, code, errCode, errDesc)
	p.identity, p.err = identity, err
	close(p.done)

	return identity, err
}

func (m *Manager) exchange(ctx context.Context, p *pendingLogin, code, errCode, errDesc string) (*Identity, error) {
	if errCode != "" {
		return nil, fmt.Errorf("identity provider returned %s: %s", errCode, errDesc)
	}
	if code == "" {
		return nil, errors.New("missing authorization code")
	}

	c, err := m.getClient(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := c.oauth.Exchange(m.ctx(ctx), code, oauth2.VerifierOption(p.verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}

	s := &session{}
	identity, err := m.applyToken(ctx, c, s, tok, p.nonce)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[p.sessionKey] = s
	m.mu.Unlock()
	m.log.Info("login succeeded", "session", redactKey(p.sessionKey), "user", identity.DisplayName(), "groups", len(identity.Groups))

	return identity, nil
}

// applyToken verifies the ID token inside tok and stores it on s.
func (m *Manager) applyToken(ctx context.Context, c *oidcClient, s *session, tok *oauth2.Token, nonce string) (*Identity, error) {
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return nil, errors.New("token response did not include an id_token")
	}
	idt, err := c.verifier.Verify(m.ctx(ctx), raw)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}
	if !audienceAllowed(idt.Audience, c.audiences) {
		return nil, fmt.Errorf("id_token audience %v not in allowed audiences %v", idt.Audience, c.audiences)
	}
	if nonce != "" && idt.Nonce != nonce {
		return nil, errors.New("id_token nonce mismatch")
	}

	var claims struct {
		Email             string   `json:"email"`
		Name              string   `json:"name"`
		PreferredUsername string   `json:"preferred_username"`
		UPN               string   `json:"upn"`
		Groups            []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode id_token claims: %w", err)
	}
	if claims.PreferredUsername == "" {
		claims.PreferredUsername = claims.UPN
	}

	s.token = tok
	s.idToken = raw
	s.identity = Identity{
		Subject:           idt.Subject,
		Issuer:            idt.Issuer,
		Email:             claims.Email,
		Name:              claims.Name,
		PreferredUsername: claims.PreferredUsername,
		Groups:            claims.Groups,
		ExpiresAt:         idt.Expiry,
	}
	id := s.identity

	return &id, nil
}

func audienceAllowed(got, allowed []string) bool {
	for _, a := range got {
		if slices.Contains(allowed, a) {
			return true
		}
	}

	return false
}

// BearerToken returns a valid token for sessionKey, refreshing it if needed.
func (m *Manager) BearerToken(ctx context.Context, sessionKey string) (string, error) {
	s := m.lookup(sessionKey)
	if s == nil {
		return "", ErrNotAuthenticated
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := m.current(s); ok {
		return tok, nil
	}

	if err := m.refresh(ctx, s); err != nil {
		m.log.Info("token refresh failed; user must log in again", "session", redactKey(sessionKey), "error", err)
		m.forget(sessionKey, s)

		return "", ErrNotAuthenticated
	}
	tok, ok := m.current(s)
	if !ok {
		m.forget(sessionKey, s)

		return "", ErrNotAuthenticated
	}

	return tok, nil
}

// current returns the presented token when it is still valid. Caller holds s.mu.
func (m *Manager) current(s *session) (string, bool) {
	deadline := m.opts.Now().Add(expirySkew)
	switch m.opts.Present {
	case TokenKindAccessToken:
		if s.token.AccessToken != "" && (s.token.Expiry.IsZero() || s.token.Expiry.After(deadline)) {
			return s.token.AccessToken, true
		}
	case TokenKindIDToken:
		if s.idToken != "" && s.identity.ExpiresAt.After(deadline) {
			return s.idToken, true
		}
	}

	return "", false
}

// refresh redeems the refresh token. Caller holds s.mu.
func (m *Manager) refresh(ctx context.Context, s *session) error {
	if s.token == nil || s.token.RefreshToken == "" {
		return errors.New("no refresh token")
	}
	c, err := m.getClient(ctx)
	if err != nil {
		return err
	}
	expired := &oauth2.Token{RefreshToken: s.token.RefreshToken, Expiry: time.Unix(1, 0)}
	tok, err := c.oauth.TokenSource(m.ctx(ctx), expired).Token()
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = s.token.RefreshToken
	}
	_, err = m.applyToken(ctx, c, s, tok, "")

	return err
}

func (m *Manager) lookup(key string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.sessions[key]
}

func (m *Manager) forget(key string, s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[key] == s {
		delete(m.sessions, key)
	}
}

// Identity returns the logged-in identity for sessionKey, if any.
func (m *Manager) Identity(sessionKey string) (*Identity, bool) {
	s := m.lookup(sessionKey)
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.identity

	return &id, true
}

// Logout drops the session's tokens from memory. It returns the identity
// provider's end-session endpoint, if it advertises one, so the user can
// optionally end the browser SSO session too. That endpoint is unrelated to
// the OAuth2 callback URL.
func (m *Manager) Logout(sessionKey string) (endSessionURL string, wasLoggedIn bool) {
	m.mu.Lock()
	_, wasLoggedIn = m.sessions[sessionKey]
	delete(m.sessions, sessionKey)
	for state, p := range m.pending {
		if p.sessionKey == sessionKey {
			p.err = ErrLoginExpired
			close(p.done)
			delete(m.pending, state)
		}
	}
	m.mu.Unlock()

	m.initMu.Lock()
	if m.client != nil {
		endSessionURL = m.client.endSession
	}
	m.initMu.Unlock()
	if wasLoggedIn {
		m.log.Info("logged out", "session", redactKey(sessionKey))
	}

	return endSessionURL, wasLoggedIn
}

// ForgetSession removes all state for a closed MCP session.
func (m *Manager) ForgetSession(sessionKey string) { m.Logout(sessionKey) }

func randomString() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(b), nil
}

// redactKey shortens session IDs in logs; they act as bearer handles in HTTP mode.
func redactKey(key string) string {
	if len(key) <= 8 {
		return key
	}

	return key[:4] + "…" + key[len(key)-4:]
}
