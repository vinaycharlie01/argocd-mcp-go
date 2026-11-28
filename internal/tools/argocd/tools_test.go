package argocd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

type fakeAPI struct {
	apps     []client.Application
	app      *client.Application
	tree     *client.ResourceTree
	man      *client.ManifestResponse
	events   []client.Event
	logs     *client.LogResult
	lastLogs client.LogOptions
	err      error
	calls    int
	lastRef  client.AppRef
	lastList client.ListOptions
}

func (f *fakeAPI) ListApplications(_ context.Context, o client.ListOptions) ([]client.Application, error) {
	f.calls++
	f.lastList = o

	return f.apps, f.err
}

func (f *fakeAPI) GetApplication(_ context.Context, r client.AppRef) (*client.Application, error) {
	f.calls++
	f.lastRef = r

	return f.app, f.err
}

func (f *fakeAPI) GetResourceTree(_ context.Context, r client.AppRef) (*client.ResourceTree, error) {
	f.calls++
	f.lastRef = r

	return f.tree, f.err
}

func (f *fakeAPI) GetManifests(_ context.Context, r client.AppRef, _ string) (*client.ManifestResponse, error) {
	f.calls++
	f.lastRef = r

	return f.man, f.err
}

func (f *fakeAPI) ListEvents(_ context.Context, r client.AppRef, _ client.EventFilter) ([]client.Event, error) {
	f.calls++
	f.lastRef = r

	return f.events, f.err
}

func (f *fakeAPI) GetLogs(_ context.Context, r client.AppRef, o client.LogOptions) (*client.LogResult, error) {
	f.calls++
	f.lastRef, f.lastLogs = r, o

	return f.logs, f.err
}

func session(t *testing.T, api API) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	New(api).Register(s)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}

	return res
}

func errorCode(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatal("expected error result")
	}
	var env struct {
		Error toolkit.ToolError `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &env); err != nil {
		t.Fatalf("error is not a JSON envelope: %v", res.Content[0])
	}

	return env.Error.Code
}

func app(name, project string) client.Application {
	var a client.Application
	a.Metadata.Name = name
	a.Spec.Project = project
	a.Spec.Source = &client.Source{RepoURL: "https://git.example.com/r", Path: name}
	a.Spec.SyncPolicy = json.RawMessage(`{"automated":{"prune":true}}`)
	a.Status.Sync.Status = "Synced"
	a.Status.Health.Status = "Healthy"

	return a
}

func TestAllToolsRegistered(t *testing.T) {
	cs := session(t, &fakeAPI{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s must be read-only", tl.Name)
		}
	}
	want := "argocd_get_application,argocd_get_application_events,argocd_get_application_history,argocd_get_application_manifests,argocd_get_application_resources,argocd_get_pod_logs,argocd_list_applications"
	if strings.Join(names, ",") != want {
		t.Fatalf("tools = %v", names)
	}
}

func TestListApplications(t *testing.T) {
	api := &fakeAPI{apps: []client.Application{app("web", "b"), app("api", "a"), app("worker", "a")}}
	cs := session(t, api)
	var out listOutput
	call(t, cs, "argocd_list_applications", map[string]any{"projects": []string{"a", "b"}, "search": "W"}, &out)
	if out.Count != 2 || out.Applications[0].Name != "worker" || out.Applications[1].Name != "web" || !out.Applications[0].AutoSync {
		t.Fatalf("%+v", out)
	}
	if len(api.lastList.Projects) != 2 {
		t.Fatalf("filters not passed: %+v", api.lastList)
	}
}

func TestInputValidationRejectsBeforeAPICall(t *testing.T) {
	api := &fakeAPI{}
	cs := session(t, api)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"argocd_get_application", map[string]any{"name": "Bad_Name"}},
		{"argocd_get_application", map[string]any{"name": "../../settings"}},
		{"argocd_get_application", map[string]any{"name": "ok", "appNamespace": "a.b"}},
		{"argocd_list_applications", map[string]any{"projects": []string{"BAD"}}},
		{"argocd_get_application_history", map[string]any{"name": "ok", "limit": 1000}},
		{"argocd_get_application_manifests", map[string]any{"name": "ok", "revision": "a b"}},
		{"argocd_get_application_events", map[string]any{"name": "ok", "resourceNamespace": "NS"}},
	}
	for _, tc := range cases {
		if code := errorCode(t, call(t, cs, tc.tool, tc.args, nil)); code != toolkit.CodeInvalidInput {
			t.Errorf("%s %v: code %s", tc.tool, tc.args, code)
		}
	}
	// Missing required "name" is rejected by the schema.
	if res := call(t, cs, "argocd_get_application", map[string]any{}, nil); !res.IsError {
		t.Error("missing name should fail")
	}
	if api.calls != 0 {
		t.Fatalf("API called %d times for invalid input", api.calls)
	}
}

func TestErrorCodes(t *testing.T) {
	for err, want := range map[error]string{
		&client.APIError{Status: 401}:               toolkit.CodeUnauthorized,
		&client.APIError{Status: 403, Message: "x"}: toolkit.CodeForbidden,
		&client.APIError{Status: 404, Message: "x"}: toolkit.CodeNotFound,
		&client.APIError{Status: 500, Message: "x"}: toolkit.CodeUpstream,
	} {
		cs := session(t, &fakeAPI{err: err})
		if got := errorCode(t, call(t, cs, "argocd_get_application", map[string]any{"name": "web"}, nil)); got != want {
			t.Errorf("%v: got %s want %s", err, got, want)
		}
	}
}

func TestGetApplicationDetails(t *testing.T) {
	a := app("web", "a")
	a.Status.Resources = []client.ResourceStatus{
		{Kind: "Deployment", Name: "web", Status: "OutOfSync"},
		{Kind: "Service", Name: "web", Status: "Synced"},
	}
	a.Status.History = make([]client.HistoryEntry, 3)
	cs := session(t, &fakeAPI{app: &a})
	var out AppDetails
	call(t, cs, "argocd_get_application", map[string]any{"name": "web", "project": "a"}, &out)
	if out.Name != "web" || out.ResourceCounts.Total != 2 || out.ResourceCounts.OutOfSync != 1 || len(out.ProblemResources) != 1 ||
		out.HistoryCount != 3 || len(out.Sources) != 1 || out.SyncPolicy["automated"] == nil {
		t.Fatalf("%+v", out)
	}
}

func TestResourcesMergeSyncAndFilter(t *testing.T) {
	a := app("web", "a")
	a.Status.Resources = []client.ResourceStatus{{Group: "apps", Kind: "Deployment", Namespace: "web", Name: "web", Status: "OutOfSync"}}
	tree := &client.ResourceTree{}
	_ = json.Unmarshal([]byte(`{"nodes":[
		{"group":"apps","kind":"Deployment","namespace":"web","name":"web","health":{"status":"Degraded"}},
		{"kind":"Pod","namespace":"web","name":"web-1","health":{"status":"Healthy"},"parentRefs":[{"kind":"ReplicaSet","name":"web-x"}],"info":[{"name":"Status Reason","value":"Running"}]}],
		"orphanedNodes":[{"kind":"ConfigMap","name":"old"}]}`), tree)
	cs := session(t, &fakeAPI{app: &a, tree: tree})

	var all resourcesOutput
	call(t, cs, "argocd_get_application_resources", map[string]any{"name": "web", "includeOrphaned": true}, &all)
	if all.Count != 3 || all.Resources[0].SyncStatus != "OutOfSync" || all.Resources[1].Parents[0] != "ReplicaSet/web-x" ||
		all.Resources[1].Info["Status Reason"] != "Running" || !all.Resources[2].Orphaned {
		t.Fatalf("%+v", all)
	}
	var degraded resourcesOutput
	call(t, cs, "argocd_get_application_resources", map[string]any{"name": "web", "health": "degraded"}, &degraded)
	if degraded.Count != 1 || degraded.Resources[0].Kind != "Deployment" {
		t.Fatalf("%+v", degraded)
	}
}

func TestHistoryNewestFirstWithLimit(t *testing.T) {
	a := app("web", "a")
	a.Status.History = []client.HistoryEntry{{ID: 1, Revision: "r1"}, {ID: 3, Revision: "r3"}, {ID: 2, Revision: "r2"}}
	cs := session(t, &fakeAPI{app: &a})
	var out historyOutput
	call(t, cs, "argocd_get_application_history", map[string]any{"name": "web", "limit": 2}, &out)
	if out.Total != 3 || out.Count != 2 || out.History[0].ID != 3 || out.History[1].ID != 2 {
		t.Fatalf("%+v", out)
	}
}

func TestManifestsKindFilterAndLimit(t *testing.T) {
	m := &client.ManifestResponse{Revision: "abc", Manifests: []string{
		`{"kind":"Deployment","metadata":{"name":"a"}}`,
		`{"kind":"Service","metadata":{"name":"a"}}`,
		`{"kind":"Deployment","metadata":{"name":"b"}}`,
		`not json`,
	}}
	cs := session(t, &fakeAPI{man: m})
	var out manifestsOutput
	call(t, cs, "argocd_get_application_manifests", map[string]any{"name": "web", "kind": "deployment", "limit": 1}, &out)
	if out.Total != 2 || out.Count != 1 || !out.Truncated || out.Revision != "abc" {
		t.Fatalf("%+v", out)
	}
}

func TestEventsNewestFirst(t *testing.T) {
	ev := []client.Event{
		{Reason: "old", LastTimestamp: "2026-01-01T00:00:00Z"},
		{Reason: "new", LastTimestamp: "2026-01-02T00:00:00Z"},
		{Reason: "micro", EventTime: "2026-01-01T12:00:00.000001Z"},
	}
	cs := session(t, &fakeAPI{events: ev})
	var out eventsOutput
	call(t, cs, "argocd_get_application_events", map[string]any{"name": "web"}, &out)
	if out.Count != 3 || out.Events[0].Reason != "new" || out.Events[1].Reason != "micro" {
		t.Fatalf("%+v", out)
	}
}

func TestPodLogs(t *testing.T) {
	api := &fakeAPI{logs: &client.LogResult{Lines: []client.LogLine{{Pod: "web-1", Content: "started"}}}}
	cs := session(t, api)
	var out logsOutput
	call(t, cs, "argocd_get_pod_logs", map[string]any{"name": "web", "kind": "Deployment", "group": "apps", "resourceName": "web", "previous": true}, &out)
	if out.Count != 1 || out.Target != "Deployment/web" || out.Lines[0].Content != "started" {
		t.Fatalf("%+v", out)
	}
	if o := api.lastLogs; o.TailLines != 200 || o.Kind != "Deployment" || o.Group != "apps" || !o.Previous || o.PodName != "" {
		t.Fatalf("options %+v", o)
	}
	call(t, cs, "argocd_get_pod_logs", map[string]any{"name": "web", "podName": "web-1", "tailLines": 50, "container": "app"}, &out)
	if api.lastLogs.PodName != "web-1" || api.lastLogs.TailLines != 50 || api.lastLogs.Kind != "" {
		t.Fatalf("options %+v", api.lastLogs)
	}
}

func TestPodLogsValidation(t *testing.T) {
	api := &fakeAPI{}
	cs := session(t, api)
	for _, args := range []map[string]any{
		{"name": "web"}, // no target
		{"name": "web", "podName": "p", "kind": "Deployment", "resourceName": "web"}, // both
		{"name": "web", "kind": "Deployment"},                                        // missing resourceName
		{"name": "web", "kind": "Deploy ment", "resourceName": "web"},
		{"name": "web", "podName": "P_1"},
		{"name": "web", "podName": "p", "tailLines": 99999},
		{"name": "web", "podName": "p", "sinceSeconds": -1},
		{"name": "web", "podName": "p", "container": "A/B"},
	} {
		if code := errorCode(t, call(t, cs, "argocd_get_pod_logs", args, nil)); code != toolkit.CodeInvalidInput {
			t.Errorf("%v: %s", args, code)
		}
	}
	if api.calls != 0 {
		t.Fatal("API must not be called for invalid input")
	}
}

func TestPodLogsForbidden(t *testing.T) {
	cs := session(t, &fakeAPI{err: &client.APIError{Status: 403, Message: "permission denied: logs, get"}})
	res := call(t, cs, "argocd_get_pod_logs", map[string]any{"name": "web", "podName": "p"}, nil)
	if code := errorCode(t, res); code != toolkit.CodeForbidden || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "logs, get") {
		t.Fatalf("%s %v", code, res.Content[0])
	}
}
