package argocd

import (
	"context"
	"net/url"
)

// ListOptions filters application listing. Empty fields match all.
type ListOptions struct {
	Projects     []string
	AppNamespace string
	Selector     string
}

// AppRef identifies one application.
type AppRef struct {
	Name         string
	AppNamespace string // apps-in-any-namespace; empty = Argo CD's namespace
	Project      string // optional; lets Argo CD authorize without a lookup
}

func (r AppRef) query() url.Values {
	q := url.Values{}
	if r.AppNamespace != "" {
		q.Set("appNamespace", r.AppNamespace)
	}
	if r.Project != "" {
		q.Set("project", r.Project)
	}

	return q
}

// ListApplications lists applications visible to the token's user.
func (c *Client) ListApplications(ctx context.Context, o ListOptions) ([]Application, error) {
	q := url.Values{}
	for _, p := range o.Projects {
		q.Add("projects", p)
	}
	if o.AppNamespace != "" {
		q.Set("appNamespace", o.AppNamespace)
	}
	if o.Selector != "" {
		q.Set("selector", o.Selector)
	}
	var list applicationList
	if err := c.get(ctx, q, &list, "applications"); err != nil {
		return nil, err
	}

	return list.Items, nil
}

// GetApplication returns one application.
func (c *Client) GetApplication(ctx context.Context, ref AppRef) (*Application, error) {
	var app Application
	if err := c.get(ctx, ref.query(), &app, "applications", ref.Name); err != nil {
		return nil, err
	}

	return &app, nil
}

// GetResourceTree returns the live resource tree of an application.
func (c *Client) GetResourceTree(ctx context.Context, ref AppRef) (*ResourceTree, error) {
	var tree ResourceTree
	if err := c.get(ctx, ref.query(), &tree, "applications", ref.Name, "resource-tree"); err != nil {
		return nil, err
	}

	return &tree, nil
}

// GetManifests returns rendered manifests for revision (empty = target revision).
func (c *Client) GetManifests(ctx context.Context, ref AppRef, revision string) (*ManifestResponse, error) {
	q := ref.query()
	if revision != "" {
		q.Set("revision", revision)
	}
	var m ManifestResponse
	if err := c.get(ctx, q, &m, "applications", ref.Name, "manifests"); err != nil {
		return nil, err
	}

	return &m, nil
}

// EventFilter narrows events to one resource of the application.
type EventFilter struct {
	ResourceNamespace string
	ResourceName      string
	ResourceUID       string
}

// ListEvents returns Kubernetes events for the application (or one of its resources).
func (c *Client) ListEvents(ctx context.Context, ref AppRef, f EventFilter) ([]Event, error) {
	q := ref.query()
	if f.ResourceNamespace != "" {
		q.Set("resourceNamespace", f.ResourceNamespace)
	}
	if f.ResourceName != "" {
		q.Set("resourceName", f.ResourceName)
	}
	if f.ResourceUID != "" {
		q.Set("resourceUID", f.ResourceUID)
	}
	var list eventList
	if err := c.get(ctx, q, &list, "applications", ref.Name, "events"); err != nil {
		return nil, err
	}

	return list.Items, nil
}
