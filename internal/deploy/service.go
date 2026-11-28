package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/imageref"
)

// API is the Argo CD port the use cases need.
type API interface {
	GetApplication(ctx context.Context, ref client.AppRef) (*client.Application, error)
	PatchApplication(ctx context.Context, ref client.AppRef, ops []client.PatchOp) (*client.Application, error)
	SyncApplication(ctx context.Context, ref client.AppRef, o client.SyncOptions) (*client.Application, error)
	GetManifests(ctx context.Context, ref client.AppRef, revision string) (*client.ManifestResponse, error)
}

// Policy errors (match with errors.Is).
var (
	ErrNotAllowed       = errors.New("not allowed by server policy")
	ErrManaged          = errors.New("application is managed by an ApplicationSet")
	ErrBusy             = errors.New("an operation is already running on the application")
	ErrConcurrentChange = errors.New("the application changed while it was being updated")
	// ErrInvalidInput: the request is invalid for this application.
	ErrInvalidInput = errors.New("invalid input")
)

func invalid(err error) error {
	if errors.Is(err, ErrNoOverridableSource) {
		return err
	}

	return fmt.Errorf("%w: %v", ErrInvalidInput, err)
}

// Config holds the deploy policy and defaults.
type Config struct {
	AllowedProjects []string      // empty = all projects the token may access
	HelmTagParam    string        // default image.tag
	HelmRepoParam   string        // default image.repository
	SyncTimeout     time.Duration // default wait for health
	MaxSyncTimeout  time.Duration
	PollInterval    time.Duration
	ConfirmTTL      time.Duration
}

func (c *Config) defaults() {
	if c.HelmTagParam == "" {
		c.HelmTagParam = "image.tag"
	}
	if c.HelmRepoParam == "" {
		c.HelmRepoParam = "image.repository"
	}
	if c.SyncTimeout <= 0 {
		c.SyncTimeout = 5 * time.Minute
	}
	if c.MaxSyncTimeout <= 0 {
		c.MaxSyncTimeout = 15 * time.Minute
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 3 * time.Second
	}
	if c.ConfirmTTL <= 0 {
		c.ConfirmTTL = 5 * time.Minute
	}
}

// Service implements the three use cases.
type Service struct {
	api     API
	cfg     Config
	confirm *Confirmations
	wait    Waiter
}

// NewService wires the use cases.
func NewService(api API, cfg Config) *Service {
	cfg.defaults()

	return &Service{
		api:     api,
		cfg:     cfg,
		confirm: NewConfirmations(cfg.ConfirmTTL, nil),
		wait:    Waiter{Interval: cfg.PollInterval},
	}
}

// Defaults returns the effective configuration.
func (s *Service) Defaults() Config { return s.cfg }

func appKey(ref client.AppRef) string { return ref.AppNamespace + "/" + ref.Name }

// load reads the app and enforces policy common to writes.
func (s *Service) load(ctx context.Context, ref client.AppRef, write bool) (*client.Application, error) {
	app, err := s.api.GetApplication(ctx, ref)
	if err != nil {
		return nil, err
	}
	if write && len(s.cfg.AllowedProjects) > 0 && !slices.Contains(s.cfg.AllowedProjects, app.Spec.Project) {
		return nil, fmt.Errorf("%w: project %q is not in ARGOCD_DEPLOY_ALLOWED_PROJECTS", ErrNotAllowed, app.Spec.Project)
	}

	return app, nil
}

func busy(app *client.Application) bool {
	return len(app.Operation) > 0 || (app.Status.OperationState != nil && app.Status.OperationState.Phase == "Running")
}

func autoSync(app *client.Application) bool {
	var p struct {
		Automated *struct {
			Enabled *bool `json:"enabled"`
		} `json:"automated"`
	}
	if len(app.Spec.SyncPolicy) == 0 || json.Unmarshal(app.Spec.SyncPolicy, &p) != nil || p.Automated == nil {
		return false
	}

	return p.Automated.Enabled == nil || *p.Automated.Enabled
}

func applicationSetOwner(app *client.Application) string {
	for _, o := range app.Metadata.OwnerReferences {
		if o.Kind == "ApplicationSet" {
			return o.Name
		}
	}

	return ""
}

func managedWarnings(app *client.Application) []string {
	if _, ok := app.Metadata.Annotations["argocd.argoproj.io/tracking-id"]; ok {
		return []string{"this Application is itself managed by another Argo CD application (app-of-apps); if that parent syncs with self-heal, the override can be reverted"}
	}

	return nil
}

// ---- status ----

// imageParamRe selects Helm parameters that describe images; other parameters
// are not returned because they may hold secrets.
var imageParamRe = regexp.MustCompile(`(?i)image|tag|repository|registry|digest`)

// SourceView is a source with its image overrides.
type SourceView struct {
	Index           int                    `json:"index"`
	Type            string                 `json:"type"`
	RepoURL         string                 `json:"repoURL"`
	Path            string                 `json:"path,omitempty"`
	Chart           string                 `json:"chart,omitempty"`
	TargetRevision  string                 `json:"targetRevision,omitempty"`
	ImageParameters []client.HelmParameter `json:"imageParameters,omitempty"`
	OtherParameters int                    `json:"otherParameterCount,omitempty"`
	KustomizeImages []string               `json:"kustomizeImages,omitempty"`
}

// Status is the output of the status use case.
type Status struct {
	Name             string                  `json:"name"`
	Project          string                  `json:"project"`
	SyncStatus       string                  `json:"syncStatus"`
	Revision         string                  `json:"revision,omitempty"`
	Health           string                  `json:"health"`
	HealthMessage    string                  `json:"healthMessage,omitempty"`
	AutoSync         bool                    `json:"autoSync"`
	OperationRunning bool                    `json:"operationRunning"`
	LastOperation    *client.OperationState  `json:"lastOperation,omitempty"`
	Sources          []SourceView            `json:"sources"`
	RunningImages    []string                `json:"runningImages"`
	OutOfSync        []client.ResourceStatus `json:"outOfSyncResources,omitempty"`
	Unhealthy        []client.ResourceStatus `json:"unhealthyResources,omitempty"`
	ManagedBy        string                  `json:"managedByApplicationSet,omitempty"`
	Warnings         []string                `json:"warnings,omitempty"`
}

// Status returns the application's images, overrides, sync and health.
func (s *Service) Status(ctx context.Context, ref client.AppRef) (*Status, error) {
	app, err := s.load(ctx, ref, false)
	if err != nil {
		return nil, err
	}

	return statusOf(app), nil
}

func statusOf(app *client.Application) *Status {
	st := &Status{
		Name: app.Metadata.Name, Project: app.Spec.Project,
		SyncStatus: app.Status.Sync.Status, Revision: app.Status.Sync.Revision,
		Health: app.Status.Health.Status, HealthMessage: app.Status.Health.Message,
		AutoSync: autoSync(app), OperationRunning: busy(app), LastOperation: app.Status.OperationState,
		Sources: []SourceView{}, RunningImages: app.Status.Summary.Images,
		ManagedBy: applicationSetOwner(app), Warnings: managedWarnings(app),
	}
	if st.RunningImages == nil {
		st.RunningImages = []string{}
	}
	for _, src := range sources(app) {
		v := SourceView{Index: src.index, Type: src.typ, RepoURL: src.src.RepoURL, Path: src.src.Path, Chart: src.src.Chart, TargetRevision: src.src.TargetRevision}
		if src.src.Helm != nil {
			for _, p := range src.src.Helm.Parameters {
				if imageParamRe.MatchString(p.Name) {
					v.ImageParameters = append(v.ImageParameters, p)
				} else {
					v.OtherParameters++
				}
			}
		}
		if src.src.Kustomize != nil {
			v.KustomizeImages = src.src.Kustomize.Images
		}
		st.Sources = append(st.Sources, v)
	}
	for _, r := range app.Status.Resources {
		if r.Status != "" && r.Status != "Synced" {
			st.OutOfSync = append(st.OutOfSync, r)
		}
		if r.Health != nil && r.Health.Status != "" && r.Health.Status != "Healthy" {
			st.Unhealthy = append(st.Unhealthy, r)
		}
	}
	if st.ManagedBy != "" {
		st.Warnings = append(st.Warnings, "managed by ApplicationSet "+st.ManagedBy+": image overrides are refused because the ApplicationSet would revert them")
	}

	return st
}

// ---- update image ----

// UpdateResult is the output of the update use case.
type UpdateResult struct {
	Status            string   `json:"status"` // UPDATED | UNCHANGED | CONFIRMATION_REQUIRED
	Application       string   `json:"application"`
	Plan              *Plan    `json:"plan"`
	AutoSync          bool     `json:"autoSync"`
	ConfirmationToken string   `json:"confirmationToken,omitempty"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
	RenderedVerified  *bool    `json:"renderedImageVerified,omitempty"`
	RenderedImages    []string `json:"renderedImages,omitempty"`
	NextStep          string   `json:"nextStep"`
	Warnings          []string `json:"warnings,omitempty"`
}

// UpdateImage sets the image tag (and optionally repository) as a parameter
// override. When auto-sync is on, applying it deploys immediately, so it then
// needs a confirmation token from a previous call.
func (s *Service) UpdateImage(ctx context.Context, ref client.AppRef, u ImageUpdate, token string) (*UpdateResult, error) {
	if u.HelmTagParam == "" {
		u.HelmTagParam = s.cfg.HelmTagParam
	}
	if u.HelmRepoParam == "" {
		u.HelmRepoParam = s.cfg.HelmRepoParam
	}
	if err := u.Validate(); err != nil {
		return nil, invalid(err)
	}
	app, err := s.load(ctx, ref, true)
	if err != nil {
		return nil, err
	}
	if owner := applicationSetOwner(app); owner != "" {
		return nil, fmt.Errorf("%w %q: the ApplicationSet would revert a parameter override; change its template or Git instead", ErrManaged, owner)
	}
	if busy(app) {
		return nil, fmt.Errorf("%w; wait for it to finish (see argocd_get_application_status)", ErrBusy)
	}
	plan, err := PlanImageUpdate(app, u)
	if err != nil {
		return nil, invalid(err)
	}
	res := &UpdateResult{Application: ref.Name, Plan: plan, AutoSync: autoSync(app), Warnings: managedWarnings(app)}
	if plan.NoChange {
		res.Status, res.NextStep = "UNCHANGED", "the image is already set; use argocd_sync_application if it is not deployed yet"

		return res, nil
	}

	if res.AutoSync {
		fp := Fingerprint(app.Spec, plan.Changes)
		if token == "" {
			t, exp, err := s.confirm.Issue("update", appKey(ref), fp)
			if err != nil {
				return nil, err
			}
			res.Status, res.ConfirmationToken, res.ExpiresAt = "CONFIRMATION_REQUIRED", t, exp.UTC().Format(time.RFC3339)
			res.NextStep = "auto-sync is ON: applying this change deploys it immediately. Show the plan to the developer and, only after they " +
				"explicitly approve, call update_application_image again with the same arguments and this confirmationToken."

			return res, nil
		}
		if err := s.confirm.Consume(token, "update", appKey(ref), fp); err != nil {
			return nil, err
		}
	}

	if _, err := s.api.PatchApplication(ctx, ref, plan.Ops); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, "test failed") {
			return nil, fmt.Errorf("%w; nothing was changed, read the status and try again", ErrConcurrentChange)
		}

		return nil, err
	}
	res.Status = "UPDATED"
	s.verifyRendered(ctx, ref, plan, res)
	if res.AutoSync {
		res.NextStep = "auto-sync will deploy the change; check progress with argocd_get_application_status"
	} else {
		res.NextStep = "call argocd_sync_application to deploy (it asks for confirmation first)"
	}

	return res, nil
}

// verifyRendered checks that the override actually changes a rendered image,
// catching a wrong Helm parameter name before anything is deployed.
func (s *Service) verifyRendered(ctx context.Context, ref client.AppRef, plan *Plan, res *UpdateResult) {
	m, err := s.api.GetManifests(ctx, ref, "")
	if err != nil {
		res.Warnings = append(res.Warnings, "could not verify rendered manifests: "+err.Error())

		return
	}
	want, _ := imageref.Parse(strings.TrimPrefix(plan.Image, ":"))
	tagOnly := strings.HasPrefix(plan.Image, ":")
	found := false
	for _, img := range renderedImages(m.Manifests) {
		r, ok := imageref.Parse(img)
		if !ok {
			continue
		}
		match := r.Tag == plan.Image[strings.LastIndex(plan.Image, ":")+1:]
		if !tagOnly {
			match = match && r.Repository == want.Repository
		}
		if match {
			found = true
			res.RenderedImages = append(res.RenderedImages, img)
		}
	}
	res.RenderedVerified = &found
	if !found {
		res.Warnings = append(res.Warnings, fmt.Sprintf("no rendered container uses %s: the parameter name may be wrong for this chart "+
			"(set helmTagParameter/helmRepositoryParameter), so syncing would not change the image", strings.TrimPrefix(plan.Image, ":")))
	}
}

func renderedImages(manifests []string) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				if s, ok := x.(string); ok && k == "image" {
					out = append(out, s)
				} else {
					walk(x)
				}
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	for _, raw := range manifests {
		var obj any
		if json.Unmarshal([]byte(raw), &obj) == nil {
			walk(obj)
		}
	}

	return out
}

// ---- sync ----

// SyncRequest configures the sync use case.
type SyncRequest struct {
	Prune         bool
	Token         string
	Timeout       time.Duration
	ExpectedImage string // optional: image (repo:tag) that must run afterwards
}

// SyncResult is the output of the sync use case.
type SyncResult struct {
	Status            string   `json:"status"` // CONFIRMATION_REQUIRED | DEPLOYED_HEALTHY | SYNC_FAILED | DEGRADED | SUSPENDED | TIMEOUT
	Application       string   `json:"application"`
	Preview           *Status  `json:"preview,omitempty"`
	Prune             bool     `json:"prune"`
	ConfirmationToken string   `json:"confirmationToken,omitempty"`
	ExpiresAt         string   `json:"expiresAt,omitempty"`
	Outcome           *Outcome `json:"outcome,omitempty"`
	ImageVerified     *bool    `json:"expectedImageRunning,omitempty"`
	NextStep          string   `json:"nextStep"`
}

// Sync previews (no token) or performs (valid token) a sync, then waits until
// the operation finished and health settled.
func (s *Service) Sync(ctx context.Context, ref client.AppRef, r SyncRequest) (*SyncResult, error) {
	if r.Timeout <= 0 {
		r.Timeout = s.cfg.SyncTimeout
	}
	if r.Timeout > s.cfg.MaxSyncTimeout {
		return nil, fmt.Errorf("%w: timeout must be at most %s", ErrInvalidInput, s.cfg.MaxSyncTimeout)
	}
	var expected imageref.Ref
	if r.ExpectedImage != "" {
		var ok bool
		if expected, ok = imageref.Parse(r.ExpectedImage); !ok || expected.Tag == "" {
			return nil, fmt.Errorf("%w: expectedImage %q must be repository:tag", ErrInvalidInput, r.ExpectedImage)
		}
	}
	app, err := s.load(ctx, ref, true)
	if err != nil {
		return nil, err
	}
	if busy(app) {
		return nil, fmt.Errorf("%w; wait for it to finish (see argocd_get_application_status)", ErrBusy)
	}
	fp := Fingerprint(app.Spec, r.Prune)
	res := &SyncResult{Application: ref.Name, Prune: r.Prune}

	if r.Token == "" {
		t, exp, err := s.confirm.Issue("sync", appKey(ref), fp)
		if err != nil {
			return nil, err
		}
		res.Status, res.Preview, res.ConfirmationToken, res.ExpiresAt = "CONFIRMATION_REQUIRED", statusOf(app), t, exp.UTC().Format(time.RFC3339)
		res.NextStep = "Show this preview (out-of-sync resources, images, prune) to the developer. Only after they explicitly approve, " +
			"call argocd_sync_application again with the same arguments and this confirmationToken."

		return res, nil
	}
	if err := s.confirm.Consume(r.Token, "sync", appKey(ref), fp); err != nil {
		return nil, err
	}

	prev := ""
	if app.Status.OperationState != nil {
		prev = app.Status.OperationState.StartedAt
	}
	if _, err := s.api.SyncApplication(ctx, ref, client.SyncOptions{Prune: r.Prune}); err != nil {
		return nil, err
	}
	out, err := s.wait.Wait(ctx, s.api, ref, prev, r.Timeout)
	if err != nil {
		return nil, err
	}
	res.Status, res.Outcome = out.Result, &out
	if r.ExpectedImage != "" {
		running := false
		for _, img := range out.RunningImages {
			if x, ok := imageref.Parse(img); ok && x.Repository == expected.Repository && x.Tag == expected.Tag {
				running = true
			}
		}
		res.ImageVerified = &running
	}
	switch out.Result {
	case OutcomeHealthy:
		res.NextStep = "deployment verified healthy"
	case OutcomeTimeout:
		res.NextStep = "the sync was started but did not finish in time; check again with argocd_get_application_status"
	default:
		res.NextStep = "deployment did not become healthy; inspect failedResources/unhealthyResources. To roll back, set the previous tag with update_application_image and sync again"
	}

	return res, nil
}
