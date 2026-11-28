package argocd

import (
	"context"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

const (
	defaultTailLines = 200
	maxTailLines     = 5000
	maxSinceSeconds  = 7 * 24 * 3600
)

var kindRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)

type logsInput struct {
	appInput
	Namespace    string `json:"namespace,omitempty" jsonschema:"Namespace of the pod or workload (defaults to the application's destination namespace)."`
	PodName      string `json:"podName,omitempty" jsonschema:"Pod name. Use this OR kind + resourceName."`
	Kind         string `json:"kind,omitempty" jsonschema:"Workload kind for aggregated logs, e.g. Deployment, StatefulSet, Job, Rollout."`
	Group        string `json:"group,omitempty" jsonschema:"API group of the workload, e.g. apps (Deployment), argoproj.io (Rollout). Optional."`
	ResourceName string `json:"resourceName,omitempty" jsonschema:"Workload name for aggregated logs (with kind)."`
	Container    string `json:"container,omitempty" jsonschema:"Container name (defaults to the pod's only/default container)."`
	TailLines    int64  `json:"tailLines,omitempty" jsonschema:"Lines from the end of each container log (default 200, max 5000)."`
	SinceSeconds int64  `json:"sinceSeconds,omitempty" jsonschema:"Only logs newer than this many seconds (max 604800)."`
	Previous     bool   `json:"previous,omitempty" jsonschema:"Logs of the previous container instance, e.g. after a crash/restart."`
	Filter       string `json:"filter,omitempty" jsonschema:"Only lines containing this text; prefix with ! to exclude lines containing it."`
}

type logsOutput struct {
	Application string           `json:"application"`
	Target      string           `json:"target"`
	Count       int              `json:"count"`
	Truncated   bool             `json:"truncated"`
	Lines       []client.LogLine `json:"lines"`
}

func (in logsInput) options() (client.LogOptions, string, error) {
	o := client.LogOptions{
		Namespace: in.Namespace, Container: in.Container, Previous: in.Previous, Filter: in.Filter,
		TailLines: in.TailLines, SinceSeconds: in.SinceSeconds,
	}
	if err := toolkit.OptionalNamespace("namespace", in.Namespace); err != nil {
		return o, "", err
	}
	if err := toolkit.OptionalNamespace("container", in.Container); err != nil {
		return o, "", err
	}
	if err := toolkit.Text("filter", in.Filter, 256); err != nil {
		return o, "", err
	}
	if in.TailLines < 0 || in.TailLines > maxTailLines {
		return o, "", toolkit.Invalid("tailLines must be between 1 and %d", maxTailLines)
	}
	if in.TailLines == 0 {
		o.TailLines = defaultTailLines
	}
	if in.SinceSeconds < 0 || in.SinceSeconds > maxSinceSeconds {
		return o, "", toolkit.Invalid("sinceSeconds must be between 1 and %d", maxSinceSeconds)
	}

	switch {
	case in.PodName != "" && (in.Kind != "" || in.ResourceName != ""):
		return o, "", toolkit.Invalid("use either podName or kind + resourceName, not both")
	case in.PodName != "":
		if err := toolkit.K8sName("podName", in.PodName); err != nil {
			return o, "", err
		}
		o.PodName = in.PodName

		return o, "Pod/" + in.PodName, nil
	case in.Kind != "" && in.ResourceName != "":
		if !kindRe.MatchString(in.Kind) {
			return o, "", toolkit.Invalid("kind %q is not a valid Kubernetes kind", in.Kind)
		}
		if err := toolkit.K8sName("resourceName", in.ResourceName); err != nil {
			return o, "", err
		}
		if err := toolkit.OptionalK8sName("group", in.Group); err != nil {
			return o, "", err
		}
		o.Kind, o.Group, o.ResourceName = in.Kind, in.Group, in.ResourceName

		return o, in.Kind + "/" + in.ResourceName, nil
	default:
		return o, "", toolkit.Invalid("podName, or kind + resourceName, is required")
	}
}

func (t *Tools) getLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, logsOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return nil, logsOutput{}, err
	}
	opts, target, err := in.options()
	if err != nil {
		return nil, logsOutput{}, err
	}
	res, err := t.api.GetLogs(ctx, ref, opts)
	if err != nil {
		return nil, logsOutput{}, wrap(err)
	}

	return nil, logsOutput{
		Application: ref.Name, Target: target,
		Count: len(res.Lines), Truncated: res.Truncated, Lines: res.Lines,
	}, nil
}
