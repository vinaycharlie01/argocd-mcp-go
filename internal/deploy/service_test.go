package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
)

// fakeAPI serves a sequence of application states for GetApplication.
type fakeAPI struct {
	mu        sync.Mutex
	states    []string // JSON per GetApplication call; the last one repeats
	calls     int
	getErrs   []error
	patched   [][]client.PatchOp
	patchErr  error
	synced    []client.SyncOptions
	syncErr   error
	manifests []string
}

func (f *fakeAPI) GetApplication(_ context.Context, _ client.AppRef) (*client.Application, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.getErrs) && f.getErrs[i] != nil {
		return nil, f.getErrs[i]
	}
	if i >= len(f.states) {
		i = len(f.states) - 1
	}
	var a client.Application
	if err := json.Unmarshal([]byte(f.states[i]), &a); err != nil {
		panic(err)
	}

	return &a, nil
}

func (f *fakeAPI) PatchApplication(_ context.Context, _ client.AppRef, ops []client.PatchOp) (*client.Application, error) {
	f.patched = append(f.patched, ops)

	return &client.Application{}, f.patchErr
}

func (f *fakeAPI) SyncApplication(_ context.Context, _ client.AppRef, o client.SyncOptions) (*client.Application, error) {
	f.synced = append(f.synced, o)

	return &client.Application{}, f.syncErr
}

func (f *fakeAPI) GetManifests(context.Context, client.AppRef, string) (*client.ManifestResponse, error) {
	return &client.ManifestResponse{Manifests: f.manifests}, nil
}

const helmApp = `{"metadata":{"name":"web","resourceVersion":"5"},"spec":{"project":"team-a",
  "source":{"repoURL":"r","chart":"web","helm":{"parameters":[{"name":"image.repository","value":"registry.example.com/team/web"},{"name":"image.tag","value":"1.0"}]}}%s},
  "status":{"sync":{"status":"Synced"},"health":{"status":"Healthy"},"summary":{"images":["registry.example.com/team/web:1.0"]}%s}}`

func app(syncPolicy, status string) string {
	return strings.Replace(strings.Replace(helmApp, "%s", syncPolicy, 1), "%s", status, 1)
}

func deployment(image string) string {
	return `{"kind":"Deployment","spec":{"template":{"spec":{"containers":[{"name":"app","image":"` + image + `"}]}}}}`
}

func newSvc(api API, cfg Config) *Service {
	cfg.PollInterval = time.Millisecond
	s := NewService(api, cfg)

	return s
}

var ref = client.AppRef{Name: "web"}

func TestUpdateImageManualSync(t *testing.T) {
	api := &fakeAPI{states: []string{app("", "")}, manifests: []string{deployment("registry.example.com/team/web:1.1")}}
	res, err := newSvc(api, Config{}).UpdateImage(context.Background(), ref, ImageUpdate{Tag: "1.1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "UPDATED" || len(api.patched) != 1 || res.RenderedVerified == nil || !*res.RenderedVerified || !strings.Contains(res.NextStep, "argocd_sync_application") {
		t.Fatalf("%+v", res)
	}
}

func TestUpdateImageWarnsWhenRenderUnchanged(t *testing.T) {
	api := &fakeAPI{states: []string{app("", "")}, manifests: []string{deployment("registry.example.com/team/web:1.0")}}
	res, err := newSvc(api, Config{}).UpdateImage(context.Background(), ref, ImageUpdate{Tag: "1.1"}, "")
	if err != nil || *res.RenderedVerified || !strings.Contains(strings.Join(res.Warnings, " "), "parameter name may be wrong") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestUpdateImageAutoSyncNeedsConfirmation(t *testing.T) {
	auto := app(`,"syncPolicy":{"automated":{"prune":false}}`, "")
	api := &fakeAPI{states: []string{auto}, manifests: []string{deployment("registry.example.com/team/web:1.1")}}
	s := newSvc(api, Config{})
	res, err := s.UpdateImage(context.Background(), ref, ImageUpdate{Tag: "1.1"}, "")
	if err != nil || res.Status != "CONFIRMATION_REQUIRED" || res.ConfirmationToken == "" || len(api.patched) != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := s.UpdateImage(context.Background(), ref, ImageUpdate{Tag: "9.9"}, res.ConfirmationToken); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("token must be bound to the planned change: %v", err)
	}
	res, _ = s.UpdateImage(context.Background(), ref, ImageUpdate{Tag: "1.1"}, "")
	res, err = s.UpdateImage(context.Background(), ref, ImageUpdate{Tag: "1.1"}, res.ConfirmationToken)
	if err != nil || res.Status != "UPDATED" || len(api.patched) != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestUpdateImagePolicy(t *testing.T) {
	ctx := context.Background()
	if _, err := newSvc(&fakeAPI{states: []string{app("", "")}}, Config{AllowedProjects: []string{"other"}}).UpdateImage(ctx, ref, ImageUpdate{Tag: "1"}, ""); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("project policy: %v", err)
	}
	owned := strings.Replace(app("", ""), `"resourceVersion":"5"`, `"resourceVersion":"5","ownerReferences":[{"kind":"ApplicationSet","name":"all-apps"}]`, 1)
	if _, err := newSvc(&fakeAPI{states: []string{owned}}, Config{}).UpdateImage(ctx, ref, ImageUpdate{Tag: "1"}, ""); !errors.Is(err, ErrManaged) {
		t.Errorf("appset: %v", err)
	}
	running := app("", `,"operationState":{"phase":"Running","startedAt":"t1"}`)
	if _, err := newSvc(&fakeAPI{states: []string{running}}, Config{}).UpdateImage(ctx, ref, ImageUpdate{Tag: "1"}, ""); !errors.Is(err, ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	api := &fakeAPI{states: []string{app("", "")}, patchErr: &client.APIError{Status: 500, Message: "error applying patch: testing value /metadata/resourceVersion failed: test failed"}}
	if _, err := newSvc(api, Config{}).UpdateImage(ctx, ref, ImageUpdate{Tag: "1"}, ""); !errors.Is(err, ErrConcurrentChange) {
		t.Errorf("concurrent: %v", err)
	}
	api = &fakeAPI{states: []string{app("", "")}}
	if res, err := newSvc(api, Config{}).UpdateImage(ctx, ref, ImageUpdate{Tag: "1.0"}, ""); err != nil || res.Status != "UNCHANGED" || len(api.patched) != 0 {
		t.Errorf("unchanged: %v %+v", err, res)
	}
}

func TestSyncPreviewThenConfirmAndVerify(t *testing.T) {
	before := app("", `,"sync":{"status":"OutOfSync"},"resources":[{"kind":"Deployment","name":"web","status":"OutOfSync"}],"operationState":{"phase":"Succeeded","startedAt":"old"}`)
	running := app("", `,"operationState":{"phase":"Running","startedAt":"new"}`)
	progressing := app("", `,"health":{"status":"Progressing"},"operationState":{"phase":"Succeeded","startedAt":"new"}`)
	healthy := app("", `,"summary":{"images":["registry.example.com/team/web:1.1"]},"operationState":{"phase":"Succeeded","startedAt":"new"}`)
	// The JSON keys repeat, so the later "status" fields override the template's.
	api := &fakeAPI{states: []string{before}}
	s := newSvc(api, Config{})

	pre, err := s.Sync(context.Background(), ref, SyncRequest{})
	if err != nil || pre.Status != "CONFIRMATION_REQUIRED" || pre.Preview == nil || len(pre.Preview.OutOfSync) != 1 || len(api.synced) != 0 {
		t.Fatalf("%v %+v", err, pre)
	}
	if _, err := s.Sync(context.Background(), ref, SyncRequest{Prune: true, Token: pre.ConfirmationToken}); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("token must be bound to prune=false: %v", err)
	}

	pre, _ = s.Sync(context.Background(), ref, SyncRequest{})
	api.states = []string{before, running, progressing, healthy}
	api.calls = 0
	res, err := s.Sync(context.Background(), ref, SyncRequest{Token: pre.ConfirmationToken, ExpectedImage: "registry.example.com/team/web:1.1", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != OutcomeHealthy || len(api.synced) != 1 || api.synced[0].Prune || res.ImageVerified == nil || !*res.ImageVerified {
		t.Fatalf("%+v %+v", res, res.Outcome)
	}
}

func TestSyncOutcomes(t *testing.T) {
	before := app("", `,"operationState":{"phase":"Succeeded","startedAt":"old"}`)
	cases := map[string]struct {
		states []string
		want   string
	}{
		"stale old success is ignored then failure": {[]string{before, before,
			app("", `,"operationState":{"phase":"Failed","startedAt":"new","message":"one or more objects failed to apply","syncResult":{"resources":[{"kind":"Deployment","name":"web","status":"SyncFailed","message":"field is immutable"}]}}`)}, OutcomeSyncFailed},
		"degraded twice": {[]string{before, app("", `,"health":{"status":"Degraded"},"operationState":{"phase":"Succeeded","startedAt":"new"}`)}, OutcomeDegraded},
		"timeout":        {[]string{before, app("", `,"operationState":{"phase":"Running","startedAt":"new"}`)}, OutcomeTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{states: []string{before}}
			s := newSvc(api, Config{})
			pre, _ := s.Sync(context.Background(), ref, SyncRequest{})
			api.states, api.calls = tc.states, 0
			res, err := s.Sync(context.Background(), ref, SyncRequest{Token: pre.ConfirmationToken, Timeout: 50 * time.Millisecond})
			if err != nil || res.Status != tc.want {
				t.Fatalf("%v %+v %+v", err, res, res.Outcome)
			}
			if tc.want == OutcomeSyncFailed && (len(res.Outcome.FailedResources) != 1 || !strings.Contains(res.NextStep, "roll back")) {
				t.Fatalf("%+v", res.Outcome)
			}
		})
	}
}

func TestSyncErrors(t *testing.T) {
	ctx := context.Background()
	busyApp := app("", `,"operationState":{"phase":"Running","startedAt":"x"}`)
	if _, err := newSvc(&fakeAPI{states: []string{busyApp}}, Config{}).Sync(ctx, ref, SyncRequest{}); !errors.Is(err, ErrBusy) {
		t.Errorf("busy: %v", err)
	}
	if _, err := newSvc(&fakeAPI{states: []string{app("", "")}}, Config{}).Sync(ctx, ref, SyncRequest{Timeout: time.Hour}); err == nil {
		t.Error("timeout above max accepted")
	}
	if _, err := newSvc(&fakeAPI{states: []string{app("", "")}}, Config{}).Sync(ctx, ref, SyncRequest{ExpectedImage: "no-tag"}); err == nil {
		t.Error("expectedImage without tag accepted")
	}
	// Permission errors from the sync call surface unchanged.
	api := &fakeAPI{states: []string{app("", "")}, syncErr: &client.APIError{Status: 403, Message: "permission denied: applications, sync"}}
	s := newSvc(api, Config{})
	pre, _ := s.Sync(ctx, ref, SyncRequest{})
	if _, err := s.Sync(ctx, ref, SyncRequest{Token: pre.ConfirmationToken}); !errors.Is(err, client.ErrForbidden) {
		t.Errorf("forbidden: %v", err)
	}
	// Transient read errors while waiting are retried; auth errors stop.
	api = &fakeAPI{states: []string{app("", "")}}
	s = newSvc(api, Config{})
	pre, _ = s.Sync(ctx, ref, SyncRequest{})
	api.calls, api.getErrs = 0, []error{nil, errors.New("connection reset"), &client.APIError{Status: 401}}
	if _, err := s.Sync(ctx, ref, SyncRequest{Token: pre.ConfirmationToken, Timeout: time.Second}); !errors.Is(err, client.ErrUnauthorized) {
		t.Errorf("auth during wait: %v", err)
	}
}

func TestStatusHidesNonImageParameters(t *testing.T) {
	raw := strings.Replace(app("", ""), `{"name":"image.tag","value":"1.0"}`, `{"name":"image.tag","value":"1.0"},{"name":"db.password","value":"s3cret"}`, 1)
	st, err := newSvc(&fakeAPI{states: []string{raw}}, Config{}).Status(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(st)
	if strings.Contains(string(b), "s3cret") || st.Sources[0].OtherParameters != 1 || len(st.Sources[0].ImageParameters) != 2 {
		t.Fatalf("%s", b)
	}
}
