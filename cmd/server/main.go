// Command server is a read-only MCP server for Argo CD that runs with the
// user's own credentials.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
	clientargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/config"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/logging"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/mcp"
	toolsargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/tools/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/transport"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devops-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	// stderr only: stdout is the MCP JSON-RPC stream.
	log := logging.New(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(log)

	var sets []mcp.Registrar
	if a := cfg.ArgoCD; a != nil {
		tokens := authargocd.New(a.Token, a.TokenFile)
		c, err := clientargocd.New(a.BaseURL, tokens, clientargocd.Options{
			CAFile: a.CAFile, InsecureSkipVerify: a.InsecureSkipVerify, Timeout: a.Timeout,
		})
		if err != nil {
			return fmt.Errorf("argo cd client: %w", err)
		}
		sets = append(sets, toolsargocd.New(c))
		log.Info("argo cd integration enabled", "server", a.BaseURL, "auth_source", authSource(a), "timeout", a.Timeout.String())
		if a.InsecureSkipVerify {
			log.Warn("TLS verification disabled for Argo CD (ARGOCD_INSECURE=true)")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting MCP server", "version", version.Version, "transport", "stdio")

	return transport.RunStdio(ctx, mcp.NewServer(sets...))
}

func authSource(a *config.ArgoCD) string {
	if a.TokenFile != "" {
		return "file"
	}

	return "env"
}
