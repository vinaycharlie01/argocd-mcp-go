package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Deploy configures the write-enabled deploy server (cmd/deploy).
type Deploy struct {
	// AllowedProjects limits writes to these Argo CD projects
	// (ARGOCD_DEPLOY_ALLOWED_PROJECTS, comma separated). Empty = any project
	// the user's token may write to; Argo CD RBAC always applies as well.
	AllowedProjects []string
	HelmTagParam    string        // ARGOCD_HELM_TAG_PARAM (default image.tag)
	HelmRepoParam   string        // ARGOCD_HELM_REPOSITORY_PARAM (default image.repository)
	SyncTimeout     time.Duration // ARGOCD_SYNC_TIMEOUT (default 5m, max 15m)
}

// MaxSyncTimeout bounds how long a sync tool call may wait.
const MaxSyncTimeout = 15 * time.Minute

var (
	helmParamRe = regexp.MustCompile(`^[A-Za-z0-9_.\-\[\]]{1,253}$`)
	projectRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// LoadDeploy reads the deploy settings from the environment.
func LoadDeploy() (*Deploy, error) { return LoadDeployFrom(os.Getenv) }

// LoadDeployFrom reads the deploy settings through getenv.
func LoadDeployFrom(getenv func(string) string) (*Deploy, error) {
	r := reader{getenv: getenv}
	d := &Deploy{
		HelmTagParam:  r.str("ARGOCD_HELM_TAG_PARAM", "image.tag"),
		HelmRepoParam: r.str("ARGOCD_HELM_REPOSITORY_PARAM", "image.repository"),
		SyncTimeout:   r.duration("ARGOCD_SYNC_TIMEOUT", 5*time.Minute),
	}
	for _, p := range strings.Split(r.str("ARGOCD_DEPLOY_ALLOWED_PROJECTS", ""), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if !projectRe.MatchString(p) {
			r.errs = append(r.errs, fmt.Errorf("ARGOCD_DEPLOY_ALLOWED_PROJECTS: %q is not a valid project name", p))
		}
		d.AllowedProjects = append(d.AllowedProjects, p)
	}
	for k, v := range map[string]string{"ARGOCD_HELM_TAG_PARAM": d.HelmTagParam, "ARGOCD_HELM_REPOSITORY_PARAM": d.HelmRepoParam} {
		if !helmParamRe.MatchString(v) {
			r.errs = append(r.errs, fmt.Errorf("%s: %q is not a valid Helm parameter name", k, v))
		}
	}
	if d.SyncTimeout <= 0 || d.SyncTimeout > MaxSyncTimeout {
		r.errs = append(r.errs, fmt.Errorf("ARGOCD_SYNC_TIMEOUT must be between 1s and %s", MaxSyncTimeout))
	}

	return d, errors.Join(r.errs...)
}
