package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestArgoCDEnabled(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"ARGOCD_SERVER": "argocd.example.com", "ARGOCD_AUTH_TOKEN": "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ArgoCD == nil || c.ArgoCD.BaseURL != "https://argocd.example.com" || c.ArgoCD.Timeout.Seconds() != 30 {
		t.Fatalf("%+v", c.ArgoCD)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	ok := map[string]string{
		"argocd.example.com":          "https://argocd.example.com",
		"argocd.example.com:8443":     "https://argocd.example.com:8443",
		"https://argocd.example.com/": "https://argocd.example.com",
		"https://example.com/argo-cd": "https://example.com/argo-cd",
	}
	for in, want := range ok {
		if got, err := NormalizeServerURL(in); err != nil || got != want {
			t.Errorf("%q => %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"http://argocd.example.com", "https://u:p@argocd.example.com", "https://a.example.com?x=1", "ftp://a"} {
		if _, err := NormalizeServerURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"nothing configured": {map[string]string{}, "no integration configured"},
		"missing token":      {map[string]string{"ARGOCD_SERVER": "a.example.com"}, "ARGOCD_AUTH_TOKEN"},
		"bad bool":           {map[string]string{"ARGOCD_SERVER": "a.example.com", "ARGOCD_AUTH_TOKEN": "t", "ARGOCD_INSECURE": "maybe"}, "ARGOCD_INSECURE"},
		"bad timeout":        {map[string]string{"ARGOCD_SERVER": "a.example.com", "ARGOCD_AUTH_TOKEN": "t", "ARGOCD_REQUEST_TIMEOUT": "soon"}, "ARGOCD_REQUEST_TIMEOUT"},
		"http server":        {map[string]string{"ARGOCD_SERVER": "http://a.example.com", "ARGOCD_AUTH_TOKEN": "t"}, "https"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFrom(env(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestTokenFileAlone(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"ARGOCD_SERVER": "a.example.com", "ARGOCD_AUTH_TOKEN_FILE": "/tmp/x"}))
	if err != nil || c.ArgoCD.TokenFile != "/tmp/x" {
		t.Fatalf("%v %+v", err, c)
	}
}
