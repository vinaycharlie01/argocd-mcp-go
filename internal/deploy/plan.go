// Package deploy implements the write use cases: change an application's
// image through Argo CD parameter overrides (no Git), and sync with an
// explicit confirmation step and health verification. It depends only on the
// API port below, so the logic is unit-testable without HTTP.
package deploy

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/imageref"
)

// Source types that support image overrides without Git.
const (
	SourceHelm      = "Helm"
	SourceKustomize = "Kustomize"
)

var (
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	helmParamRe = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]]{1,253}$`)

	// ErrNoOverridableSource: the source type cannot override images without Git.
	ErrNoOverridableSource = errors.New("no Helm or Kustomize source")
)

// ImageUpdate is a requested image change.
type ImageUpdate struct {
	Tag            string
	Repository     string // optional: new image repository
	SourceIndex    *int   // optional: which source of a multi-source app
	HelmTagParam   string // e.g. image.tag
	HelmRepoParam  string // e.g. image.repository
	KustomizeImage string // Kustomize image name to override (optional when unambiguous)
}

// Validate checks the request format.
func (u ImageUpdate) Validate() error {
	if !tagRe.MatchString(u.Tag) {
		return fmt.Errorf("tag %q is not a valid image tag (letters, digits, '_', '.', '-'; max 128)", u.Tag)
	}
	if u.Repository != "" {
		if r, ok := imageref.Parse(u.Repository); !ok || r.Tag != "" || r.Digest != "" {
			return fmt.Errorf("repository %q must be an image repository without tag or digest, e.g. registry.example.com/team/app", u.Repository)
		}
	}
	for name, v := range map[string]string{"helmTagParameter": u.HelmTagParam, "helmRepositoryParameter": u.HelmRepoParam} {
		if v != "" && !helmParamRe.MatchString(v) {
			return fmt.Errorf("%s %q is not a valid Helm parameter name", name, v)
		}
	}
	if u.KustomizeImage != "" {
		if _, ok := imageref.Parse(u.KustomizeImage); !ok {
			return fmt.Errorf("kustomizeImage %q is not a valid image name", u.KustomizeImage)
		}
	}
	if u.SourceIndex != nil && *u.SourceIndex < 0 {
		return errors.New("sourceIndex must be >= 0")
	}

	return nil
}

// Change is one field the plan changes.
type Change struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// Plan is the computed, minimal change to the Application.
type Plan struct {
	SourceIndex int      `json:"sourceIndex"`
	SourceType  string   `json:"sourceType"`
	Changes     []Change `json:"changes"`
	// Image is the image the change should make the app render: repository:tag
	// when the repository is known, otherwise ":tag".
	Image    string           `json:"image"`
	NoChange bool             `json:"noChange"`
	Ops      []client.PatchOp `json:"-"`
}

type source struct {
	index int
	path  string // JSON pointer, e.g. /spec/source or /spec/sources/1
	src   client.Source
	typ   string
}

// sources lists the application's sources with their JSON pointer and type.
func sources(app *client.Application) []source {
	var out []source
	if app.Spec.Source != nil {
		return append(out, source{0, "/spec/source", *app.Spec.Source, sourceType(*app.Spec.Source, app.Status.SourceType)})
	}
	for i, s := range app.Spec.Sources {
		t := ""
		if i < len(app.Status.SourceTypes) {
			t = app.Status.SourceTypes[i]
		}
		out = append(out, source{i, fmt.Sprintf("/spec/sources/%d", i), s, sourceType(s, t)})
	}

	return out
}

func sourceType(s client.Source, reported string) string {
	switch {
	case s.Helm != nil || s.Chart != "" || reported == SourceHelm:
		return SourceHelm
	case s.Kustomize != nil || reported == SourceKustomize:
		return SourceKustomize
	case reported != "":
		return reported
	case s.Ref != "" && s.Path == "":
		return "Ref"
	}

	return "Directory"
}

func pickSource(app *client.Application, idx *int) (source, error) {
	all := sources(app)
	var candidates []source
	var types []string
	for _, s := range all {
		types = append(types, fmt.Sprintf("#%d %s", s.index, s.typ))
		if s.typ == SourceHelm || s.typ == SourceKustomize {
			candidates = append(candidates, s)
		}
	}
	if idx != nil {
		if *idx >= len(all) {
			return source{}, fmt.Errorf("sourceIndex %d out of range: the application has %d source(s)", *idx, len(all))
		}
		s := all[*idx]
		if s.typ != SourceHelm && s.typ != SourceKustomize {
			return source{}, fmt.Errorf("%w: source #%d is %s; images can only be overridden for Helm or Kustomize sources without changing Git", ErrNoOverridableSource, s.index, s.typ)
		}

		return s, nil
	}
	switch len(candidates) {
	case 1:
		return candidates[0], nil
	case 0:
		return source{}, fmt.Errorf("%w (sources: %s); this application's image can only be changed in Git", ErrNoOverridableSource, strings.Join(types, ", "))
	default:
		return source{}, fmt.Errorf("the application has several Helm/Kustomize sources (%s); set sourceIndex", strings.Join(types, ", "))
	}
}

// PlanImageUpdate computes the minimal JSON Patch for the image change.
// Every other source setting, Helm parameter and Kustomize image is kept.
// The patch starts with a resourceVersion test, so it is rejected if the
// application changed after it was read.
func PlanImageUpdate(app *client.Application, u ImageUpdate) (*Plan, error) {
	s, err := pickSource(app, u.SourceIndex)
	if err != nil {
		return nil, err
	}
	p := &Plan{SourceIndex: s.index, SourceType: s.typ}
	test := client.PatchOp{Op: "test", Path: "/metadata/resourceVersion", Value: app.Metadata.ResourceVersion}

	switch s.typ {
	case SourceHelm:
		params := []client.HelmParameter{}
		if s.src.Helm != nil {
			params = append(params, s.src.Helm.Parameters...)
		}
		repo := currentParam(params, u.HelmRepoParam)
		params = setParam(p, params, u.HelmTagParam, u.Tag)
		if u.Repository != "" {
			params = setParam(p, params, u.HelmRepoParam, u.Repository)
			repo = u.Repository
		}
		p.Image = repo + ":" + u.Tag
		op := client.PatchOp{Op: "add", Path: s.path + "/helm/parameters", Value: params}
		if s.src.Helm == nil {
			op = client.PatchOp{Op: "add", Path: s.path + "/helm", Value: map[string]any{"parameters": params}}
		}
		p.Ops = []client.PatchOp{test, op}
	case SourceKustomize:
		images := []string{}
		if s.src.Kustomize != nil {
			images = append(images, s.src.Kustomize.Images...)
		}
		images, err = setKustomizeImage(p, images, u)
		if err != nil {
			return nil, err
		}
		op := client.PatchOp{Op: "add", Path: s.path + "/kustomize/images", Value: images}
		if s.src.Kustomize == nil {
			op = client.PatchOp{Op: "add", Path: s.path + "/kustomize", Value: map[string]any{"images": images}}
		}
		p.Ops = []client.PatchOp{test, op}
	}
	p.NoChange = len(p.Changes) == 0

	return p, nil
}

func currentParam(params []client.HelmParameter, name string) string {
	for _, x := range params {
		if x.Name == name {
			return x.Value
		}
	}

	return ""
}

// setParam updates (or appends) a parameter as a string (--set-string), so
// tags like 1.10 or 20240101 are not converted to numbers by Helm.
func setParam(p *Plan, params []client.HelmParameter, name, value string) []client.HelmParameter {
	for i := range params {
		if params[i].Name != name {
			continue
		}
		if params[i].Value != value || !params[i].ForceString {
			if params[i].Value != value {
				p.Changes = append(p.Changes, Change{Field: "helm.parameters[" + name + "]", Old: params[i].Value, New: value})
			}
			params[i].Value, params[i].ForceString = value, true
		}

		return params
	}
	p.Changes = append(p.Changes, Change{Field: "helm.parameters[" + name + "]", Old: "", New: value})

	return append(params, client.HelmParameter{Name: name, Value: value, ForceString: true})
}

// kustomizeName returns the image name a Kustomize override applies to
// ("name=newName:tag" -> name; "name:tag" -> name), matching Argo CD.
func kustomizeName(entry string) string {
	if name, _, ok := strings.Cut(entry, "="); ok {
		return name
	}
	if i := strings.Index(entry, "@"); i >= 0 {
		entry = entry[:i]
	}
	if i := strings.LastIndex(entry, ":"); i > strings.LastIndex(entry, "/") {
		entry = entry[:i]
	}

	return entry
}

func setKustomizeImage(p *Plan, images []string, u ImageUpdate) ([]string, error) {
	name := u.KustomizeImage
	if name == "" {
		switch {
		case len(images) == 1:
			name = kustomizeName(images[0])
		case u.Repository != "":
			name = u.Repository
		default:
			return nil, errors.New("set kustomizeImage: the image name to override as it appears in the manifests (e.g. registry.example.com/team/app)")
		}
	}

	target := u.Repository
	idx := -1
	for i, e := range images {
		if kustomizeName(e) == name {
			idx = i
			if target == "" {
				if _, newName, ok := strings.Cut(e, "="); ok {
					target = kustomizeName(newName)
				}
			}
		}
	}
	entry := name + ":" + u.Tag
	if target != "" && target != name {
		entry = name + "=" + target + ":" + u.Tag
	}
	if target == "" {
		target = name
	}
	p.Image = target + ":" + u.Tag

	if idx >= 0 {
		if images[idx] != entry {
			p.Changes = append(p.Changes, Change{Field: "kustomize.images[" + name + "]", Old: images[idx], New: entry})
			images[idx] = entry
		}

		return images, nil
	}
	p.Changes = append(p.Changes, Change{Field: "kustomize.images[" + name + "]", Old: "", New: entry})

	return append(images, entry), nil
}
