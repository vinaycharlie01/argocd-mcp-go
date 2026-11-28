package argocd

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

// ---- argocd_list_applications ----

type listInput struct {
	Projects     []string `json:"projects,omitempty" jsonschema:"Only applications in these projects."`
	AppNamespace string   `json:"appNamespace,omitempty" jsonschema:"Only applications whose Application resource is in this namespace."`
	Selector     string   `json:"selector,omitempty" jsonschema:"Kubernetes label selector, e.g. team=payments."`
	Search       string   `json:"search,omitempty" jsonschema:"Case-insensitive substring match on the application name."`
}

// AppSummary is one row of argocd_list_applications.
type AppSummary struct {
	Name                 string `json:"name"`
	Namespace            string `json:"namespace,omitempty"`
	Project              string `json:"project"`
	RepoURL              string `json:"repoURL,omitempty"`
	Path                 string `json:"path,omitempty"`
	Chart                string `json:"chart,omitempty"`
	TargetRevision       string `json:"targetRevision,omitempty"`
	DestinationServer    string `json:"destinationServer,omitempty"`
	DestinationName      string `json:"destinationName,omitempty"`
	DestinationNamespace string `json:"destinationNamespace,omitempty"`
	SyncStatus           string `json:"syncStatus,omitempty"`
	HealthStatus         string `json:"healthStatus,omitempty"`
	Revision             string `json:"revision,omitempty"`
	OperationPhase       string `json:"operationPhase,omitempty"`
	AutoSync             bool   `json:"autoSync"`
}

type listOutput struct {
	Count        int          `json:"count"`
	Applications []AppSummary `json:"applications"`
}

func (t *Tools) listApplications(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	for _, p := range in.Projects {
		if err := toolkit.K8sName("projects[]", p); err != nil {
			return nil, listOutput{}, err
		}
	}
	if err := toolkit.OptionalNamespace("appNamespace", in.AppNamespace); err != nil {
		return nil, listOutput{}, err
	}
	if err := toolkit.Text("selector", in.Selector, 1024); err != nil {
		return nil, listOutput{}, err
	}
	if err := toolkit.Text("search", in.Search, 253); err != nil {
		return nil, listOutput{}, err
	}

	apps, err := t.api.ListApplications(ctx, client.ListOptions{Projects: in.Projects, AppNamespace: in.AppNamespace, Selector: in.Selector})
	if err != nil {
		return nil, listOutput{}, wrap(err)
	}
	q := strings.ToLower(strings.TrimSpace(in.Search))
	out := listOutput{Applications: make([]AppSummary, 0, len(apps))}
	for i := range apps {
		if q != "" && !strings.Contains(strings.ToLower(apps[i].Metadata.Name), q) {
			continue
		}
		out.Applications = append(out.Applications, summarize(&apps[i]))
	}
	sort.Slice(out.Applications, func(i, j int) bool {
		a, b := out.Applications[i], out.Applications[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}

		return a.Name < b.Name
	})
	out.Count = len(out.Applications)

	return nil, out, nil
}

func primarySource(a *client.Application) *client.Source {
	if a.Spec.Source != nil {
		return a.Spec.Source
	}
	if len(a.Spec.Sources) > 0 {
		return &a.Spec.Sources[0]
	}

	return nil
}

func hasAutomated(raw json.RawMessage) bool {
	var p struct {
		Automated *json.RawMessage `json:"automated"`
	}

	return len(raw) > 0 && json.Unmarshal(raw, &p) == nil && p.Automated != nil
}

func summarize(a *client.Application) AppSummary {
	s := AppSummary{
		Name:                 a.Metadata.Name,
		Namespace:            a.Metadata.Namespace,
		Project:              a.Spec.Project,
		DestinationServer:    a.Spec.Destination.Server,
		DestinationName:      a.Spec.Destination.Name,
		DestinationNamespace: a.Spec.Destination.Namespace,
		SyncStatus:           a.Status.Sync.Status,
		HealthStatus:         a.Status.Health.Status,
		Revision:             a.Status.Sync.Revision,
		AutoSync:             hasAutomated(a.Spec.SyncPolicy),
	}
	if src := primarySource(a); src != nil {
		s.RepoURL, s.Path, s.Chart, s.TargetRevision = src.RepoURL, src.Path, src.Chart, src.TargetRevision
	}
	if s.Revision == "" && len(a.Status.Sync.Revisions) > 0 {
		s.Revision = a.Status.Sync.Revisions[0]
	}
	if a.Status.OperationState != nil {
		s.OperationPhase = a.Status.OperationState.Phase
	}

	return s
}

// ---- argocd_get_application ----

// AppDetails is the output of argocd_get_application.
type AppDetails struct {
	AppSummary
	Labels           map[string]string       `json:"labels,omitempty"`
	CreatedAt        string                  `json:"createdAt,omitempty"`
	Sources          []client.Source         `json:"sources"`
	SyncPolicy       map[string]any          `json:"syncPolicy,omitempty"`
	HealthMessage    string                  `json:"healthMessage,omitempty"`
	SourceType       string                  `json:"sourceType,omitempty"`
	ReconciledAt     string                  `json:"reconciledAt,omitempty"`
	Conditions       []client.Condition      `json:"conditions,omitempty"`
	LastOperation    *client.OperationState  `json:"lastOperation,omitempty"`
	Images           []string                `json:"images,omitempty"`
	ExternalURLs     []string                `json:"externalURLs,omitempty"`
	ResourceCounts   resourceCounts          `json:"resourceCounts"`
	ProblemResources []client.ResourceStatus `json:"problemResources,omitempty"`
	HistoryCount     int                     `json:"historyCount"`
}

type resourceCounts struct {
	Total     int `json:"total"`
	OutOfSync int `json:"outOfSync"`
	Unhealthy int `json:"unhealthy"`
}

func (t *Tools) getApplication(ctx context.Context, _ *mcp.CallToolRequest, in appInput) (*mcp.CallToolResult, AppDetails, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, AppDetails{}, err
	}
	a, err := t.api.GetApplication(ctx, ref)
	if err != nil {
		return nil, AppDetails{}, wrap(err)
	}

	d := AppDetails{
		AppSummary:    summarize(a),
		Labels:        a.Metadata.Labels,
		CreatedAt:     a.Metadata.CreationTimestamp,
		Sources:       a.Spec.Sources,
		HealthMessage: a.Status.Health.Message,
		SourceType:    a.Status.SourceType,
		ReconciledAt:  a.Status.ReconciledAt,
		Conditions:    a.Status.Conditions,
		LastOperation: a.Status.OperationState,
		Images:        a.Status.Summary.Images,
		ExternalURLs:  a.Status.Summary.ExternalURLs,
		HistoryCount:  len(a.Status.History),
	}
	if a.Spec.Source != nil {
		d.Sources = append([]client.Source{*a.Spec.Source}, d.Sources...)
	}
	if d.Sources == nil {
		d.Sources = []client.Source{}
	}
	if len(a.Spec.SyncPolicy) > 0 {
		_ = json.Unmarshal(a.Spec.SyncPolicy, &d.SyncPolicy)
	}
	for _, r := range a.Status.Resources {
		d.ResourceCounts.Total++
		problem := false
		if r.Status != "" && r.Status != "Synced" {
			d.ResourceCounts.OutOfSync++
			problem = true
		}
		if r.Health != nil && r.Health.Status != "" && r.Health.Status != "Healthy" {
			d.ResourceCounts.Unhealthy++
			problem = true
		}
		if problem {
			d.ProblemResources = append(d.ProblemResources, r)
		}
	}

	return nil, d, nil
}

// ---- argocd_get_application_resources ----

type resourcesInput struct {
	appInput
	Kind            string `json:"kind,omitempty" jsonschema:"Only resources of this kind, e.g. Deployment or Pod (case-insensitive)."`
	Health          string `json:"health,omitempty" jsonschema:"Only resources with this health status, e.g. Degraded, Progressing, Missing."`
	IncludeOrphaned bool   `json:"includeOrphaned,omitempty" jsonschema:"Also include orphaned resources (if orphaned-resource monitoring is enabled)."`
}

// Resource is one node of the resource tree.
type Resource struct {
	Group         string            `json:"group,omitempty"`
	Version       string            `json:"version,omitempty"`
	Kind          string            `json:"kind"`
	Namespace     string            `json:"namespace,omitempty"`
	Name          string            `json:"name"`
	SyncStatus    string            `json:"syncStatus,omitempty"`
	Health        string            `json:"health,omitempty"`
	HealthMessage string            `json:"healthMessage,omitempty"`
	Parents       []string          `json:"parents,omitempty"`
	Images        []string          `json:"images,omitempty"`
	Info          map[string]string `json:"info,omitempty"`
	CreatedAt     string            `json:"createdAt,omitempty"`
	Orphaned      bool              `json:"orphaned,omitempty"`
}

type resourcesOutput struct {
	Application string     `json:"application"`
	Count       int        `json:"count"`
	Resources   []Resource `json:"resources"`
}

func (t *Tools) getResources(ctx context.Context, _ *mcp.CallToolRequest, in resourcesInput) (*mcp.CallToolResult, resourcesOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, resourcesOutput{}, err
	}
	if err := toolkit.Text("kind", in.Kind, 253); err != nil {
		return nil, resourcesOutput{}, err
	}
	if err := toolkit.Text("health", in.Health, 64); err != nil {
		return nil, resourcesOutput{}, err
	}
	tree, err := t.api.GetResourceTree(ctx, ref)
	if err != nil {
		return nil, resourcesOutput{}, wrap(err)
	}

	// Sync status lives on the Application, not the tree. Best effort: the
	// tree is still useful if this lookup fails.
	syncByKey := map[string]string{}
	if app, err := t.api.GetApplication(ctx, ref); err == nil {
		for _, r := range app.Status.Resources {
			syncByKey[resourceKey(r.Group, r.Kind, r.Namespace, r.Name)] = r.Status
		}
	}

	out := resourcesOutput{Application: ref.Name, Resources: []Resource{}}
	add := func(n client.ResourceNode, orphaned bool) {
		if in.Kind != "" && !strings.EqualFold(n.Kind, in.Kind) {
			return
		}
		r := Resource{
			Group: n.Group, Version: n.Version, Kind: n.Kind, Namespace: n.Namespace, Name: n.Name,
			SyncStatus: syncByKey[resourceKey(n.Group, n.Kind, n.Namespace, n.Name)],
			Images:     n.Images, CreatedAt: n.CreatedAt, Orphaned: orphaned,
		}
		if n.Health != nil {
			r.Health, r.HealthMessage = n.Health.Status, n.Health.Message
		}
		if in.Health != "" && !strings.EqualFold(r.Health, in.Health) {
			return
		}
		for _, p := range n.ParentRefs {
			r.Parents = append(r.Parents, p.Kind+"/"+p.Name)
		}
		if len(n.Info) > 0 {
			r.Info = make(map[string]string, len(n.Info))
			for _, i := range n.Info {
				r.Info[i.Name] = i.Value
			}
		}
		out.Resources = append(out.Resources, r)
	}
	for _, n := range tree.Nodes {
		add(n, false)
	}
	if in.IncludeOrphaned {
		for _, n := range tree.OrphanedNodes {
			add(n, true)
		}
	}
	out.Count = len(out.Resources)

	return nil, out, nil
}

func resourceKey(group, kind, ns, name string) string {
	return group + "/" + kind + "/" + ns + "/" + name
}

// ---- argocd_get_application_history ----

type historyInput struct {
	appInput
	Limit int `json:"limit,omitempty" jsonschema:"Maximum entries to return (default 20, max 100)."`
}

type historyOutput struct {
	Application string                `json:"application"`
	Total       int                   `json:"total"`
	Count       int                   `json:"count"`
	History     []client.HistoryEntry `json:"history"`
}

func (t *Tools) getHistory(ctx context.Context, _ *mcp.CallToolRequest, in historyInput) (*mcp.CallToolResult, historyOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, historyOutput{}, err
	}
	limit, err := toolkit.Limit("limit", in.Limit, 20, 100)
	if err != nil {
		return nil, historyOutput{}, err
	}
	a, err := t.api.GetApplication(ctx, ref)
	if err != nil {
		return nil, historyOutput{}, wrap(err)
	}
	h := append([]client.HistoryEntry(nil), a.Status.History...)
	sort.Slice(h, func(i, j int) bool { return h[i].ID > h[j].ID })
	out := historyOutput{Application: ref.Name, Total: len(h)}
	if len(h) > limit {
		h = h[:limit]
	}
	if h == nil {
		h = []client.HistoryEntry{}
	}
	out.History, out.Count = h, len(h)

	return nil, out, nil
}

// ---- argocd_get_application_manifests ----

const maxManifestBytes = 1 << 20 // keep responses usable by an LLM

type manifestsInput struct {
	appInput
	Revision string `json:"revision,omitempty" jsonschema:"Git revision, tag or Helm chart version. Defaults to the application's target revision."`
	Kind     string `json:"kind,omitempty" jsonschema:"Only manifests of this kind, e.g. Deployment (case-insensitive)."`
	Limit    int    `json:"limit,omitempty" jsonschema:"Maximum manifests to return (default 100, max 500)."`
}

type manifestsOutput struct {
	Application string           `json:"application"`
	Revision    string           `json:"revision,omitempty"`
	SourceType  string           `json:"sourceType,omitempty"`
	Total       int              `json:"total"`
	Count       int              `json:"count"`
	Truncated   bool             `json:"truncated"`
	Manifests   []map[string]any `json:"manifests"`
}

func (t *Tools) getManifests(ctx context.Context, _ *mcp.CallToolRequest, in manifestsInput) (*mcp.CallToolResult, manifestsOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, manifestsOutput{}, err
	}
	if err := toolkit.Text("revision", in.Revision, 255); err != nil {
		return nil, manifestsOutput{}, err
	}
	if strings.ContainsAny(in.Revision, " \t") {
		return nil, manifestsOutput{}, toolkit.Invalid("revision must not contain whitespace")
	}
	if err := toolkit.Text("kind", in.Kind, 253); err != nil {
		return nil, manifestsOutput{}, err
	}
	limit, err := toolkit.Limit("limit", in.Limit, 100, 500)
	if err != nil {
		return nil, manifestsOutput{}, err
	}

	m, err := t.api.GetManifests(ctx, ref, in.Revision)
	if err != nil {
		return nil, manifestsOutput{}, wrap(err)
	}
	out := manifestsOutput{Application: ref.Name, Revision: m.Revision, SourceType: m.SourceType, Manifests: []map[string]any{}}
	size := 0
	for _, raw := range m.Manifests {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			continue
		}
		if in.Kind != "" {
			if k, _ := obj["kind"].(string); !strings.EqualFold(k, in.Kind) {
				continue
			}
		}
		out.Total++
		if len(out.Manifests) >= limit || size+len(raw) > maxManifestBytes {
			out.Truncated = true

			continue
		}
		size += len(raw)
		out.Manifests = append(out.Manifests, obj)
	}
	out.Count = len(out.Manifests)

	return nil, out, nil
}

// ---- argocd_get_application_events ----

type eventsInput struct {
	appInput
	ResourceNamespace string `json:"resourceNamespace,omitempty" jsonschema:"Only events for a resource in this namespace (use with resourceName)."`
	ResourceName      string `json:"resourceName,omitempty" jsonschema:"Only events for this resource name."`
	ResourceUID       string `json:"resourceUID,omitempty" jsonschema:"Only events for the resource with this UID."`
	Limit             int    `json:"limit,omitempty" jsonschema:"Maximum events to return (default 50, max 500)."`
}

type eventsOutput struct {
	Application string         `json:"application"`
	Total       int            `json:"total"`
	Count       int            `json:"count"`
	Events      []client.Event `json:"events"`
}

func (t *Tools) getEvents(ctx context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, eventsOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, eventsOutput{}, err
	}
	if err := toolkit.OptionalNamespace("resourceNamespace", in.ResourceNamespace); err != nil {
		return nil, eventsOutput{}, err
	}
	if err := toolkit.OptionalK8sName("resourceName", in.ResourceName); err != nil {
		return nil, eventsOutput{}, err
	}
	if err := toolkit.Text("resourceUID", in.ResourceUID, 64); err != nil {
		return nil, eventsOutput{}, err
	}
	limit, err := toolkit.Limit("limit", in.Limit, 50, 500)
	if err != nil {
		return nil, eventsOutput{}, err
	}

	ev, err := t.api.ListEvents(ctx, ref, client.EventFilter{
		ResourceNamespace: in.ResourceNamespace, ResourceName: in.ResourceName, ResourceUID: in.ResourceUID,
	})
	if err != nil {
		return nil, eventsOutput{}, wrap(err)
	}
	sort.SliceStable(ev, func(i, j int) bool { return lastSeen(ev[i]) > lastSeen(ev[j]) })
	out := eventsOutput{Application: ref.Name, Total: len(ev)}
	if len(ev) > limit {
		ev = ev[:limit]
	}
	if ev == nil {
		ev = []client.Event{}
	}
	out.Events, out.Count = ev, len(ev)

	return nil, out, nil
}

// lastSeen returns an RFC3339 timestamp; lexical order equals time order.
func lastSeen(e client.Event) string {
	for _, t := range []string{e.LastTimestamp, e.EventTime, e.FirstTimestamp} {
		if t != "" {
			return t
		}
	}

	return ""
}
