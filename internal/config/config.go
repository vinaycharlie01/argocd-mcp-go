// Package config loads server configuration from environment variables.
// Each integration is optional and is enabled when its server URL is set.
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

// Config is the full server configuration.
type Config struct {
	ArgoCD  *ArgoCD // nil when ARGOCD_SERVER is not set
	Logging Logging
}

// ArgoCD configures the Argo CD integration.
type ArgoCD struct {
	// BaseURL is the normalised https URL derived from ARGOCD_SERVER.
	BaseURL string
	// Token is ARGOCD_AUTH_TOKEN. Never log it.
	Token string
	// TokenFile is ARGOCD_AUTH_TOKEN_FILE; re-read on every request so the
	// token can be rotated without a restart.
	TokenFile          string
	CAFile             string        // ARGOCD_CA_FILE
	InsecureSkipVerify bool          // ARGOCD_INSECURE
	Timeout            time.Duration // ARGOCD_REQUEST_TIMEOUT
}

// Logging configures structured logging.
type Logging struct {
	Level  string // LOG_LEVEL
	Format string // LOG_FORMAT: json | text
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) { return LoadFrom(os.Getenv) }

// LoadFrom reads configuration through getenv (injectable for tests).
func LoadFrom(getenv func(string) string) (*Config, error) {
	r := reader{getenv: getenv}
	cfg := &Config{
		Logging: Logging{Level: r.str("LOG_LEVEL", "info"), Format: r.str("LOG_FORMAT", "json")},
	}

	if server := r.str("ARGOCD_SERVER", ""); server != "" {
		base, err := NormalizeServerURL(server)
		if err != nil {
			r.errs = append(r.errs, fmt.Errorf("ARGOCD_SERVER: %w", err))
		}
		cfg.ArgoCD = &ArgoCD{
			BaseURL:            base,
			Token:              strings.TrimSpace(getenv("ARGOCD_AUTH_TOKEN")),
			TokenFile:          r.str("ARGOCD_AUTH_TOKEN_FILE", ""),
			CAFile:             r.str("ARGOCD_CA_FILE", ""),
			InsecureSkipVerify: r.boolean("ARGOCD_INSECURE", false),
			Timeout:            r.duration("ARGOCD_REQUEST_TIMEOUT", 30*time.Second),
		}
		if cfg.ArgoCD.Token == "" && cfg.ArgoCD.TokenFile == "" {
			r.errs = append(r.errs, errors.New("ARGOCD_AUTH_TOKEN or ARGOCD_AUTH_TOKEN_FILE is required when ARGOCD_SERVER is set"))
		}
		if cfg.ArgoCD.Timeout <= 0 {
			r.errs = append(r.errs, errors.New("ARGOCD_REQUEST_TIMEOUT must be positive"))
		}
	}

	if cfg.ArgoCD == nil {
		r.errs = append(r.errs, errors.New("no integration configured: set ARGOCD_SERVER"))
	}

	return cfg, errors.Join(r.errs...)
}

// NormalizeServerURL accepts what the argocd CLI accepts for --server
// ("host", "host:port") as well as a full URL, and returns an https base URL.
// Plain http is refused so tokens never travel unencrypted.
func NormalizeServerURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server %q", s)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("server %q must use https", s)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("server %q must not contain credentials, query or fragment", s)
	}

	return strings.TrimRight(u.String(), "/"), nil
}

type reader struct {
	getenv func(string) string
	errs   []error
}

func (r *reader) str(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}

	return def
}

func (r *reader) boolean(key string, def bool) bool {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
	}

	return b
}

func (r *reader) duration(key string, def time.Duration) time.Duration {
	v := r.str(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
	}

	return d
}
