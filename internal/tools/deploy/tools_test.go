package deploy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	core "github.com/vinaycharlie01/argocd-mcp-go/internal/deploy"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

type fakeAPI struct {
	states   []string
	calls    int
	getErr   error
	patched  int
	patchErr error
	synced   int
	syncErr  error
}

func (f *fakeAPI) GetApplication(context.Context, client.AppRef) (*client.Application, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	i := min(f.calls, len(f.states)-1)
	f.calls++
	var a client.Application
	_ = json.Unmarshal([]byte(f.states[i]), &a)

	return &a, nil
}

func (f *fakeAPI) PatchApplication(context.Context, client.AppRef, []client.PatchOp) (*client.Application, error) {
	f.patched++

	return &client.Application{}, f.patchErr
}

func (f *fakeAPI) SyncApplication(context.Context, client.AppRef, client.SyncOptions) (*client.Application, error) {
	f.synced++

	return &client.Application{}, f.syncErr
}

func (f *fakeAPI) GetManifests(context.Context, client.AppRef, string) (*client.ManifestResponse, error) {
	return &client.ManifestResponse{Manifests: []string{`{"kind":"Deployment","spec":{"template":{"spec":{"containers":[{"name":"a","image":"registry.example.com/team/web:2.0"}]}}}}`}}, nil
}

const base = `{"metadata":{"name":"web","resourceVersion":"1"},"spec":{"project":"p","source":{"repoURL":"r","chart":"web",
  "helm":{"parameters":[{"name":"image.repository","value":"registry.example.com/team/web"},{"name":"image.tag","value":"1.0"}]}}},
  "status":{"sync":{"status":"OutOfSync"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded","startedAt":"old"}}}`

const done = `{"metadata":{"name":"web"},"spec":{"project":"p"},"status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},
  "summary":{"images":["registry.example.com/team/web:2.0"]},"operationState":{"phase":"Succeeded","startedAt":"new"}}}`

func session(t *testing.T, api core.API) *mcp.ClientSession {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "t"}, nil)
	New(core.NewService(api, core.Config{PollInterval: time.Millisecond})).Register(s)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := s.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}

	return res, out
}

func code(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatal("expected error")
	}
	var env struct {
		Error toolkit.ToolError `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &env); err != nil {
		t.Fatalf("not an error envelope: %v", res.Content[0])
	}

	return env.Error.Code
}

func TestExactlyThreeTools(t *testing.T) {
	res, err := session(t, &fakeAPI{states: []string{base}}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.ToolAnnotations{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl.Annotations
	}
	if len(got) != 3 {
		t.Fatalf("tools: %v", got)
	}
	if !got["argocd_get_application_status"].ReadOnlyHint {
		t.Error("status must be read-only")
	}
	for _, n := range []string{"update_application_image", "argocd_sync_application"} {
		if a := got[n]; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
			t.Errorf("%s must be marked destructive", n)
		}
	}
}

func TestUpdateThenConfirmedSync(t *testing.T) {
	api := &fakeAPI{states: []string{base}}
	cs := session(t, api)

	_, st := call(t, cs, "argocd_get_application_status", map[string]any{"name": "web"})
	if st["syncStatus"] != "OutOfSync" {
		t.Fatalf("%v", st)
	}
	_, up := call(t, cs, "update_application_image", map[string]any{"name": "web", "tag": "2.0"})
	if up["status"] != "UPDATED" || up["renderedImageVerified"] != true || api.patched != 1 {
		t.Fatalf("%v", up)
	}

	_, pre := call(t, cs, "argocd_sync_application", map[string]any{"name": "web"})
	if pre["status"] != "CONFIRMATION_REQUIRED" || api.synced != 0 {
		t.Fatalf("sync must not run without confirmation: %v", pre)
	}
	api.states, api.calls = []string{base, done}, 0
	_, res := call(t, cs, "argocd_sync_application", map[string]any{
		"name": "web", "confirmationToken": pre["confirmationToken"], "expectedImage": "registry.example.com/team/web:2.0", "timeoutSeconds": 5,
	})
	if res["status"] != core.OutcomeHealthy || res["expectedImageRunning"] != true || api.synced != 1 {
		t.Fatalf("%v", res)
	}
}

func TestValidationAndErrorCodes(t *testing.T) {
	cs := session(t, &fakeAPI{states: []string{base}})
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"update_application_image", map[string]any{"name": "web", "tag": "bad tag"}},
		{"update_application_image", map[string]any{"name": "Bad_Name", "tag": "1"}},
		{"update_application_image", map[string]any{"name": "web", "tag": "1", "repository": "repo:with-tag"}},
		{"update_application_image", map[string]any{"name": "web", "tag": "1", "helmTagParameter": "a b"}},
		{"argocd_sync_application", map[string]any{"name": "web", "timeoutSeconds": 5000}},
		{"argocd_sync_application", map[string]any{"name": "web", "expectedImage": "no-tag"}},
	} {
		if c := code(t, mustErr(t, cs, tc.tool, tc.args)); c != toolkit.CodeInvalidInput {
			t.Errorf("%s %v: %s", tc.tool, tc.args, c)
		}
	}
	if c := code(t, mustErr(t, cs, "argocd_sync_application", map[string]any{"name": "web", "confirmationToken": "forged"})); c != toolkit.CodeConfirmation {
		t.Errorf("forged token: %s", c)
	}

	for err, want := range map[error]string{
		&client.APIError{Status: 403, Message: "permission denied: applications, update"}: toolkit.CodeForbidden,
		&client.APIError{Status: 401}: toolkit.CodeUnauthorized,
		&client.APIError{Status: 400, Code: 9, Message: "another operation is already in progress"}: toolkit.CodeConflict,
	} {
		cs := session(t, &fakeAPI{states: []string{base}, patchErr: err})
		if c := code(t, mustErr(t, cs, "update_application_image", map[string]any{"name": "web", "tag": "2.0"})); c != want {
			t.Errorf("%v: %s want %s", err, c, want)
		}
	}
	cs = session(t, &fakeAPI{states: []string{`{"metadata":{"name":"web"},"spec":{"project":"p","source":{"repoURL":"r","path":"k8s"}},"status":{"sourceType":"Directory"}}`}})
	if c := code(t, mustErr(t, cs, "update_application_image", map[string]any{"name": "web", "tag": "2.0"})); c != toolkit.CodePreconditionFailed {
		t.Errorf("directory source: %s", c)
	}
}

func mustErr(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, _ := call(t, cs, tool, args)

	return res
}
