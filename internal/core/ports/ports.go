// Package ports declares the interfaces the core depends on. Adapters
// (Argo CD REST client, OIDC manager) implement them.
package ports

import (
	"context"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
)

// ArgoCD is the outbound port to the Argo CD API. Every call carries the
// end user's own bearer token; the adapter holds no credentials.
type ArgoCD interface {
	SSOSettings(ctx context.Context) (*domain.SSOSettings, error)
	ListApplications(ctx context.Context, bearer string, f domain.ApplicationFilter) ([]domain.Application, error)
}

// TokenProvider returns the bearer token of the user behind an MCP session.
type TokenProvider interface {
	BearerToken(ctx context.Context, sessionKey string) (string, error)
}
