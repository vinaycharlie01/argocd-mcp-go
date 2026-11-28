package argocd

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResourceQuery identifies one live resource managed by (or belonging to) an application.
type ResourceQuery struct {
	Namespace string
	Name      string
	Group     string // "" for the core group
	Version   string // e.g. v1
	Kind      string
}

// GetResource returns the live manifest of one resource of the application.
// Argo CD masks Secret data before returning it.
func (c *Client) GetResource(ctx context.Context, ref AppRef, r ResourceQuery) (map[string]any, error) {
	q := ref.query()
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("namespace", r.Namespace)
	set("resourceName", r.Name)
	set("group", r.Group)
	set("version", r.Version)
	set("kind", r.Kind)

	var resp struct {
		Manifest string `json:"manifest"`
	}
	if err := c.get(ctx, q, &resp, "applications", ref.Name, "resource"); err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(resp.Manifest), &obj); err != nil {
		return nil, fmt.Errorf("decode live manifest: %w", err)
	}

	return obj, nil
}
