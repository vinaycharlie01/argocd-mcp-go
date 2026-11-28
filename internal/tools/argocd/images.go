package argocd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/imageref"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

// Comparison results, worst last.
const (
	ResultMatch          = "MATCH"
	ResultMatchTagOnly   = "MATCH_TAG_ONLY"
	ResultMixedRollout   = "MIXED_ROLLOUT"
	ResultDigestMismatch = "TAG_MATCH_DIGEST_DIFFERENT"
	ResultDifferentTag   = "DIFFERENT_TAG"
	ResultNotFound       = "NOT_FOUND"
)

var severity = map[string]int{
	ResultMatch: 0, ResultMatchTagOnly: 1, ResultMixedRollout: 2,
	ResultDigestMismatch: 3, ResultDifferentTag: 4, ResultNotFound: 5,
}

const (
	defaultMaxPods = 30
	maxMaxPods     = 200
	maxExpected    = 50
)

// ExpectedImage is an image a build produced.
type ExpectedImage struct {
	Image  string `json:"image" jsonschema:"Image reference as built, e.g. registry.example.com/team/app:1.4.2 (may include @sha256:...)."`
	Digest string `json:"digest,omitempty" jsonschema:"Registry digest sha256:... if known separately."`
}

type imagesInput struct {
	appInput
	Expected []ExpectedImage `json:"expected,omitempty" jsonschema:"Images to verify against what is running, e.g. the images from jenkins_get_build_images."`
	MaxPods  int             `json:"maxPods,omitempty" jsonschema:"Maximum pods to inspect (default 30, max 200)."`
}

// RunningImage is one distinct image a container runs, with the pods running it.
type RunningImage struct {
	Image  string   `json:"image"`
	Tag    string   `json:"tag,omitempty"`
	Digest string   `json:"digest,omitempty"`
	Pods   []string `json:"pods"`
}

// Comparison is the verdict for one container against one expected image.
type Comparison struct {
	Expected string `json:"expected"`
	Result   string `json:"result"`
	Hint     string `json:"hint,omitempty"`
}

// ContainerImages describes one container of one workload.
type ContainerImages struct {
	Workload     string         `json:"workload"`
	Namespace    string         `json:"namespace,omitempty"`
	Container    string         `json:"container"`
	Init         bool           `json:"init,omitempty"`
	Repository   string         `json:"repository"`
	Desired      string         `json:"desiredImage,omitempty"`
	Running      []RunningImage `json:"running"`
	MixedRollout bool           `json:"mixedRollout"`
	Drift        bool           `json:"drift"`
	Comparison   *Comparison    `json:"comparison,omitempty"`
}

type imagesOutput struct {
	Application   string            `json:"application"`
	Verdict       string            `json:"verdict,omitempty"`
	PodsInspected int               `json:"podsInspected"`
	PodsSkipped   int               `json:"podsSkipped"`
	Containers    []ContainerImages `json:"containers"`
	NotFound      []Comparison      `json:"notFound,omitempty"`
	Warnings      []string          `json:"warnings,omitempty"`
}

type containerKey struct{ workload, container string }

func (t *Tools) getImages(ctx context.Context, _ *mcp.CallToolRequest, in imagesInput) (*mcp.CallToolResult, imagesOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, imagesOutput{}, err
	}
	maxPods, err := toolkit.Limit("maxPods", in.MaxPods, defaultMaxPods, maxMaxPods)
	if err != nil {
		return nil, imagesOutput{}, err
	}
	expected, err := parseExpected(in.Expected)
	if err != nil {
		return nil, imagesOutput{}, err
	}

	tree, err := t.api.GetResourceTree(ctx, ref)
	if err != nil {
		return nil, imagesOutput{}, wrap(err)
	}
	out := imagesOutput{Application: ref.Name, Containers: []ContainerImages{}}
	byKey := map[containerKey]*ContainerImages{}

	// Running images from live pods.
	byUID := map[string]client.ResourceNode{}
	for _, n := range tree.Nodes {
		if n.UID != "" {
			byUID[n.UID] = n
		}
	}
	var podErr error
	for _, n := range tree.Nodes {
		if n.Kind != "Pod" || n.Group != "" {
			continue
		}
		if out.PodsInspected >= maxPods {
			out.PodsSkipped++

			continue
		}
		pod, err := t.api.GetResource(ctx, ref, client.ResourceQuery{Namespace: n.Namespace, Name: n.Name, Version: "v1", Kind: "Pod"})
		if err != nil {
			if errors.Is(err, client.ErrUnauthorized) || errors.Is(err, client.ErrForbidden) {
				return nil, imagesOutput{}, wrap(err)
			}
			podErr = err
			out.Warnings = append(out.Warnings, fmt.Sprintf("pod %s/%s: %v", n.Namespace, n.Name, err))

			continue
		}
		out.PodsInspected++
		workload := workloadOf(n, byUID)
		for _, c := range podContainers(pod) {
			k := containerKey{workload, c.name}
			ci := byKey[k]
			if ci == nil {
				ci = &ContainerImages{Workload: workload, Namespace: n.Namespace, Container: c.name, Init: c.init, Running: []RunningImage{}}
				byKey[k] = ci
			}
			addRunning(ci, c.image, c.digest, n.Name)
		}
	}
	if out.PodsInspected == 0 && podErr != nil {
		return nil, imagesOutput{}, wrap(podErr)
	}

	// Desired images from the Git-rendered manifests (best effort).
	if m, err := t.api.GetManifests(ctx, ref, ""); err != nil {
		out.Warnings = append(out.Warnings, "desired images unavailable: "+err.Error())
	} else {
		for k, img := range desiredImages(m.Manifests) {
			ci := byKey[k]
			if ci == nil {
				ci = &ContainerImages{Workload: k.workload, Container: k.container, Running: []RunningImage{}}
				byKey[k] = ci
			}
			ci.Desired = img
		}
	}

	for _, ci := range byKey {
		finalize(ci)
		out.Containers = append(out.Containers, *ci)
	}
	sort.Slice(out.Containers, func(i, j int) bool {
		a, b := out.Containers[i], out.Containers[j]
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}

		return a.Container < b.Container
	})
	if len(expected) > 0 {
		compareAll(&out, expected)
	}

	return nil, out, nil
}

func parseExpected(in []ExpectedImage) ([]imageref.Ref, error) {
	if len(in) > maxExpected {
		return nil, toolkit.Invalid("at most %d expected images", maxExpected)
	}
	out := make([]imageref.Ref, 0, len(in))
	for i, e := range in {
		r, ok := imageref.Parse(e.Image)
		if !ok {
			return nil, toolkit.Invalid("expected[%d].image %q is not a valid image reference", i, e.Image)
		}
		if e.Digest != "" {
			d := strings.ToLower(strings.TrimSpace(e.Digest))
			if _, ok := imageref.Parse(r.Repository + "@" + d); !ok {
				return nil, toolkit.Invalid("expected[%d].digest must look like sha256:<64 hex>", i)
			}
			if r.Digest != "" && r.Digest != d {
				return nil, toolkit.Invalid("expected[%d] has two different digests", i)
			}
			r.Digest = d
		}
		out = append(out, r)
	}

	return out, nil
}

// workloadOf walks owner references up to the top-level workload, e.g.
// Pod -> ReplicaSet -> Deployment, and returns "Kind/name".
func workloadOf(n client.ResourceNode, byUID map[string]client.ResourceNode) string {
	kind, name := n.Kind, n.Name
	cur := n
	for range 10 { // bounded: owner chains are short
		if len(cur.ParentRefs) == 0 {
			break
		}
		p := cur.ParentRefs[0]
		kind, name = p.Kind, p.Name
		next, ok := byUID[p.UID]
		if !ok {
			break
		}
		cur = next
	}

	return kind + "/" + name
}

type podContainer struct {
	name, image, digest string
	init                bool
}

// podContainers extracts (name, spec image, registry digest) for every container.
func podContainers(pod map[string]any) []podContainer {
	spec, _ := pod["spec"].(map[string]any)
	status, _ := pod["status"].(map[string]any)
	digests := map[string]string{}
	for _, key := range []string{"containerStatuses", "initContainerStatuses"} {
		for _, s := range asList(status[key]) {
			name, _ := s["name"].(string)
			id, _ := s["imageID"].(string)
			digests[key+"/"+name] = imageref.DigestFromImageID(id)
		}
	}
	var out []podContainer
	for _, c := range []struct {
		specKey, statusKey string
		init               bool
	}{{"containers", "containerStatuses", false}, {"initContainers", "initContainerStatuses", true}} {
		for _, ct := range asList(spec[c.specKey]) {
			name, _ := ct["name"].(string)
			image, _ := ct["image"].(string)
			if name == "" || image == "" {
				continue
			}
			out = append(out, podContainer{name: name, image: image, digest: digests[c.statusKey+"/"+name], init: c.init})
		}
	}

	return out
}

func asList(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}

	return out
}

func addRunning(ci *ContainerImages, image, digest, pod string) {
	r, _ := imageref.Parse(image)
	if digest == "" {
		digest = r.Digest // image pinned by digest in the spec
	}
	if ci.Repository == "" {
		ci.Repository = r.Repository
	}
	for i := range ci.Running {
		if ci.Running[i].Image == image && ci.Running[i].Digest == digest {
			ci.Running[i].Pods = append(ci.Running[i].Pods, pod)

			return
		}
	}
	ci.Running = append(ci.Running, RunningImage{Image: image, Tag: r.Tag, Digest: digest, Pods: []string{pod}})
}

// podTemplatePaths are where workload kinds keep their pod spec.
var podTemplatePaths = map[string][]string{
	"Deployment":  {"spec", "template", "spec"},
	"StatefulSet": {"spec", "template", "spec"},
	"DaemonSet":   {"spec", "template", "spec"},
	"ReplicaSet":  {"spec", "template", "spec"},
	"Job":         {"spec", "template", "spec"},
	"Rollout":     {"spec", "template", "spec"},
	"CronJob":     {"spec", "jobTemplate", "spec", "template", "spec"},
	"Pod":         {"spec"},
}

// desiredImages returns the image Git wants for each workload container.
func desiredImages(manifests []string) map[containerKey]string {
	out := map[containerKey]string{}
	for _, raw := range manifests {
		obj, err := decodeObject(raw)
		if err != nil {
			continue
		}
		kind, _ := obj["kind"].(string)
		path, ok := podTemplatePaths[kind]
		if !ok {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		var spec any = obj
		for _, p := range path {
			m, _ := spec.(map[string]any)
			spec = m[p]
		}
		ps, _ := spec.(map[string]any)
		for _, key := range []string{"containers", "initContainers"} {
			for _, c := range asList(ps[key]) {
				cn, _ := c["name"].(string)
				img, _ := c["image"].(string)
				if cn != "" && img != "" {
					out[containerKey{kind + "/" + name, cn}] = img
				}
			}
		}
	}

	return out
}

func finalize(ci *ContainerImages) {
	sort.Slice(ci.Running, func(i, j int) bool { return len(ci.Running[i].Pods) > len(ci.Running[j].Pods) })
	for i := range ci.Running {
		sort.Strings(ci.Running[i].Pods)
	}
	ci.MixedRollout = len(ci.Running) > 1
	if ci.Desired == "" {
		return
	}
	d, ok := imageref.Parse(ci.Desired)
	if !ok {
		return
	}
	if ci.Repository == "" {
		ci.Repository = d.Repository
	}
	if len(ci.Running) == 0 {
		ci.Drift = true

		return
	}
	for _, r := range ci.Running {
		rr, _ := imageref.Parse(r.Image)
		same := rr.Repository == d.Repository && rr.Tag == d.Tag
		if d.Digest != "" {
			same = rr.Repository == d.Repository && r.Digest == d.Digest
		}
		if !same {
			ci.Drift = true
		}
	}
}

// compare returns the verdict of one container against one expected image.
func compare(e imageref.Ref, ci *ContainerImages) Comparison {
	c := Comparison{Expected: e.String()}
	total, matched, verified := 0, 0, true
	tagSeen := false
	for _, r := range ci.Running {
		n := len(r.Pods)
		total += n
		ok := false
		if e.Digest != "" && r.Digest != "" {
			ok = r.Digest == e.Digest
		} else {
			verified = false
			ok = e.Tag != "" && r.Tag == e.Tag
		}
		if e.Tag != "" && r.Tag == e.Tag {
			tagSeen = true
		}
		if ok {
			matched += n
		}
	}

	switch {
	case total == 0:
		c.Result, c.Hint = ResultNotFound, "no running pods for this container"
	case matched == total && verified:
		c.Result = ResultMatch
	case matched == total:
		c.Result, c.Hint = ResultMatchTagOnly, "tags match but the digest could not be verified (no digest from the build or the cluster)"
	case matched > 0:
		c.Result, c.Hint = ResultMixedRollout, fmt.Sprintf("%d of %d pods run the expected image; a rollout is in progress or stuck", matched, total)
	case tagSeen && e.Digest != "":
		c.Result, c.Hint = ResultDigestMismatch, "same tag, different image: the tag was pushed again after this build, pods were not restarted, "+
			"or one side reports a multi-arch index digest and the other a platform digest"
	default:
		c.Result, c.Hint = ResultDifferentTag, "the build's image is not running"
		if d, ok := imageref.Parse(ci.Desired); ok && d.Repository == e.Repository && d.Tag == e.Tag {
			c.Hint += "; Git already has it, so check the rollout (events, pod status)"
		} else {
			c.Hint += "; Git still points to a different tag (not deployed yet?)"
		}
	}

	return c
}

func compareAll(out *imagesOutput, expected []imageref.Ref) {
	worst := ResultMatch
	note := func(r string) {
		if severity[r] > severity[worst] {
			worst = r
		}
	}
	for _, e := range expected {
		found := false
		for i := range out.Containers {
			ci := &out.Containers[i]
			if ci.Repository != e.Repository {
				continue
			}
			found = true
			c := compare(e, ci)
			ci.Comparison = &c
			note(c.Result)
		}
		if !found {
			out.NotFound = append(out.NotFound, Comparison{Expected: e.String(), Result: ResultNotFound,
				Hint: "no container of this application uses repository " + e.Repository})
			note(ResultNotFound)
		}
	}
	out.Verdict = worst
}

func decodeObject(raw string) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal([]byte(raw), &obj)

	return obj, err
}
