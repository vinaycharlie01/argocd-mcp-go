// Package domain holds the Argo CD entities exposed by the MCP tools. It has
// no dependencies on transports or HTTP clients.
package domain

import "errors"

var (
	// ErrUnauthorized means the platform rejected the user's token.
	ErrUnauthorized = errors.New("argo cd rejected the credentials (401); log in again")
	// ErrForbidden means the user is authenticated but Argo CD RBAC denies the action.
	ErrForbidden = errors.New("permission denied by Argo CD RBAC (403)")
)

// Application is a summary of an Argo CD Application.
type Application struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace,omitempty"`
	Project        string `json:"project"`
	RepoURL        string `json:"repoURL,omitempty"`
	Path           string `json:"path,omitempty"`
	Chart          string `json:"chart,omitempty"`
	TargetRevision string `json:"targetRevision,omitempty"`
	DestServer     string `json:"destinationServer,omitempty"`
	DestName       string `json:"destinationName,omitempty"`
	DestNamespace  string `json:"destinationNamespace,omitempty"`
	SyncStatus     string `json:"syncStatus,omitempty"`
	HealthStatus   string `json:"healthStatus,omitempty"`
	Revision       string `json:"revision,omitempty"`
	OperationPhase string `json:"operationPhase,omitempty"`
	AutoSync       bool   `json:"autoSync"`
}

// ApplicationFilter narrows a list request. Empty fields match everything.
type ApplicationFilter struct {
	Projects     []string
	Selector     string
	AppNamespace string
	Search       string
}

// SSOSettings is the public SSO configuration Argo CD advertises.
type SSOSettings struct {
	URL string
	// Direct OIDC (e.g. Entra ID configured in argocd-cm oidc.config).
	OIDCIssuer      string
	OIDCClientID    string
	OIDCCLIClientID string
	OIDCScopes      []string
	// Bundled Dex.
	DexConnectors      []string
	DexAuthConnectorID string
}
