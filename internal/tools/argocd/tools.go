// Package argocd defines the read-only Argo CD MCP tools. It depends only on
// the small API interface below, so the tools are unit-testable without HTTP.
package argocd

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

// API is the subset of the Argo CD client the tools use.
type API interface {
	ListApplications(ctx context.Context, o client.ListOptions) ([]client.Application, error)
	GetApplication(ctx context.Context, ref client.AppRef) (*client.Application, error)
	GetResourceTree(ctx context.Context, ref client.AppRef) (*client.ResourceTree, error)
	GetManifests(ctx context.Context, ref client.AppRef, revision string) (*client.ManifestResponse, error)
	ListEvents(ctx context.Context, ref client.AppRef, f client.EventFilter) ([]client.Event, error)
	GetLogs(ctx context.Context, ref client.AppRef, o client.LogOptions) (*client.LogResult, error)
	GetResource(ctx context.Context, ref client.AppRef, r client.ResourceQuery) (map[string]any, error)
}

// Tools holds the dependencies of the Argo CD tools.
type Tools struct{ api API }

// New returns the Argo CD tool set.
func New(api API) *Tools { return &Tools{api: api} }

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}

func ptr[T any](v T) *T { return &v }

// Register adds every Argo CD tool to s.
func (t *Tools) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_list_applications",
		Title:       "List Argo CD applications",
		Description: "List Argo CD applications visible to the configured user (Argo CD RBAC applies), with sync and health status. Optional filters: projects, appNamespace, label selector, name search.",
		Annotations: readOnly,
	}, t.listApplications)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_get_application",
		Title:       "Get Argo CD application",
		Description: "Get details of one Argo CD application: sources, destination, sync policy, sync/health status, conditions, last operation, images and resource status counts.",
		Annotations: readOnly,
	}, t.getApplication)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_get_application_resources",
		Title:       "Get application resources",
		Description: "List the Kubernetes resources of an Argo CD application (live resource tree, including Pods and ReplicaSets) with health, sync status and parent links. Optional kind/health filters.",
		Annotations: readOnly,
	}, t.getResources)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_get_application_history",
		Title:       "Get deployment history",
		Description: "Get the deployment (sync) history of an Argo CD application, newest first: history ID, revision(s), deployed time and who initiated it.",
		Annotations: readOnly,
	}, t.getHistory)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_get_application_manifests",
		Title:       "Get application manifests",
		Description: "Get the rendered target manifests of an Argo CD application, optionally at a specific revision. Secret data is masked by Argo CD. Optional kind filter and result limit.",
		Annotations: readOnly,
	}, t.getManifests)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "argocd_get_application_events",
		Title:       "Get application events",
		Description: "Get Kubernetes events for an Argo CD application, or for one of its resources, newest first.",
		Annotations: readOnly,
	}, t.getEvents)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "argocd_get_pod_logs",
		Title: "Get pod logs",
		Description: "Get container logs through Argo CD (no cluster access needed; requires the Argo CD 'logs, get' permission). " +
			"Target either one pod (podName) or a whole workload (kind + resourceName, e.g. Deployment web, aggregating all its pods). " +
			"Use argocd_get_application_resources to find pod names. Supports tailLines, sinceSeconds, container, previous (crashed container) and a server-side filter.",
		Annotations: readOnly,
	}, t.getLogs)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "argocd_get_images",
		Title: "Get running images and digests",
		Description: "List the container images of an Argo CD application per workload and container: the image Git wants (desired), " +
			"and what pods actually run (tag and registry digest sha256, with pod names). Flags mixed rollouts and Git-vs-running drift. " +
			"Optionally pass 'expected' images (e.g. from jenkins_get_build_images) to get a deterministic verdict: " +
			"MATCH, MATCH_TAG_ONLY, MIXED_ROLLOUT, TAG_MATCH_DIGEST_DIFFERENT, DIFFERENT_TAG or NOT_FOUND.",
		Annotations: readOnly,
	}, t.getImages)
}

// appInput is embedded by every single-application tool.
type appInput struct {
	Name         string `json:"name" jsonschema:"Application name."`
	AppNamespace string `json:"appNamespace,omitempty" jsonschema:"Namespace of the Application resource (only for apps-in-any-namespace setups)."`
	Project      string `json:"project,omitempty" jsonschema:"Project of the application (optional)."`
}

func (in appInput) ref() (client.AppRef, error) {
	if err := toolkit.K8sName("name", in.Name); err != nil {
		return client.AppRef{}, err
	}
	if err := toolkit.OptionalNamespace("appNamespace", in.AppNamespace); err != nil {
		return client.AppRef{}, err
	}
	if err := toolkit.OptionalK8sName("project", in.Project); err != nil {
		return client.AppRef{}, err
	}

	return client.AppRef{Name: in.Name, AppNamespace: in.AppNamespace, Project: in.Project}, nil
}

// classify maps Argo CD client errors to error-envelope codes.
func classify(err error) string {
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		return toolkit.CodeUnauthorized
	case errors.Is(err, client.ErrForbidden):
		return toolkit.CodeForbidden
	case errors.Is(err, client.ErrNotFound):
		return toolkit.CodeNotFound
	case errors.Is(err, authargocd.ErrNoToken):
		return toolkit.CodeNotConfigured
	}

	return ""
}

func wrap(err error) error { return toolkit.Wrap(err, classify) }
