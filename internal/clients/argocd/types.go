package argocd

import "encoding/json"

// Wire types: only the fields the tools need. Unknown fields are ignored, so
// these stay compatible across Argo CD versions.

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
}

// Source is an application source.
type Source struct {
	RepoURL        string `json:"repoURL"`
	Path           string `json:"path,omitempty"`
	Chart          string `json:"chart,omitempty"`
	TargetRevision string `json:"targetRevision,omitempty"`
	Ref            string `json:"ref,omitempty"`
	Name           string `json:"name,omitempty"`
}

// Destination is the target cluster and namespace.
type Destination struct {
	Server    string `json:"server,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

type appSpec struct {
	Project     string          `json:"project"`
	Source      *Source         `json:"source,omitempty"`
	Sources     []Source        `json:"sources,omitempty"`
	Destination Destination     `json:"destination"`
	SyncPolicy  json.RawMessage `json:"syncPolicy,omitempty"`
}

// Condition is an application condition (errors and warnings).
type Condition struct {
	Type               string `json:"type"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// OperationState is the last/current sync operation.
type OperationState struct {
	Phase      string `json:"phase"`
	Message    string `json:"message,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Operation  struct {
		InitiatedBy Initiator `json:"initiatedBy"`
	} `json:"operation"`
}

// Initiator is who started an operation.
type Initiator struct {
	Username  string `json:"username,omitempty"`
	Automated bool   `json:"automated,omitempty"`
}

// HistoryEntry is one deployment from status.history.
type HistoryEntry struct {
	ID              int64     `json:"id"`
	Revision        string    `json:"revision,omitempty"`
	Revisions       []string  `json:"revisions,omitempty"`
	DeployedAt      string    `json:"deployedAt"`
	DeployStartedAt string    `json:"deployStartedAt,omitempty"`
	Source          *Source   `json:"source,omitempty"`
	Sources         []Source  `json:"sources,omitempty"`
	InitiatedBy     Initiator `json:"initiatedBy"`
}

// ResourceStatus is a managed resource's sync/health entry.
type ResourceStatus struct {
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
	Health    *struct {
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"health,omitempty"`
}

type appStatus struct {
	Sync struct {
		Status    string   `json:"status"`
		Revision  string   `json:"revision,omitempty"`
		Revisions []string `json:"revisions,omitempty"`
	} `json:"sync"`
	Health struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	} `json:"health"`
	OperationState *OperationState  `json:"operationState,omitempty"`
	Conditions     []Condition      `json:"conditions,omitempty"`
	History        []HistoryEntry   `json:"history,omitempty"`
	Resources      []ResourceStatus `json:"resources,omitempty"`
	ReconciledAt   string           `json:"reconciledAt,omitempty"`
	SourceType     string           `json:"sourceType,omitempty"`
	Summary        struct {
		Images       []string `json:"images,omitempty"`
		ExternalURLs []string `json:"externalURLs,omitempty"`
	} `json:"summary"`
}

// Application is the wire form of an Argo CD Application.
type Application struct {
	Metadata objectMeta `json:"metadata"`
	Spec     appSpec    `json:"spec"`
	Status   appStatus  `json:"status"`
}

type applicationList struct {
	Items []Application `json:"items"`
}

// ResourceNode is a node of the application resource tree.
type ResourceNode struct {
	Group           string `json:"group,omitempty"`
	Version         string `json:"version,omitempty"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	ParentRefs      []struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace,omitempty"`
		Name      string `json:"name"`
	} `json:"parentRefs,omitempty"`
	Info []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"info,omitempty"`
	Images []string `json:"images,omitempty"`
	Health *struct {
		Status  string `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"health,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
}

// ResourceTree is the response of /resource-tree.
type ResourceTree struct {
	Nodes         []ResourceNode `json:"nodes"`
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
}

// ManifestResponse is the response of /manifests. Secret data is masked by
// the Argo CD server before it is returned.
type ManifestResponse struct {
	Manifests  []string `json:"manifests"`
	Namespace  string   `json:"namespace,omitempty"`
	Server     string   `json:"server,omitempty"`
	Revision   string   `json:"revision,omitempty"`
	SourceType string   `json:"sourceType,omitempty"`
}

// Event is a Kubernetes event as returned by /events.
type Event struct {
	Type           string `json:"type"`
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	Count          int32  `json:"count,omitempty"`
	FirstTimestamp string `json:"firstTimestamp,omitempty"`
	LastTimestamp  string `json:"lastTimestamp,omitempty"`
	EventTime      string `json:"eventTime,omitempty"`
	InvolvedObject struct {
		Kind      string `json:"kind"`
		Namespace string `json:"namespace,omitempty"`
		Name      string `json:"name"`
	} `json:"involvedObject"`
	Source struct {
		Component string `json:"component,omitempty"`
	} `json:"source"`
}

type eventList struct {
	Items []Event `json:"items"`
}
