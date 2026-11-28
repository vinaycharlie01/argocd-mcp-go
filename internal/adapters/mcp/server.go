// Package mcp is the inbound adapter: it exposes the core use cases as MCP
// tools over stdio (local, single user) or Streamable HTTP (remote, one login
// per MCP session).
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/auth"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/service"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"
)

// Authenticator is what the MCP layer needs from the auth manager.
type Authenticator interface {
	StartLogin(ctx context.Context, sessionKey string) (*auth.LoginAttempt, error)
	BearerToken(ctx context.Context, sessionKey string) (string, error)
	Identity(sessionKey string) (*auth.Identity, bool)
	Logout(sessionKey string) (string, bool)
	CallbackHandler() http.Handler
	RedirectURL(ctx context.Context) (string, error)
}

// Options configures the MCP adapter.
type Options struct {
	Apps   *service.Applications
	Auth   Authenticator
	Logger *slog.Logger
	// Local is true for the stdio transport: one user, loopback callback.
	Local bool
	// EnsureCallback starts the loopback callback listener (local mode only).
	EnsureCallback func(context.Context) error
	OpenBrowser    bool
	LoginWait      time.Duration
}

// Server owns the MCP server instance.
type Server struct {
	opts Options
	log  *slog.Logger
	srv  *mcp.Server
}

// New builds the MCP server and registers its tools.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.LoginWait <= 0 {
		opts.LoginWait = 2 * time.Minute
	}
	s := &Server{opts: opts, log: opts.Logger.With("component", "mcp")}
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:    "argocd-mcp-go",
		Title:   "Argo CD MCP (Entra ID SSO)",
		Version: version.Version,
	}, &mcp.ServerOptions{
		Instructions: "Call the `login` tool first: it signs the user in to Argo CD with their own Microsoft Entra ID account. " +
			"All other tools act with that user's Argo CD RBAC permissions.",
	})
	s.registerTools()

	return s
}

// MCPServer exposes the underlying SDK server (for tests).
func (s *Server) MCPServer() *mcp.Server { return s.srv }

// RunStdio serves a single local user over stdin/stdout.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.srv.Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler returns the Streamable HTTP handler plus the OAuth callback.
func (s *Server) HTTPHandler(ctx context.Context, jsonResponse bool) (http.Handler, error) {
	redirect, err := s.opts.Auth.RedirectURL(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(redirect)
	if err != nil {
		return nil, fmt.Errorf("parse redirect URL: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.srv },
		&mcp.StreamableHTTPOptions{
			JSONResponse:          jsonResponse,
			Logger:                s.log,
			CrossOriginProtection: http.NewCrossOriginProtection(),
		}))
	mux.Handle(u.Path, s.opts.Auth.CallbackHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux, nil
}

// sessionKey identifies the user behind a tool call: the single local user in
// stdio mode, or the MCP session in HTTP mode.
func (s *Server) sessionKey(req *mcp.CallToolRequest) (string, error) {
	if s.opts.Local {
		return auth.LocalSessionKey, nil
	}
	if req != nil && req.Session != nil && req.Session.ID() != "" {
		return req.Session.ID(), nil
	}

	return "", errors.New("no MCP session: the HTTP transport must run in stateful mode")
}
