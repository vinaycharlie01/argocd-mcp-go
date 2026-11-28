// Package service implements the use cases behind the MCP tools.
package service

import (
	"context"
	"sort"
	"strings"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/ports"
)

// Applications implements application use cases on behalf of a user.
type Applications struct {
	api    ports.ArgoCD
	tokens ports.TokenProvider
}

// NewApplications wires the use case to its ports.
func NewApplications(api ports.ArgoCD, tokens ports.TokenProvider) *Applications {
	return &Applications{api: api, tokens: tokens}
}

// List returns the applications the session's user is allowed to see.
// Filtering by RBAC happens in Argo CD itself.
func (s *Applications) List(ctx context.Context, sessionKey string, f domain.ApplicationFilter) ([]domain.Application, error) {
	token, err := s.tokens.BearerToken(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	apps, err := s.api.ListApplications(ctx, token, f)
	if err != nil {
		return nil, err
	}
	if q := strings.ToLower(strings.TrimSpace(f.Search)); q != "" {
		filtered := apps[:0]
		for _, a := range apps {
			if strings.Contains(strings.ToLower(a.Name), q) {
				filtered = append(filtered, a)
			}
		}
		apps = filtered
	}
	sort.Slice(apps, func(i, j int) bool {
		if apps[i].Project != apps[j].Project {
			return apps[i].Project < apps[j].Project
		}

		return apps[i].Name < apps[j].Name
	})

	return apps, nil
}
