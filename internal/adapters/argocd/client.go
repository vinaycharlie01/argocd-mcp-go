// Package argocd is the outbound adapter for the Argo CD REST API
// (/api/v1). It is written against the HTTP API directly instead of shelling
// out to the argocd CLI.
package argocd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"
)

const maxErrorBody = 4 << 10

// Client talks to one Argo CD server.
type Client struct {
	base *url.URL
	http *http.Client
}

// NewHTTPClient builds an HTTP client honouring an optional CA bundle and the
// insecure flag (intended only for lower environments with private CAs).
func NewHTTPClient(caFile string, insecure bool, timeout time.Duration) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // path comes from operator config
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	tlsCfg.InsecureSkipVerify = insecure //nolint:gosec // explicit operator opt-in
	tr.TLSClientConfig = tlsCfg

	return &http.Client{Transport: tr, Timeout: timeout}, nil
}

// New returns a client for serverURL.
func New(serverURL string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(serverURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse Argo CD URL: %w", err)
	}

	return &Client{base: u, http: hc}, nil
}

func (c *Client) do(ctx context.Context, bearer, path string, q url.Values, out any) error {
	u := c.base.JoinPath(path)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "argocd-mcp-go/"+version.Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("argo cd request %s: %w", path, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return domain.ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", domain.ErrForbidden, readError(resp.Body))
	case resp.StatusCode >= http.StatusBadRequest:
		return fmt.Errorf("argo cd %s returned %d: %s", path, resp.StatusCode, readError(resp.Body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}

	return nil
}

// readError extracts Argo CD's gRPC-gateway error message.
func readError(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	var e struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Error != "" {
			return e.Error
		}
	}

	return strings.TrimSpace(string(b))
}

// settingsResponse mirrors the subset of /api/v1/settings we need.
type settingsResponse struct {
	URL        string `json:"url"`
	OIDCConfig *struct {
		Issuer      string   `json:"issuer"`
		ClientID    string   `json:"clientID"`
		CLIClientID string   `json:"cliClientID"`
		Scopes      []string `json:"scopes"`
	} `json:"oidcConfig"`
	DexConfig *struct {
		Connectors []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"connectors"`
		DexAuthConnectorID string `json:"dexAuthConnectorID"`
	} `json:"dexConfig"`
}

// SSOSettings reads the public, unauthenticated settings endpoint.
func (c *Client) SSOSettings(ctx context.Context) (*domain.SSOSettings, error) {
	var s settingsResponse
	if err := c.do(ctx, "", "/api/v1/settings", nil, &s); err != nil {
		return nil, err
	}
	out := &domain.SSOSettings{URL: strings.TrimRight(s.URL, "/")}
	if out.URL == "" {
		out.URL = c.base.String()
	}
	if s.OIDCConfig != nil {
		out.OIDCIssuer = s.OIDCConfig.Issuer
		out.OIDCClientID = s.OIDCConfig.ClientID
		out.OIDCCLIClientID = s.OIDCConfig.CLIClientID
		out.OIDCScopes = s.OIDCConfig.Scopes
	}
	if s.DexConfig != nil {
		for _, cn := range s.DexConfig.Connectors {
			out.DexConnectors = append(out.DexConnectors, cn.ID)
		}
		out.DexAuthConnectorID = s.DexConfig.DexAuthConnectorID
	}

	return out, nil
}

type applicationList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Project     string       `json:"project"`
			Source      *sourceJSON  `json:"source"`
			Sources     []sourceJSON `json:"sources"`
			Destination struct {
				Server    string `json:"server"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"destination"`
			SyncPolicy *struct {
				Automated *json.RawMessage `json:"automated"`
			} `json:"syncPolicy"`
		} `json:"spec"`
		Status struct {
			Sync struct {
				Status   string `json:"status"`
				Revision string `json:"revision"`
			} `json:"sync"`
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			OperationState *struct {
				Phase string `json:"phase"`
			} `json:"operationState"`
		} `json:"status"`
	} `json:"items"`
}

type sourceJSON struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path"`
	Chart          string `json:"chart"`
	TargetRevision string `json:"targetRevision"`
}

// ListApplications lists applications visible to the bearer token's user.
func (c *Client) ListApplications(ctx context.Context, bearer string, f domain.ApplicationFilter) ([]domain.Application, error) {
	if bearer == "" {
		return nil, errors.New("missing user token")
	}
	q := url.Values{}
	for _, p := range f.Projects {
		if p = strings.TrimSpace(p); p != "" {
			q.Add("projects", p)
		}
	}
	if f.Selector != "" {
		q.Set("selector", f.Selector)
	}
	if f.AppNamespace != "" {
		q.Set("appNamespace", f.AppNamespace)
	}

	var list applicationList
	if err := c.do(ctx, bearer, "/api/v1/applications", q, &list); err != nil {
		return nil, err
	}

	apps := make([]domain.Application, 0, len(list.Items))
	for _, it := range list.Items {
		a := domain.Application{
			Name:          it.Metadata.Name,
			Namespace:     it.Metadata.Namespace,
			Project:       it.Spec.Project,
			DestServer:    it.Spec.Destination.Server,
			DestName:      it.Spec.Destination.Name,
			DestNamespace: it.Spec.Destination.Namespace,
			SyncStatus:    it.Status.Sync.Status,
			HealthStatus:  it.Status.Health.Status,
			Revision:      it.Status.Sync.Revision,
			AutoSync:      it.Spec.SyncPolicy != nil && it.Spec.SyncPolicy.Automated != nil,
		}
		src := it.Spec.Source
		if src == nil && len(it.Spec.Sources) > 0 {
			src = &it.Spec.Sources[0]
		}
		if src != nil {
			a.RepoURL, a.Path, a.Chart, a.TargetRevision = src.RepoURL, src.Path, src.Chart, src.TargetRevision
		}
		if it.Status.OperationState != nil {
			a.OperationPhase = it.Status.OperationState.Phase
		}
		apps = append(apps, a)
	}

	return apps, nil
}
