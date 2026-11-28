package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
)

func appFrom(t *testing.T, raw string) *client.Application {
	t.Helper()
	var a client.Application
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatal(err)
	}

	return &a
}

func opsJSON(t *testing.T, p *Plan) string {
	t.Helper()
	b, _ := json.Marshal(p.Ops)

	return string(b)
}

func helmUpdate(tag string) ImageUpdate {
	return ImageUpdate{Tag: tag, HelmTagParam: "image.tag", HelmRepoParam: "image.repository"}
}

func TestPlanHelmPreservesParameters(t *testing.T) {
	app := appFrom(t, `{"metadata":{"name":"web","resourceVersion":"41"},
	  "spec":{"source":{"repoURL":"https://git.example.com/charts","chart":"web","helm":{"parameters":[
	    {"name":"replicaCount","value":"3"},{"name":"image.repository","value":"registry.example.com/team/web"},
	    {"name":"image.tag","value":"1.4.1"},{"name":"db.password","value":"s3cret"}],"valueFiles":["values-qa.yaml"]}}}}`)
	p, err := PlanImageUpdate(app, helmUpdate("1.10"))
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceType != SourceHelm || p.Image != "registry.example.com/team/web:1.10" || p.NoChange {
		t.Fatalf("%+v", p)
	}
	if len(p.Changes) != 1 || p.Changes[0] != (Change{Field: "helm.parameters[image.tag]", Old: "1.4.1", New: "1.10"}) {
		t.Fatalf("changes %+v", p.Changes)
	}
	got := opsJSON(t, p)
	want := `[{"op":"test","path":"/metadata/resourceVersion","value":"41"},{"op":"add","path":"/spec/source/helm/parameters","value":[` +
		`{"name":"replicaCount","value":"3"},{"name":"image.repository","value":"registry.example.com/team/web"},` +
		`{"name":"image.tag","value":"1.10","forceString":true},{"name":"db.password","value":"s3cret"}]}]`
	if got != want {
		t.Fatalf("ops\n got %s\nwant %s", got, want)
	}
	// valueFiles are untouched because only helm/parameters is patched.
	if strings.Contains(got, "valueFiles") {
		t.Fatal("must not touch other helm fields")
	}
}

func TestPlanHelmAddsWhenMissing(t *testing.T) {
	app := appFrom(t, `{"metadata":{"resourceVersion":"1"},"spec":{"source":{"repoURL":"r","path":"chart"}},"status":{"sourceType":"Helm"}}`)
	u := helmUpdate("2.0")
	u.Repository = "registry.example.com/team/web"
	p, err := PlanImageUpdate(app, u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opsJSON(t, p), `{"op":"add","path":"/spec/source/helm","value":{"parameters":[{"name":"image.tag","value":"2.0","forceString":true},{"name":"image.repository","value":"registry.example.com/team/web","forceString":true}]}}`) {
		t.Fatal(opsJSON(t, p))
	}
	if len(p.Changes) != 2 || p.Image != "registry.example.com/team/web:2.0" {
		t.Fatalf("%+v", p)
	}
}

func TestPlanNoChange(t *testing.T) {
	app := appFrom(t, `{"metadata":{"resourceVersion":"1"},"spec":{"source":{"repoURL":"r","chart":"c","helm":{"parameters":[{"name":"image.tag","value":"5","forceString":true}]}}}}`)
	p, err := PlanImageUpdate(app, helmUpdate("5"))
	if err != nil || !p.NoChange || p.Image != ":5" {
		t.Fatalf("%v %+v", err, p)
	}
}

func TestPlanKustomize(t *testing.T) {
	app := appFrom(t, `{"metadata":{"resourceVersion":"9"},"spec":{"source":{"repoURL":"r","path":"overlays/qa","kustomize":{"images":[
	  "registry.example.com/team/web:1.0","registry.example.com/team/worker=registry.example.com/team/worker-v2:3"]}}},"status":{"sourceType":"Kustomize"}}`)

	u := ImageUpdate{Tag: "1.1", KustomizeImage: "registry.example.com/team/web"}
	p, err := PlanImageUpdate(app, u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(opsJSON(t, p), `"value":["registry.example.com/team/web:1.1","registry.example.com/team/worker=registry.example.com/team/worker-v2:3"]`) {
		t.Fatal(opsJSON(t, p))
	}
	// A renamed image keeps its newName when only the tag changes.
	p, err = PlanImageUpdate(app, ImageUpdate{Tag: "4", KustomizeImage: "registry.example.com/team/worker"})
	if err != nil || p.Changes[0].New != "registry.example.com/team/worker=registry.example.com/team/worker-v2:4" || p.Image != "registry.example.com/team/worker-v2:4" {
		t.Fatalf("%v %+v", err, p)
	}
	// Ambiguous without kustomizeImage.
	if _, err := PlanImageUpdate(app, ImageUpdate{Tag: "4"}); err == nil || !strings.Contains(err.Error(), "kustomizeImage") {
		t.Fatalf("got %v", err)
	}
}

func TestPlanSourceSelection(t *testing.T) {
	multi := appFrom(t, `{"metadata":{"resourceVersion":"1"},"spec":{"sources":[
	  {"repoURL":"https://git.example.com/values","ref":"values"},
	  {"repoURL":"https://charts.example.com","chart":"web","helm":{"valueFiles":["$values/qa.yaml"]}}]}}`)
	p, err := PlanImageUpdate(multi, helmUpdate("7"))
	if err != nil || p.SourceIndex != 1 || !strings.Contains(opsJSON(t, p), `"/spec/sources/1/helm/parameters"`) {
		t.Fatalf("%v %+v %s", err, p, opsJSON(t, p))
	}
	zero := 0
	u := helmUpdate("7")
	u.SourceIndex = &zero
	if _, err := PlanImageUpdate(multi, u); !errors.Is(err, ErrNoOverridableSource) {
		t.Fatalf("ref source must be rejected: %v", err)
	}

	dir := appFrom(t, `{"spec":{"source":{"repoURL":"r","path":"k8s"}},"status":{"sourceType":"Directory"}}`)
	if _, err := PlanImageUpdate(dir, helmUpdate("7")); !errors.Is(err, ErrNoOverridableSource) || !strings.Contains(err.Error(), "Git") {
		t.Fatalf("got %v", err)
	}
	two := appFrom(t, `{"spec":{"sources":[{"repoURL":"a","chart":"x"},{"repoURL":"b","chart":"y"}]}}`)
	if _, err := PlanImageUpdate(two, helmUpdate("7")); err == nil || !strings.Contains(err.Error(), "sourceIndex") {
		t.Fatalf("got %v", err)
	}
}

func TestValidate(t *testing.T) {
	ok := []ImageUpdate{{Tag: "1.4.2"}, {Tag: "v1_rc-2"}, {Tag: "abc", Repository: "registry.example.com:5000/team/app"}}
	for _, u := range ok {
		if err := u.Validate(); err != nil {
			t.Errorf("%+v: %v", u, err)
		}
	}
	neg := -1
	bad := []ImageUpdate{
		{Tag: ""}, {Tag: "-x"}, {Tag: "a b"}, {Tag: strings.Repeat("a", 129)},
		{Tag: "1", Repository: "repo:tag"}, {Tag: "1", Repository: "bad repo"},
		{Tag: "1", HelmTagParam: "image tag"}, {Tag: "1", SourceIndex: &neg},
	}
	for _, u := range bad {
		if err := u.Validate(); err == nil {
			t.Errorf("%+v accepted", u)
		}
	}
}
