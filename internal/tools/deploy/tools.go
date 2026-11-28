// Package deploy exposes the deploy use cases as exactly three MCP tools.
package deploy

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	core "github.com/vinaycharlie01/argocd-mcp-go/internal/deploy"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

// Tools adapts the deploy service to MCP.
type Tools struct{ svc *core.Service }

// New returns the deploy tool set.
func New(svc *core.Service) *Tools { return &Tools{svc: svc} }

func ptr[T any](v T) *T { return &v }

// Register adds the three tools.
func (t *Tools) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "argocd_get_application_status",
		Title: "Argo CD: application status and images",
		Description: "Read-only. Show an application's sync and health status, running images, image overrides " +
			"(Helm image parameters or Kustomize images) per source, auto-sync, and any running operation. Use it before " +
			"update_application_image and to re-check a deployment.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, t.status)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "update_application_image",
		Title: "Argo CD: set image tag/repository (no Git)",
		Description: "Change the image tag (and optionally repository) of an application through Argo CD parameter overrides, " +
			"without changing Git. Helm: sets the tag/repository parameters (default image.tag / image.repository) as strings and keeps " +
			"every other parameter and setting. Kustomize: updates the matching kustomize image. It verifies the change reaches a rendered " +
			"container image. It does NOT sync, unless the app has auto-sync, in which case it first returns CONFIRMATION_REQUIRED: " +
			"show the plan to the developer and call again with confirmationToken only after explicit approval.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: true, OpenWorldHint: ptr(true)},
	}, t.update)

	mcp.AddTool(s, &mcp.Tool{
		Name:  "argocd_sync_application",
		Title: "Argo CD: sync (deploy) with confirmation",
		Description: "Deploy the application's current desired state. First call (no confirmationToken) returns a preview " +
			"(out-of-sync resources, images, prune) and a confirmationToken; it does not sync. Show the preview to the developer and only " +
			"after they explicitly approve, call again with the token. The sync then runs and the tool waits until the operation finished and " +
			"health settled: DEPLOYED_HEALTHY, SYNC_FAILED, DEGRADED, SUSPENDED or TIMEOUT. prune defaults to false.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true), IdempotentHint: false, OpenWorldHint: ptr(true)},
	}, t.sync)
}

type appInput struct {
	Name         string `json:"name" jsonschema:"Application name."`
	AppNamespace string `json:"appNamespace,omitempty" jsonschema:"Namespace of the Application resource (only for apps-in-any-namespace)."`
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

func (t *Tools) status(ctx context.Context, _ *mcp.CallToolRequest, in appInput) (*mcp.CallToolResult, *core.Status, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, nil, err
	}
	st, err := t.svc.Status(ctx, ref)
	if err != nil {
		return nil, nil, wrap(err)
	}

	return nil, st, nil
}

type updateInput struct {
	appInput
	Tag                     string `json:"tag" jsonschema:"New image tag, e.g. 1.4.2."`
	Repository              string `json:"repository,omitempty" jsonschema:"New image repository without tag, e.g. registry.example.com/team/app (optional)."`
	SourceIndex             *int   `json:"sourceIndex,omitempty" jsonschema:"Source to change in a multi-source application (optional when only one Helm/Kustomize source)."`
	HelmTagParameter        string `json:"helmTagParameter,omitempty" jsonschema:"Helm parameter holding the tag (default from ARGOCD_HELM_TAG_PARAM, else image.tag)."`
	HelmRepositoryParameter string `json:"helmRepositoryParameter,omitempty" jsonschema:"Helm parameter holding the repository (default from ARGOCD_HELM_REPOSITORY_PARAM, else image.repository)."`
	KustomizeImage          string `json:"kustomizeImage,omitempty" jsonschema:"Kustomize: image name to override as used in the manifests (optional when unambiguous)."`
	ConfirmationToken       string `json:"confirmationToken,omitempty" jsonschema:"Only for auto-sync apps: token from the previous call, after the developer approved."`
}

func (t *Tools) update(ctx context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, *core.UpdateResult, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, nil, err
	}
	if err := toolkit.Text("confirmationToken", in.ConfirmationToken, 64); err != nil {
		return nil, nil, err
	}
	u := core.ImageUpdate{
		Tag: in.Tag, Repository: in.Repository, SourceIndex: in.SourceIndex,
		HelmTagParam: in.HelmTagParameter, HelmRepoParam: in.HelmRepositoryParameter, KustomizeImage: in.KustomizeImage,
	}
	if u.HelmTagParam == "" {
		u.HelmTagParam = t.svc.Defaults().HelmTagParam
	}
	if u.HelmRepoParam == "" {
		u.HelmRepoParam = t.svc.Defaults().HelmRepoParam
	}
	if err := u.Validate(); err != nil {
		return nil, nil, toolkit.Invalid("%v", err)
	}
	res, err := t.svc.UpdateImage(ctx, ref, u, in.ConfirmationToken)
	if err != nil {
		return nil, nil, wrap(err)
	}

	return nil, res, nil
}

type syncInput struct {
	appInput
	ConfirmationToken string `json:"confirmationToken,omitempty" jsonschema:"Token from the preview call; only pass it after the developer explicitly approved."`
	Prune             bool   `json:"prune,omitempty" jsonschema:"Delete resources no longer in the desired state (default false). Must match the preview."`
	TimeoutSeconds    int    `json:"timeoutSeconds,omitempty" jsonschema:"How long to wait for the sync and health (default 300, max 900)."`
	ExpectedImage     string `json:"expectedImage,omitempty" jsonschema:"Optional repository:tag that must be running afterwards (e.g. the image just set)."`
}

func (t *Tools) sync(ctx context.Context, _ *mcp.CallToolRequest, in syncInput) (*mcp.CallToolResult, *core.SyncResult, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, nil, err
	}
	if err := toolkit.Text("confirmationToken", in.ConfirmationToken, 64); err != nil {
		return nil, nil, err
	}
	if err := toolkit.Text("expectedImage", in.ExpectedImage, 512); err != nil {
		return nil, nil, err
	}
	maxSecs := int(t.svc.Defaults().MaxSyncTimeout / time.Second)
	if in.TimeoutSeconds < 0 || in.TimeoutSeconds > maxSecs {
		return nil, nil, toolkit.Invalid("timeoutSeconds must be between 1 and %d", maxSecs)
	}
	res, err := t.svc.Sync(ctx, ref, core.SyncRequest{
		Prune: in.Prune, Token: in.ConfirmationToken, Timeout: time.Duration(in.TimeoutSeconds) * time.Second, ExpectedImage: in.ExpectedImage,
	})
	if err != nil {
		return nil, nil, wrap(err)
	}

	return nil, res, nil
}

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
	case errors.Is(err, core.ErrConfirmation):
		return toolkit.CodeConfirmation
	case errors.Is(err, client.ErrConflict), errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrConcurrentChange):
		return toolkit.CodeConflict
	case errors.Is(err, core.ErrNotAllowed), errors.Is(err, core.ErrManaged), errors.Is(err, core.ErrNoOverridableSource):
		return toolkit.CodePreconditionFailed
	case errors.Is(err, client.ErrInvalid), errors.Is(err, core.ErrInvalidInput):
		return toolkit.CodeInvalidInput
	}

	return ""
}

func wrap(err error) error { return toolkit.Wrap(err, classify) }
