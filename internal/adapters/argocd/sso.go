package argocd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/auth"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/config"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
)

// Constants mirrored from Argo CD (common/common.go, util/oidc).
const (
	dexAPIEndpoint    = "/api/dex"
	dexCLIClientID    = "argo-cd-cli"
	dexFederatedScope = "federated:id"
)

var defaultScopes = []string{"openid", "profile", "email", "groups"}

// ProviderResolver returns an auth.ConfigResolver that derives the OIDC client
// the same way `argocd login --sso` does:
//
//   - oidc.config (Entra ID directly): issuer from settings, client ID =
//     cliClientID if set, else clientID; scopes from settings.
//   - bundled Dex (Entra ID as a Dex connector): issuer <argocd>/api/dex,
//     client ID argo-cd-cli, plus the federated:id scope.
//
// Any OIDC_* environment override wins over discovered values.
func ProviderResolver(c *Client, o config.OIDCConfig) auth.ConfigResolver {
	return func(ctx context.Context) (auth.ProviderConfig, error) {
		pc := auth.ProviderConfig{
			IssuerURL:    o.IssuerURL,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Scopes:       o.Scopes,
			RedirectURL:  o.RedirectURL,
			AuthParams:   map[string]string{},
		}
		for k, v := range o.AuthParams {
			pc.AuthParams[k] = v
		}
		if pc.IssuerURL != "" && pc.ClientID != "" {
			if len(pc.Scopes) == 0 {
				pc.Scopes = defaultScopes
			}

			return pc, nil
		}

		s, err := c.SSOSettings(ctx)
		if err != nil {
			return pc, fmt.Errorf("read Argo CD SSO settings: %w", err)
		}
		return fromSettings(pc, s)
	}
}

func fromSettings(pc auth.ProviderConfig, s *domain.SSOSettings) (auth.ProviderConfig, error) {
	switch {
	case s.OIDCIssuer != "":
		if pc.IssuerURL == "" {
			pc.IssuerURL = s.OIDCIssuer
		}
		if pc.ClientID == "" {
			pc.ClientID = s.OIDCCLIClientID
			if pc.ClientID == "" {
				pc.ClientID = s.OIDCClientID
			}
		}
		if len(pc.Scopes) == 0 {
			pc.Scopes = s.OIDCScopes
		}
		if len(pc.Scopes) == 0 {
			pc.Scopes = defaultScopes
		}
	case len(s.DexConnectors) > 0:
		if pc.IssuerURL == "" {
			pc.IssuerURL = s.URL + dexAPIEndpoint
		}
		if pc.ClientID == "" {
			pc.ClientID = dexCLIClientID
		}
		if len(pc.Scopes) == 0 {
			pc.Scopes = slices.Clone(defaultScopes)
		}
		if !slices.Contains(pc.Scopes, dexFederatedScope) {
			pc.Scopes = append(pc.Scopes, dexFederatedScope)
		}
		if s.DexAuthConnectorID != "" {
			pc.AuthParams["connector_id"] = s.DexAuthConnectorID
		}
	default:
		return pc, errors.New("argo cd reports no SSO configuration (neither oidc.config nor dex.config); set OIDC_ISSUER_URL and OIDC_CLIENT_ID")
	}

	return pc, nil
}
