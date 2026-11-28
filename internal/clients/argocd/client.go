// Package argocd is a read-only client for the Argo CD REST API (/api/v1).
//
// It uses the same REST/JSON API as the Argo CD web UI, over plain HTTPS. The
// argocd CLI needs --grpc-web because it speaks gRPC; this client does not, so
// it works through any ingress that serves the UI. No Kubernetes access is needed.
package argocd

import (
	"bytes"
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

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"
)

const (
	maxResponseBytes = 64 << 20 // guards against unbounded responses
	maxErrorBytes    = 4 << 10
)

// Sentinel errors; match with errors.Is.
var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrNotFound     = errors.New("not found")
	// ErrConflict: another operation is running, the app changed concurrently,
	// a sync window blocks the sync, or another precondition failed.
	ErrConflict = errors.New("conflict")
	// ErrInvalid: Argo CD rejected the request as invalid (e.g. spec validation).
	ErrInvalid = errors.New("invalid request")
)

// gRPC status codes Argo CD returns in error bodies.
const (
	grpcInvalidArgument    = 3
	grpcAlreadyExists      = 6
	grpcFailedPrecondition = 9
	grpcAborted            = 10
)

// APIError is a non-2xx response from Argo CD.
type APIError struct {
	Status  int
	Code    int // gRPC status code from the error body, when present
	Message string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "Argo CD rejected the token (401): it is missing, invalid or expired. Refresh ARGOCD_AUTH_TOKEN"
	case http.StatusForbidden:
		return "permission denied by Argo CD RBAC (403): " + e.Message
	case http.StatusNotFound:
		return "not found (404): " + e.Message
	}
	switch {
	case e.Is(ErrConflict):
		return "Argo CD refused the request (precondition): " + e.Message
	case e.Is(ErrInvalid):
		return "Argo CD rejected the request as invalid: " + e.Message
	default:
		return fmt.Sprintf("Argo CD returned HTTP %d: %s", e.Status, e.Message)
	}
}

// Is maps status codes to the sentinel errors.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrForbidden:
		return e.Status == http.StatusForbidden
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict || e.Code == grpcFailedPrecondition || e.Code == grpcAborted || e.Code == grpcAlreadyExists
	case ErrInvalid:
		return e.Code == grpcInvalidArgument
	}

	return false
}

// Client talks to one Argo CD server on behalf of the token's user.
type Client struct {
	base   *url.URL
	http   *http.Client
	tokens authargocd.TokenSource
}

// Options configures transport security and timeouts.
type Options struct {
	CAFile             string
	InsecureSkipVerify bool
	Timeout            time.Duration
	// HTTPClient overrides the transport entirely (tests).
	HTTPClient *http.Client
}

// New returns a client for baseURL (https://host[:port][/path]).
func New(baseURL string, tokens authargocd.TokenSource, opts Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid Argo CD URL %q", baseURL)
	}
	if tokens == nil {
		return nil, errors.New("token source is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc, err = newHTTPClient(opts)
		if err != nil {
			return nil, err
		}
	}

	return &Client{base: u, http: hc, tokens: tokens}, nil
}

func newHTTPClient(opts Options) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ARGOCD_CA_FILE: %w", err)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", opts.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	tlsCfg.InsecureSkipVerify = opts.InsecureSkipVerify //nolint:gosec // explicit operator opt-in
	tr.TLSClientConfig = tlsCfg
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		// Never follow redirects: a redirect to another host would carry the token.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// get performs an authenticated GET of /api/v1/<segments> and decodes JSON into out.
func (c *Client) get(ctx context.Context, q url.Values, out any, segments ...string) error {
	return c.do(ctx, http.MethodGet, q, nil, out, segments...)
}

// do performs an authenticated request with an optional JSON body and decodes
// the JSON response into out (if non-nil).
func (c *Client) do(ctx context.Context, method string, q url.Values, body, out any, segments ...string) error {
	resp, err := c.send(ctx, method, q, body, segments...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("decode Argo CD response: %w", err)
	}

	return nil
}

// send performs an authenticated request and returns a 2xx JSON response; the
// caller must close the body. Non-2xx responses become *APIError.
func (c *Client) send(ctx context.Context, method string, q url.Values, body any, segments ...string) (*http.Response, error) {
	token, err := c.tokens.Token()
	if err != nil {
		return nil, err
	}
	u, err := c.endpoint(segments...)
	if err != nil {
		return nil, err
	}
	u.RawQuery = q.Encode()

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "argocd-mcp-go/"+version.Version)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error contains only the URL (no credentials).
		return nil, fmt.Errorf("cannot reach Argo CD: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()

		msg, code := errorMessage(resp)

		return nil, &APIError{Status: resp.StatusCode, Code: code, Message: msg}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		resp.Body.Close()

		return nil, fmt.Errorf("unexpected %q response from Argo CD (is ARGOCD_SERVER the API server, not an SSO page?)", ct)
	}

	return resp, nil
}

// endpoint builds base + /api/v1/<segments>, escaping each segment so a value
// can never add path levels or traverse ("..") out of its position.
func (c *Client) endpoint(segments ...string) (*url.URL, error) {
	u := *c.base
	raw := strings.TrimRight(u.EscapedPath(), "/") + "/api/v1"
	plain := strings.TrimRight(u.Path, "/") + "/api/v1"
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("invalid path segment %q", s)
		}
		raw += "/" + url.PathEscape(s)
		plain += "/" + s
	}
	u.Path, u.RawPath = plain, raw

	return &u, nil
}

// errorMessage extracts the grpc-gateway error message and gRPC status code.
func errorMessage(resp *http.Response) (string, int) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Message != "" {
			return e.Message, e.Code
		}
		if e.Error != "" {
			return e.Error, e.Code
		}
	}
	if s := strings.TrimSpace(string(b)); s != "" && !strings.HasPrefix(s, "<") {
		return s, 0
	}

	return http.StatusText(resp.StatusCode), 0
}
