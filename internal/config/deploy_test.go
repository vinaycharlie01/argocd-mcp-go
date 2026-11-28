package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDeployDefaults(t *testing.T) {
	d, err := LoadDeployFrom(env(map[string]string{}))
	if err != nil || d.HelmTagParam != "image.tag" || d.HelmRepoParam != "image.repository" || d.SyncTimeout != 5*time.Minute || d.AllowedProjects != nil {
		t.Fatalf("%v %+v", err, d)
	}
	d, err = LoadDeployFrom(env(map[string]string{"ARGOCD_DEPLOY_ALLOWED_PROJECTS": "team-a, team-b,", "ARGOCD_HELM_TAG_PARAM": "app.image.tag", "ARGOCD_SYNC_TIMEOUT": "10m"}))
	if err != nil || strings.Join(d.AllowedProjects, "|") != "team-a|team-b" || d.HelmTagParam != "app.image.tag" || d.SyncTimeout != 10*time.Minute {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestLoadDeployValidation(t *testing.T) {
	for k, v := range map[string]string{
		"ARGOCD_DEPLOY_ALLOWED_PROJECTS": "Team_A",
		"ARGOCD_HELM_TAG_PARAM":          "image tag",
		"ARGOCD_SYNC_TIMEOUT":            "1h",
	} {
		if _, err := LoadDeployFrom(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
}
