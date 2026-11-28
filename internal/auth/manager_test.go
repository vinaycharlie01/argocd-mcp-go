package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, authorize-less token
// endpoint enforcing PKCE, and refresh_token grant.
type fakeIdP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	aud      string

	mu        sync.Mutex
	codes     map[string]codeInfo
	refreshes int
	idTTL     time.Duration
}

type codeInfo struct{ challenge, nonce string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{t: t, key: key, clientID: "cli-client", codes: map[string]codeInfo{}, idTTL: time.Hour}
	f.aud = f.clientID
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"authorization_endpoint":                f.srv.URL + "/authorize",
			"token_endpoint":                        f.srv.URL + "/token",
			"jwks_uri":                              f.srv.URL + "/keys",
			"end_session_endpoint":                  f.srv.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)

	return f
}

// authorize simulates the user signing in at the authorize URL.
func (f *fakeIdP) authorize(authURL string) (state, code string) {
	u, err := url.Parse(authURL)
	if err != nil {
		f.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		f.t.Fatalf("authorize URL lacks PKCE: %s", authURL)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	code = "code-" + q.Get("state")[:6]
	f.codes[code] = codeInfo{challenge: q.Get("code_challenge"), nonce: q.Get("nonce")}

	return q.Get("state"), code
}

func (f *fakeIdP) idToken(nonce string) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now()
	claims := map[string]any{
		"iss": f.srv.URL, "sub": "user-1", "aud": f.aud,
		"iat": now.Unix(), "exp": now.Add(f.idTTL).Unix(),
		"preferred_username": "jane@example.com", "name": "Jane", "groups": []string{"g-devops"},
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	raw, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		f.t.Fatal(err)
	}

	return raw
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	defer f.mu.Unlock()
	var nonce string
	switch r.Form.Get("grant_type") {
	case "authorization_code":
		ci, ok := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		if !ok || s256(r.Form.Get("code_verifier")) != ci.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)

			return
		}
		nonce = ci.nonce
	case "refresh_token":
		if r.Form.Get("refresh_token") != "rt-1" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)

			return
		}
		f.refreshes++
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)

		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
		"refresh_token": "rt-1", "id_token": f.idToken(nonce),
	})
}

func newTestManager(t *testing.T, f *fakeIdP, now func() time.Time) *Manager {
	t.Helper()
	m, err := NewManager(Options{
		Resolver: func(context.Context) (ProviderConfig, error) {
			return ProviderConfig{IssuerURL: f.srv.URL, ClientID: f.clientID, RedirectURL: "http://localhost:8085/auth/callback", Scopes: []string{"profile"}}, nil
		},
		HTTPClient: f.srv.Client(),
		Now:        now,
	})
	if err != nil {
		t.Fatal(err)
	}

	return m
}

func callback(t *testing.T, m *Manager, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	m.CallbackHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?"+q.Encode(), nil))

	return rec
}

func TestLoginFlow(t *testing.T) {
	f := newFakeIdP(t)
	m := newTestManager(t, f, nil)
	ctx := context.Background()

	if _, err := m.BearerToken(ctx, "s1"); err != ErrNotAuthenticated {
		t.Fatalf("want ErrNotAuthenticated, got %v", err)
	}
	a, err := m.StartLogin(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.AuthURL, "offline_access") || !strings.Contains(a.AuthURL, "nonce=") {
		t.Fatalf("auth URL missing offline_access or nonce: %s", a.AuthURL)
	}
	again, _ := m.StartLogin(ctx, "s1")
	if again.AuthURL != a.AuthURL {
		t.Fatal("pending login for same session should be reused")
	}

	state, code := f.authorize(a.AuthURL)
	rec := callback(t, m, url.Values{"state": {state}, "code": {code}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "jane@example.com") {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-a.Done:
	default:
		t.Fatal("attempt not completed")
	}
	id, err := a.Result()
	if err != nil || id.Subject != "user-1" || len(id.Groups) != 1 {
		t.Fatalf("identity %+v err %v", id, err)
	}
	tok, err := m.BearerToken(ctx, "s1")
	if err != nil || strings.Count(tok, ".") != 2 {
		t.Fatalf("expected id_token JWT, got %q err %v", tok, err)
	}
	// Other sessions stay isolated.
	if _, err := m.BearerToken(ctx, "s2"); err != ErrNotAuthenticated {
		t.Fatal("session s2 must not see s1's token")
	}
	// State is single use.
	if rec := callback(t, m, url.Values{"state": {state}, "code": {code}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback should fail, got %d", rec.Code)
	}

	end, was := m.Logout("s1")
	if !was || end != f.srv.URL+"/logout" {
		t.Fatalf("logout: %v %q", was, end)
	}
	if _, err := m.BearerToken(ctx, "s1"); err != ErrNotAuthenticated {
		t.Fatal("token should be gone after logout")
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	f := newFakeIdP(t)
	m := newTestManager(t, f, nil)
	if _, err := m.StartLogin(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	rec := callback(t, m, url.Values{"state": {"forged"}, "code": {"x"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestCallbackRejectsWrongVerifier(t *testing.T) {
	f := newFakeIdP(t)
	m := newTestManager(t, f, nil)
	a, _ := m.StartLogin(context.Background(), "s1")
	state, code := f.authorize(a.AuthURL)
	f.mu.Lock()
	f.codes[code] = codeInfo{challenge: "tampered", nonce: f.codes[code].nonce}
	f.mu.Unlock()
	if rec := callback(t, m, url.Values{"state": {state}, "code": {code}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if _, err := a.Result(); err == nil {
		t.Fatal("expected exchange error")
	}
}

func TestCallbackRejectsWrongAudience(t *testing.T) {
	f := newFakeIdP(t)
	f.aud = "some-other-app"
	m := newTestManager(t, f, nil)
	a, _ := m.StartLogin(context.Background(), "s1")
	state, code := f.authorize(a.AuthURL)
	if rec := callback(t, m, url.Values{"state": {state}, "code": {code}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for foreign audience, got %d", rec.Code)
	}
}

func TestProviderErrorIsReported(t *testing.T) {
	f := newFakeIdP(t)
	m := newTestManager(t, f, nil)
	a, _ := m.StartLogin(context.Background(), "s1")
	state, _ := f.authorize(a.AuthURL)
	rec := callback(t, m, url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"<script>x</script>"}})
	if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "<script>") {
		t.Fatalf("error must be reported and HTML-escaped: %s", rec.Body.String())
	}
}

func TestRefreshOnExpiry(t *testing.T) {
	f := newFakeIdP(t)
	f.idTTL = 2 * time.Minute
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := newTestManager(t, f, clock)
	ctx := context.Background()
	a, _ := m.StartLogin(ctx, "s1")
	state, code := f.authorize(a.AuthURL)
	callback(t, m, url.Values{"state": {state}, "code": {code}})
	if _, err := m.BearerToken(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 0 {
		t.Fatal("no refresh expected while valid")
	}

	mu.Lock()
	now = now.Add(90 * time.Second) // within expiry skew
	mu.Unlock()
	f.idTTL = time.Hour
	if _, err := m.BearerToken(ctx, "s1"); err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if f.refreshes != 1 {
		t.Fatalf("want 1 refresh, got %d", f.refreshes)
	}
}

func TestLoginExpires(t *testing.T) {
	f := newFakeIdP(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	m := newTestManager(t, f, clock)
	a, _ := m.StartLogin(context.Background(), "s1")
	state, code := f.authorize(a.AuthURL)
	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	if rec := callback(t, m, url.Values{"state": {state}, "code": {code}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired login should fail, got %d", rec.Code)
	}
	if _, err := a.Result(); err != ErrLoginExpired {
		t.Fatalf("want ErrLoginExpired, got %v", err)
	}
}
