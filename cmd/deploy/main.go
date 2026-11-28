// Command deploy is a WRITE-ENABLED Argo CD MCP server with exactly three
// tools: application status, image update (Argo CD parameter override, no
// Git) and a confirmed sync with health verification. It uses only the Argo
// CD REST API with the user's own token; Argo CD RBAC applies to every call.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
	clientargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/config"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/deploy"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/logging"
	toolsdeploy "github.com/vinaycharlie01/argocd-mcp-go/internal/tools/deploy"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/transport"
	"github.com/vinaycharlie01/argocd-mcp-go/pkg/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "argocd-deploy-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	if cfg.ArgoCD == nil {
		return fmt.Errorf("configuration: ARGOCD_SERVER is required")
	}
	dcfg, err := config.LoadDeploy()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	// stderr only: stdout is the MCP JSON-RPC stream.
	log := logging.New(os.Stderr, cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(log)

	a := cfg.ArgoCD
	c, err := clientargocd.New(a.BaseURL, authargocd.New(a.Token, a.TokenFile), clientargocd.Options{
		CAFile: a.CAFile, InsecureSkipVerify: a.InsecureSkipVerify, Timeout: a.Timeout,
	})
	if err != nil {
		return fmt.Errorf("argo cd client: %w", err)
	}
	svc := deploy.NewService(c, deploy.Config{
		AllowedProjects: dcfg.AllowedProjects,
		HelmTagParam:    dcfg.HelmTagParam,
		HelmRepoParam:   dcfg.HelmRepoParam,
		SyncTimeout:     dcfg.SyncTimeout,
		MaxSyncTimeout:  config.MaxSyncTimeout,
		ConfirmTTL:      5 * time.Minute,
	})

	log.Warn("WRITE-ENABLED server: update_application_image and argocd_sync_application change Argo CD applications",
		"server", a.BaseURL, "allowed_projects", dcfg.AllowedProjects, "helm_tag_param", dcfg.HelmTagParam,
		"helm_repository_param", dcfg.HelmRepoParam, "sync_timeout", dcfg.SyncTimeout.String())
	if a.InsecureSkipVerify {
		log.Warn("TLS verification disabled for Argo CD (ARGOCD_INSECURE=true)")
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "argocd-deploy-mcp", Title: "Argo CD Deploy (image update + confirmed sync)", Version: version.Version},
		&mcp.ServerOptions{Instructions: "Write-enabled Argo CD tools. Never pass a confirmationToken unless the developer explicitly approved " +
			"the preview in this conversation. Errors are JSON: {\"error\":{\"code\",\"message\"}}."})
	toolsdeploy.New(svc).Register(s)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting MCP server", "version", version.Version, "transport", "stdio")

	return transport.RunStdio(ctx, s)
}
