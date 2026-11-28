package deploy

import (
	"context"
	"errors"
	"time"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
)

// Deployment outcomes.
const (
	OutcomeHealthy    = "DEPLOYED_HEALTHY"
	OutcomeSyncFailed = "SYNC_FAILED"
	OutcomeDegraded   = "DEGRADED"
	OutcomeSuspended  = "SUSPENDED"
	OutcomeTimeout    = "TIMEOUT"
)

// Outcome is the verified result of a sync.
type Outcome struct {
	Result          string                  `json:"result"`
	OperationPhase  string                  `json:"operationPhase,omitempty"`
	OperationMsg    string                  `json:"operationMessage,omitempty"`
	SyncStatus      string                  `json:"syncStatus,omitempty"`
	Revision        string                  `json:"revision,omitempty"`
	Health          string                  `json:"health,omitempty"`
	HealthMessage   string                  `json:"healthMessage,omitempty"`
	RunningImages   []string                `json:"runningImages,omitempty"`
	FailedResources []client.ResourceResult `json:"failedResources,omitempty"`
	Unhealthy       []client.ResourceStatus `json:"unhealthyResources,omitempty"`
	ElapsedSeconds  int                     `json:"elapsedSeconds"`
}

// Getter reads an application.
type Getter interface {
	GetApplication(ctx context.Context, ref client.AppRef) (*client.Application, error)
}

// Waiter polls an application until its new sync operation finished and its
// health settled, or the timeout passes.
type Waiter struct {
	Interval time.Duration
	Now      func() time.Time
}

func terminal(phase string) bool {
	return phase == "Succeeded" || phase == "Failed" || phase == "Error"
}

// Wait waits for the operation started after prevStartedAt.
func (w Waiter) Wait(ctx context.Context, g Getter, ref client.AppRef, prevStartedAt string, timeout time.Duration) (Outcome, error) {
	now := w.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last Outcome
	degradedSeen := 0
	for {
		app, err := g.GetApplication(ctx, ref)
		switch {
		case err == nil:
			last = observe(app)
			op := app.Status.OperationState
			ours := op != nil && op.StartedAt != prevStartedAt
			if len(app.Operation) == 0 && ours && terminal(op.Phase) {
				if op.Phase != "Succeeded" {
					last.Result = OutcomeSyncFailed
					last.ElapsedSeconds = int(now().Sub(start).Seconds())

					return last, nil
				}
				switch app.Status.Health.Status {
				case "Healthy":
					last.Result = OutcomeHealthy
				case "Suspended":
					last.Result = OutcomeSuspended
				case "Degraded":
					// Require it twice in a row to skip a transient state.
					if degradedSeen++; degradedSeen >= 2 {
						last.Result = OutcomeDegraded
					}
				default:
					degradedSeen = 0
				}
				if last.Result != "" {
					last.ElapsedSeconds = int(now().Sub(start).Seconds())

					return last, nil
				}
			}
		case errors.Is(err, client.ErrUnauthorized), errors.Is(err, client.ErrForbidden), errors.Is(err, client.ErrNotFound):
			return last, err
		default:
			// Transient (network, timeout of one request): keep polling.
		}

		select {
		case <-ctx.Done():
			last.Result = OutcomeTimeout
			last.ElapsedSeconds = int(now().Sub(start).Seconds())

			return last, nil
		case <-time.After(w.Interval):
		}
	}
}

func observe(app *client.Application) Outcome {
	o := Outcome{
		SyncStatus:    app.Status.Sync.Status,
		Revision:      app.Status.Sync.Revision,
		Health:        app.Status.Health.Status,
		HealthMessage: app.Status.Health.Message,
		RunningImages: app.Status.Summary.Images,
	}
	if op := app.Status.OperationState; op != nil {
		o.OperationPhase, o.OperationMsg = op.Phase, op.Message
		if op.SyncResult != nil {
			for _, r := range op.SyncResult.Resources {
				if r.Status == "SyncFailed" || r.HookPhase == "Failed" || r.HookPhase == "Error" {
					o.FailedResources = append(o.FailedResources, r)
				}
			}
		}
	}
	for _, r := range app.Status.Resources {
		if r.Health != nil && r.Health.Status != "" && r.Health.Status != "Healthy" {
			o.Unhealthy = append(o.Unhealthy, r)
		}
	}

	return o
}
