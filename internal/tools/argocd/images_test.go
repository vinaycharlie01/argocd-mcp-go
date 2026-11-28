package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

var (
	dgNew = "sha256:" + strings.Repeat("a1", 32)
	dgOld = "sha256:" + strings.Repeat("b2", 32)
)

const repo = "registry.example.com/team/payments"

func pod(image, digest string) map[string]any {
	var p map[string]any
	_ = json.Unmarshal([]byte(fmt.Sprintf(`{
	  "spec":{"containers":[{"name":"app","image":%q},{"name":"proxy","image":"envoyproxy/envoy:v1.30"}],
	          "initContainers":[{"name":"migrate","image":%q}]},
	  "status":{"containerStatuses":[
	      {"name":"app","imageID":"docker-pullable://%s@%s"},
	      {"name":"proxy","imageID":"docker://sha256:%s"}],
	    "initContainerStatuses":[{"name":"migrate","imageID":"%s@%s"}]}}`,
		image, image, repo, digest, strings.Repeat("c", 64), repo, digest)), &p)

	return p
}

func imagesFixture(pods map[string]map[string]any, desiredTag string) *fakeAPI {
	tree := &client.ResourceTree{}
	_ = json.Unmarshal([]byte(`{"nodes":[
	  {"kind":"Deployment","group":"apps","namespace":"pay","name":"payments","uid":"d1"},
	  {"kind":"ReplicaSet","group":"apps","namespace":"pay","name":"payments-new","uid":"rs1","parentRefs":[{"kind":"Deployment","group":"apps","name":"payments","uid":"d1"}]},
	  {"kind":"ReplicaSet","group":"apps","namespace":"pay","name":"payments-old","uid":"rs2","parentRefs":[{"kind":"Deployment","group":"apps","name":"payments","uid":"d1"}]},
	  {"kind":"Pod","namespace":"pay","name":"p1","uid":"p1","parentRefs":[{"kind":"ReplicaSet","group":"apps","name":"payments-new","uid":"rs1"}]},
	  {"kind":"Pod","namespace":"pay","name":"p2","uid":"p2","parentRefs":[{"kind":"ReplicaSet","group":"apps","name":"payments-new","uid":"rs1"}]},
	  {"kind":"Pod","namespace":"pay","name":"p3","uid":"p3","parentRefs":[{"kind":"ReplicaSet","group":"apps","name":"payments-old","uid":"rs2"}]},
	  {"kind":"Service","namespace":"pay","name":"payments"}]}`), tree)
	for name := range map[string]bool{"p1": true, "p2": true, "p3": true} {
		if _, ok := pods[name]; !ok {
			delete(pods, name)
		}
	}
	manifest := fmt.Sprintf(`{"kind":"Deployment","metadata":{"name":"payments"},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"%s:%s"}]}}}}`, repo, desiredTag)

	return &fakeAPI{tree: tree, pods: pods, man: &client.ManifestResponse{Manifests: []string{manifest, `{"kind":"Service"}`}}}
}

func findContainer(t *testing.T, out imagesOutput, name string) ContainerImages {
	t.Helper()
	for _, c := range out.Containers {
		if c.Container == name {
			return c
		}
	}
	t.Fatalf("container %s not found in %+v", name, out.Containers)

	return ContainerImages{}
}

func TestImagesRunningDesiredAndMixedRollout(t *testing.T) {
	api := imagesFixture(map[string]map[string]any{
		"p1": pod(repo+":1.4.2", dgNew), "p2": pod(repo+":1.4.2", dgNew), "p3": pod(repo+":1.4.1", dgOld),
	}, "1.4.2")
	var out imagesOutput
	call(t, session(t, api), "argocd_get_images", map[string]any{"name": "payments"}, &out)

	if out.PodsInspected != 3 || out.Verdict != "" {
		t.Fatalf("%+v", out)
	}
	app := findContainer(t, out, "app")
	if app.Workload != "Deployment/payments" || app.Repository != repo || app.Desired != repo+":1.4.2" {
		t.Fatalf("%+v", app)
	}
	if !app.MixedRollout || !app.Drift || len(app.Running) != 2 {
		t.Fatalf("expected mixed rollout + drift: %+v", app)
	}
	if r := app.Running[0]; r.Tag != "1.4.2" || r.Digest != dgNew || strings.Join(r.Pods, ",") != "p1,p2" {
		t.Fatalf("%+v", r)
	}
	proxy := findContainer(t, out, "proxy")
	if proxy.Repository != "docker.io/envoyproxy/envoy" || proxy.Running[0].Digest != "" {
		t.Fatalf("local image IDs must not be reported as digests: %+v", proxy)
	}
	if mig := findContainer(t, out, "migrate"); !mig.Init {
		t.Fatalf("%+v", mig)
	}
}

func TestImagesComparisonVerdicts(t *testing.T) {
	allNew := func() map[string]map[string]any {
		return map[string]map[string]any{"p1": pod(repo+":1.4.2", dgNew), "p2": pod(repo+":1.4.2", dgNew), "p3": pod(repo+":1.4.2", dgNew)}
	}
	cases := []struct {
		name     string
		pods     map[string]map[string]any
		desired  string
		expected []map[string]any
		verdict  string
	}{
		{"digest match", allNew(), "1.4.2", []map[string]any{{"image": repo + ":1.4.2", "digest": dgNew}}, ResultMatch},
		{"digest in ref", allNew(), "1.4.2", []map[string]any{{"image": repo + ":1.4.2@" + dgNew}}, ResultMatch},
		{"tag only", allNew(), "1.4.2", []map[string]any{{"image": repo + ":1.4.2"}}, ResultMatchTagOnly},
		{"tag re-pushed", allNew(), "1.4.2", []map[string]any{{"image": repo + ":1.4.2", "digest": dgOld}}, ResultDigestMismatch},
		{"not deployed", allNew(), "1.4.2", []map[string]any{{"image": repo + ":1.5.0", "digest": dgOld}}, ResultDifferentTag},
		{"mixed", map[string]map[string]any{"p1": pod(repo+":1.4.2", dgNew), "p2": pod(repo+":1.4.1", dgOld)}, "1.4.2",
			[]map[string]any{{"image": repo + ":1.4.2", "digest": dgNew}}, ResultMixedRollout},
		{"wrong repo", allNew(), "1.4.2", []map[string]any{{"image": "registry.example.com/team/other:1"}}, ResultNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out imagesOutput
			call(t, session(t, imagesFixture(tc.pods, tc.desired)), "argocd_get_images",
				map[string]any{"name": "payments", "expected": tc.expected}, &out)
			if out.Verdict != tc.verdict {
				t.Fatalf("verdict %s, want %s: %+v", out.Verdict, tc.verdict, out)
			}
		})
	}
}

func TestImagesDifferentTagHintMentionsGit(t *testing.T) {
	pods := map[string]map[string]any{"p1": pod(repo+":1.4.1", dgOld)}
	var out imagesOutput
	call(t, session(t, imagesFixture(pods, "1.5.0")), "argocd_get_images",
		map[string]any{"name": "payments", "expected": []map[string]any{{"image": repo + ":1.5.0"}}}, &out)
	app := findContainer(t, out, "app")
	if app.Comparison == nil || app.Comparison.Result != ResultDifferentTag || !strings.Contains(app.Comparison.Hint, "Git already has it") {
		t.Fatalf("%+v", app.Comparison)
	}
}

func TestImagesMaxPodsAndValidation(t *testing.T) {
	api := imagesFixture(map[string]map[string]any{"p1": pod(repo+":1", dgNew), "p2": pod(repo+":1", dgNew), "p3": pod(repo+":1", dgNew)}, "1")
	var out imagesOutput
	call(t, session(t, api), "argocd_get_images", map[string]any{"name": "payments", "maxPods": 2}, &out)
	if out.PodsInspected != 2 || out.PodsSkipped != 1 {
		t.Fatalf("%+v", out)
	}

	cs := session(t, &fakeAPI{})
	for _, args := range []map[string]any{
		{"name": "payments", "expected": []map[string]any{{"image": "not an image"}}},
		{"name": "payments", "expected": []map[string]any{{"image": repo + ":1", "digest": "sha256:short"}}},
		{"name": "payments", "expected": []map[string]any{{"image": repo + ":1@" + dgNew, "digest": dgOld}}},
		{"name": "payments", "maxPods": 1000},
	} {
		if c := errorCode(t, call(t, cs, "argocd_get_images", args, nil)); c != toolkit.CodeInvalidInput {
			t.Errorf("%v: %s", args, c)
		}
	}
}

func TestImagesAuthErrorsFailFast(t *testing.T) {
	api := imagesFixture(map[string]map[string]any{}, "1")
	api.err = nil
	cs := session(t, &forbiddenPods{fakeAPI: api})
	if c := errorCode(t, call(t, cs, "argocd_get_images", map[string]any{"name": "payments"}, nil)); c != toolkit.CodeForbidden {
		t.Fatalf("got %s", c)
	}
}

// forbiddenPods returns 403 for pod lookups only.
type forbiddenPods struct{ *fakeAPI }

func (f *forbiddenPods) GetResource(_ context.Context, _ client.AppRef, _ client.ResourceQuery) (map[string]any, error) {
	return nil, &client.APIError{Status: 403, Message: "permission denied"}
}
