package argocd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/config"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
)

func server(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func TestListApplicationsSendsUserToken(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer user-token" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}
		if got := r.URL.Query()["projects"]; len(got) != 2 || r.URL.Query().Get("selector") != "team=pay" {
			t.Errorf("query %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"web","namespace":"argocd"},
		  "spec":{"project":"pay","sources":[{"repoURL":"https://git/x","path":"k8s","targetRevision":"main"}],
		  "destination":{"server":"https://kubernetes.default.svc","namespace":"web"},"syncPolicy":{"automated":{}}},
		  "status":{"sync":{"status":"Synced","revision":"abc"},"health":{"status":"Healthy"},"operationState":{"phase":"Succeeded"}}}]}`))
	})
	apps, err := c.ListApplications(context.Background(), "user-token", domain.ApplicationFilter{Projects: []string{"pay", "core"}, Selector: "team=pay"})
	if err != nil {
		t.Fatal(err)
	}
	want := domain.Application{Name: "web", Namespace: "argocd", Project: "pay", RepoURL: "https://git/x", Path: "k8s", TargetRevision: "main",
		DestServer: "https://kubernetes.default.svc", DestNamespace: "web", SyncStatus: "Synced", HealthStatus: "Healthy",
		Revision: "abc", OperationPhase: "Succeeded", AutoSync: true}
	if len(apps) != 1 || apps[0] != want {
		t.Fatalf("got %+v", apps)
	}
}

func TestErrorMapping(t *testing.T) {
	for code, want := range map[int]error{http.StatusUnauthorized: domain.ErrUnauthorized, http.StatusForbidden: domain.ErrForbidden} {
		c := server(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"permission denied"}`))
		})
		_, err := c.ListApplications(context.Background(), "t", domain.ApplicationFilter{})
		if !errors.Is(err, want) {
			t.Errorf("%d: got %v", code, err)
		}
	}
}

func TestResolverDirectOIDCPrefersCLIClient(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("settings must be fetched anonymously")
		}
		_, _ = w.Write([]byte(`{"url":"https://argocd.example.com","oidcConfig":{"issuer":"https://login.microsoftonline.com/tid/v2.0",
		  "clientID":"web-app","cliClientID":"cli-app","scopes":["openid","profile","email"]}}`))
	})
	pc, err := ProviderResolver(c, config.OIDCConfig{RedirectURL: config.DefaultLocalRedirectURL})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pc.IssuerURL != "https://login.microsoftonline.com/tid/v2.0" || pc.ClientID != "cli-app" || len(pc.Scopes) != 3 {
		t.Fatalf("got %+v", pc)
	}
}

func TestResolverDex(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"https://argocd.example.com","dexConfig":{"connectors":[{"id":"microsoft","type":"microsoft"}],"dexAuthConnectorID":"microsoft"}}`))
	})
	pc, err := ProviderResolver(c, config.OIDCConfig{RedirectURL: config.DefaultLocalRedirectURL})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pc.IssuerURL != "https://argocd.example.com/api/dex" || pc.ClientID != "argo-cd-cli" || pc.AuthParams["connector_id"] != "microsoft" {
		t.Fatalf("got %+v", pc)
	}
	if pc.Scopes[len(pc.Scopes)-1] != "federated:id" {
		t.Fatalf("scopes %v", pc.Scopes)
	}
}

func TestResolverOverridesSkipDiscovery(t *testing.T) {
	c := server(t, func(http.ResponseWriter, *http.Request) { t.Error("discovery should be skipped") })
	pc, err := ProviderResolver(c, config.OIDCConfig{IssuerURL: "https://idp", ClientID: "x", RedirectURL: "http://localhost:1/cb"})(context.Background())
	if err != nil || pc.ClientID != "x" {
		t.Fatalf("%+v %v", pc, err)
	}
}

func TestResolverNoSSO(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"url":"https://a"}`)) })
	if _, err := ProviderResolver(c, config.OIDCConfig{})(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
