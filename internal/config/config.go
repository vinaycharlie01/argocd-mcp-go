// Package config loads server configuration from environment variables.
// Secrets (an optional OIDC client secret) are only ever read from the
// environment and are never logged.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Transport names.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// DefaultLocalRedirectURL matches the loopback redirect URI the Argo CD CLI
// uses, which is pre-registered on Dex's argo-cd-cli client and is the usual
// "Mobile and desktop" redirect on the Entra app used as cliClientID.
const DefaultLocalRedirectURL = "http://localhost:8085/auth/callback"

// Config is the full server configuration.
type Config struct {
	ArgoCD  ArgoCDConfig
	OIDC    OIDCConfig
	MCP     MCPConfig
	Logging LoggingConfig
}

// ArgoCDConfig describes how to reach the Argo CD API server.
type ArgoCDConfig struct {
	ServerURL          string        // ARGOCD_SERVER_URL (required)
	CAFile             string        // ARGOCD_CA_FILE
	InsecureSkipVerify bool          // ARGOCD_INSECURE_SKIP_VERIFY
	RequestTimeout     time.Duration // ARGOCD_REQUEST_TIMEOUT
}

// OIDCConfig holds optional overrides. When IssuerURL/ClientID are empty they
// are discovered from Argo CD's unauthenticated /api/v1/settings endpoint.
type OIDCConfig struct {
	IssuerURL    string            // OIDC_ISSUER_URL
	ClientID     string            // OIDC_CLIENT_ID
	ClientSecret string            // OIDC_CLIENT_SECRET (optional, confidential clients only)
	Scopes       []string          // OIDC_SCOPES (comma or space separated)
	RedirectURL  string            // OIDC_REDIRECT_URL
	AuthParams   map[string]string // OIDC_DOMAIN_HINT -> domain_hint
	LoginTimeout time.Duration     // OIDC_LOGIN_TIMEOUT: how long a started login stays valid
	LoginWait    time.Duration     // OIDC_LOGIN_WAIT: how long the login tool blocks waiting
	OpenBrowser  bool              // OIDC_OPEN_BROWSER (stdio only)
}

// MCPConfig selects the transport.
type MCPConfig struct {
	Transport    string // MCP_TRANSPORT: stdio | http
	HTTPAddr     string // MCP_HTTP_ADDR, e.g. :8080
	PublicURL    string // MCP_PUBLIC_URL, external base URL in http mode
	JSONResponse bool   // MCP_JSON_RESPONSE
}

// LoggingConfig controls structured logging.
type LoggingConfig struct {
	Level  string // LOG_LEVEL
	Format string // LOG_FORMAT: json | text
}

// Load reads configuration from the environment.
func Load() (*Config, error) {
	return LoadFrom(os.Getenv)
}

// LoadFrom reads configuration using getenv (injectable for tests).
func LoadFrom(getenv func(string) string) (*Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}

		return def
	}
	var errs []error
	boolean := func(key string, def bool) bool {
		v := get(key, "")
		if v == "" {
			return def
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}

		return b
	}
	duration := func(key string, def time.Duration) time.Duration {
		v := get(key, "")
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
		}

		return d
	}

	c := &Config{
		ArgoCD: ArgoCDConfig{
			ServerURL:          strings.TrimRight(get("ARGOCD_SERVER_URL", ""), "/"),
			CAFile:             get("ARGOCD_CA_FILE", ""),
			InsecureSkipVerify: boolean("ARGOCD_INSECURE_SKIP_VERIFY", false),
			RequestTimeout:     duration("ARGOCD_REQUEST_TIMEOUT", 30*time.Second),
		},
		OIDC: OIDCConfig{
			IssuerURL:    get("OIDC_ISSUER_URL", ""),
			ClientID:     get("OIDC_CLIENT_ID", ""),
			ClientSecret: getenv("OIDC_CLIENT_SECRET"),
			Scopes:       splitList(get("OIDC_SCOPES", "")),
			RedirectURL:  get("OIDC_REDIRECT_URL", ""),
			AuthParams:   map[string]string{"domain_hint": get("OIDC_DOMAIN_HINT", "")},
			LoginTimeout: duration("OIDC_LOGIN_TIMEOUT", 10*time.Minute),
			LoginWait:    duration("OIDC_LOGIN_WAIT", 2*time.Minute),
			OpenBrowser:  boolean("OIDC_OPEN_BROWSER", true),
		},
		MCP: MCPConfig{
			Transport:    strings.ToLower(get("MCP_TRANSPORT", TransportStdio)),
			HTTPAddr:     get("MCP_HTTP_ADDR", ":8080"),
			PublicURL:    strings.TrimRight(get("MCP_PUBLIC_URL", ""), "/"),
			JSONResponse: boolean("MCP_JSON_RESPONSE", false),
		},
		Logging: LoggingConfig{
			Level:  get("LOG_LEVEL", "info"),
			Format: get("LOG_FORMAT", "json"),
		},
	}

	if c.OIDC.RedirectURL == "" {
		if c.MCP.Transport == TransportHTTP && c.MCP.PublicURL != "" {
			c.OIDC.RedirectURL = c.MCP.PublicURL + "/auth/callback"
		} else {
			c.OIDC.RedirectURL = DefaultLocalRedirectURL
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return c, c.Validate()
}

// Validate checks required settings and their consistency.
func (c *Config) Validate() error {
	var errs []error
	if c.ArgoCD.ServerURL == "" {
		errs = append(errs, errors.New("ARGOCD_SERVER_URL is required"))
	} else if err := requireURL(c.ArgoCD.ServerURL, false); err != nil {
		errs = append(errs, fmt.Errorf("ARGOCD_SERVER_URL: %w", err))
	}
	if err := requireURL(c.OIDC.RedirectURL, true); err != nil {
		errs = append(errs, fmt.Errorf("OIDC_REDIRECT_URL: %w", err))
	}
	if c.OIDC.IssuerURL != "" && c.OIDC.ClientID == "" {
		errs = append(errs, errors.New("OIDC_CLIENT_ID is required when OIDC_ISSUER_URL is set"))
	}

	switch c.MCP.Transport {
	case TransportStdio:
	case TransportHTTP:
		if c.MCP.PublicURL == "" {
			errs = append(errs, errors.New("MCP_PUBLIC_URL is required when MCP_TRANSPORT=http (used to build the OAuth callback URL)"))
		} else if err := requireURL(c.MCP.PublicURL, true); err != nil {
			errs = append(errs, fmt.Errorf("MCP_PUBLIC_URL: %w", err))
		}
	default:
		errs = append(errs, fmt.Errorf("MCP_TRANSPORT must be %q or %q, got %q", TransportStdio, TransportHTTP, c.MCP.Transport))
	}

	return errors.Join(errs...)
}

// requireURL checks u is absolute. Plain http is only allowed for loopback
// hosts when allowLoopbackHTTP is set, so tokens never cross the network
// unencrypted.
func requireURL(raw string, allowLoopbackHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid absolute URL %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if allowLoopbackHTTP && (h == "localhost" || h == "127.0.0.1" || h == "::1") {
			return nil
		}

		return fmt.Errorf("%q must use https (http is only allowed for localhost callbacks)", raw)
	default:
		return fmt.Errorf("unsupported scheme in %q", raw)
	}
}

func splitList(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	if len(f) == 0 {
		return nil
	}

	return f
}
