// Command argocd-mcp is an MCP server for Argo CD with per-user Microsoft
// Entra ID single sign-on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/adapters/argocd"
	mcpadapter "github.com/vinaycharlie01/argocd-mcp-go/internal/adapters/mcp"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/auth"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/config"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/service"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/logging"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "argocd-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// Logs always go to stderr; in stdio mode stdout is the MCP stream.
	logger := logging.New(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(logger)
	logger.Info("starting argocd-mcp", "version", version.Version, "commit", version.GitCommit,
		"transport", cfg.MCP.Transport, "argocd", cfg.ArgoCD.ServerURL)
	if cfg.ArgoCD.InsecureSkipVerify {
		logger.Warn("TLS verification disabled for Argo CD (ARGOCD_INSECURE_SKIP_VERIFY=true)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hc, err := argocd.NewHTTPClient(cfg.ArgoCD.CAFile, cfg.ArgoCD.InsecureSkipVerify, cfg.ArgoCD.RequestTimeout)
	if err != nil {
		return err
	}
	api, err := argocd.New(cfg.ArgoCD.ServerURL, hc)
	if err != nil {
		return err
	}
	authMgr, err := auth.NewManager(auth.Options{
		Resolver:   argocd.ProviderResolver(api, cfg.OIDC),
		HTTPClient: hc,
		Present:    auth.TokenKindIDToken, // Argo CD validates the ID token.
		LoginTTL:   cfg.OIDC.LoginTimeout,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	local := cfg.MCP.Transport == config.TransportStdio
	cb := auth.NewLocalCallbackServer(authMgr)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = cb.Close(sctx)
	}()

	srv := mcpadapter.New(mcpadapter.Options{
		Apps:           service.NewApplications(api, authMgr),
		Auth:           authMgr,
		Logger:         logger,
		Local:          local,
		EnsureCallback: cb.Ensure,
		OpenBrowser:    cfg.OIDC.OpenBrowser,
		LoginWait:      cfg.OIDC.LoginWait,
	})

	if local {
		err := srv.RunStdio(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}

		return nil
	}

	return serveHTTP(ctx, cfg, srv, logger)
}

func serveHTTP(ctx context.Context, cfg *config.Config, srv *mcpadapter.Server, logger *slog.Logger) error {
	handler, err := srv.HTTPHandler(ctx, cfg.MCP.JSONResponse)
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr:              cfg.MCP.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.MCP.HTTPAddr, "mcp_endpoint", cfg.MCP.PublicURL+"/mcp")
		errCh <- hs.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return hs.Shutdown(sctx)
}
