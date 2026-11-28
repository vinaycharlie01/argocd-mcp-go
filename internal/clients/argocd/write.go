package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// PatchOp is one RFC 6902 JSON Patch operation.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// PatchApplication applies a JSON Patch to the Application
// (PATCH /api/v1/applications/{name}, RBAC: applications, update). Argo CD
// validates the patched spec; a failing "test" op means the app changed
// concurrently and nothing is written.
func (c *Client) PatchApplication(ctx context.Context, ref AppRef, ops []PatchOp) (*Application, error) {
	patch, err := json.Marshal(ops)
	if err != nil {
		return nil, fmt.Errorf("encode patch: %w", err)
	}
	body := map[string]string{"name": ref.Name, "patch": string(patch), "patchType": "json"}
	if ref.AppNamespace != "" {
		body["appNamespace"] = ref.AppNamespace
	}
	if ref.Project != "" {
		body["project"] = ref.Project
	}
	var app Application
	if err := c.do(ctx, http.MethodPatch, nil, body, &app, "applications", ref.Name); err != nil {
		return nil, err
	}

	return &app, nil
}

// SyncOptions configures a sync.
type SyncOptions struct {
	Prune  bool
	DryRun bool
}

// SyncApplication starts a sync to the application's current target
// (POST /api/v1/applications/{name}/sync, RBAC: applications, sync).
// It returns as soon as the operation is accepted.
func (c *Client) SyncApplication(ctx context.Context, ref AppRef, o SyncOptions) (*Application, error) {
	body := map[string]any{"name": ref.Name, "prune": o.Prune, "dryRun": o.DryRun}
	if ref.AppNamespace != "" {
		body["appNamespace"] = ref.AppNamespace
	}
	if ref.Project != "" {
		body["project"] = ref.Project
	}
	var app Application
	if err := c.do(ctx, http.MethodPost, nil, body, &app, "applications", ref.Name, "sync"); err != nil {
		return nil, err
	}

	return &app, nil
}
