package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"ARGOCD_SERVER_URL": "https://argocd.example.com/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ArgoCD.ServerURL != "https://argocd.example.com" {
		t.Errorf("trailing slash not trimmed: %q", c.ArgoCD.ServerURL)
	}
	if c.MCP.Transport != TransportStdio || c.OIDC.RedirectURL != DefaultLocalRedirectURL || !c.OIDC.OpenBrowser {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestHTTPModeDerivesCallback(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{
		"ARGOCD_SERVER_URL": "https://argocd.example.com",
		"MCP_TRANSPORT":     "http",
		"MCP_PUBLIC_URL":    "https://mcp.example.com/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDC.RedirectURL != "https://mcp.example.com/auth/callback" {
		t.Errorf("redirect = %q", c.OIDC.RedirectURL)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"missing server":   {},
		"http server":      {"ARGOCD_SERVER_URL": "http://argocd.example.com"},
		"http public url":  {"ARGOCD_SERVER_URL": "https://a.example.com", "MCP_TRANSPORT": "http", "MCP_PUBLIC_URL": "http://mcp.example.com"},
		"http no public":   {"ARGOCD_SERVER_URL": "https://a.example.com", "MCP_TRANSPORT": "http"},
		"bad transport":    {"ARGOCD_SERVER_URL": "https://a.example.com", "MCP_TRANSPORT": "sse"},
		"issuer no client": {"ARGOCD_SERVER_URL": "https://a.example.com", "OIDC_ISSUER_URL": "https://login.microsoftonline.com/t/v2.0"},
		"bad bool":         {"ARGOCD_SERVER_URL": "https://a.example.com", "OIDC_OPEN_BROWSER": "maybe"},
		"bad duration":     {"ARGOCD_SERVER_URL": "https://a.example.com", "OIDC_LOGIN_WAIT": "soon"},
		"remote http cb":   {"ARGOCD_SERVER_URL": "https://a.example.com", "OIDC_REDIRECT_URL": "http://mcp.example.com/cb"},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFrom(env(e)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestScopesParsing(t *testing.T) {
	c, err := LoadFrom(env(map[string]string{"ARGOCD_SERVER_URL": "https://a.example.com", "OIDC_SCOPES": "openid, profile email"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.OIDC.Scopes, "|") != "openid|profile|email" {
		t.Errorf("scopes = %v", c.OIDC.Scopes)
	}
}
