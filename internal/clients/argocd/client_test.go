package argocd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authargocd "github.com/vinaycharlie01/argocd-mcp-go/internal/auth/argocd"
)

const testToken = "user-token-123"

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c, err := New(srv.URL, authargocd.Static(testToken), Options{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func jsonResp(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestBearerTokenSent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("no cookies expected")
		}
		jsonResp(w, `{"items":[]}`)
	})
	if _, err := c.ListApplications(context.Background(), ListOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestListApplicationsQueryAndParse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications" {
			t.Errorf("path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if strings.Join(q["projects"], ",") != "a,b" || q.Get("appNamespace") != "team" || q.Get("selector") != "x=y" {
			t.Errorf("query %v", q)
		}
		jsonResp(w, `{"items":[{"metadata":{"name":"web","namespace":"argocd"},"spec":{"project":"a",
			"source":{"repoURL":"https://git.example.com/r","path":"k8s","targetRevision":"main"},
			"destination":{"server":"https://kubernetes.default.svc","namespace":"web"}},
			"status":{"sync":{"status":"Synced","revision":"abc"},"health":{"status":"Healthy"},"unknownField":1}}]}`)
	})
	apps, err := c.ListApplications(context.Background(), ListOptions{Projects: []string{"a", "b"}, AppNamespace: "team", Selector: "x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Metadata.Name != "web" || apps[0].Spec.Source.Path != "k8s" || apps[0].Status.Health.Status != "Healthy" {
		t.Fatalf("%+v", apps)
	}
}

func TestPathsAndEscaping(t *testing.T) {
	var gotPath, gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.EscapedPath(), r.URL.RawQuery
		switch {
		case strings.HasSuffix(r.URL.Path, "/resource-tree"):
			jsonResp(w, `{"nodes":[{"kind":"Pod","name":"p1","health":{"status":"Healthy"}}]}`)
		case strings.HasSuffix(r.URL.Path, "/manifests"):
			jsonResp(w, `{"manifests":["{\"kind\":\"Service\"}"],"revision":"v1"}`)
		case strings.HasSuffix(r.URL.Path, "/events"):
			jsonResp(w, `{"items":[{"type":"Warning","reason":"BackOff","message":"m","involvedObject":{"kind":"Pod","name":"p1"}}]}`)
		default:
			jsonResp(w, `{"metadata":{"name":"web"}}`)
		}
	})
	ctx := context.Background()
	ref := AppRef{Name: "web", AppNamespace: "team", Project: "p"}

	if _, err := c.GetApplication(ctx, ref); err != nil || gotPath != "/api/v1/applications/web" || gotQuery != "appNamespace=team&project=p" {
		t.Fatalf("get: %v %s?%s", err, gotPath, gotQuery)
	}
	tree, err := c.GetResourceTree(ctx, ref)
	if err != nil || gotPath != "/api/v1/applications/web/resource-tree" || tree.Nodes[0].Name != "p1" {
		t.Fatalf("tree: %v %s", err, gotPath)
	}
	m, err := c.GetManifests(ctx, ref, "v1.2")
	if err != nil || !strings.Contains(gotQuery, "revision=v1.2") || m.Revision != "v1" {
		t.Fatalf("manifests: %v %s", err, gotQuery)
	}
	ev, err := c.ListEvents(ctx, ref, EventFilter{ResourceName: "p1", ResourceNamespace: "web"})
	if err != nil || !strings.Contains(gotQuery, "resourceName=p1") || ev[0].Reason != "BackOff" {
		t.Fatalf("events: %v %s", err, gotQuery)
	}
	// Path traversal attempts stay inside the segment.
	_, _ = c.GetApplication(ctx, AppRef{Name: "../settings"})
	if gotPath != "/api/v1/applications/..%2Fsettings" {
		t.Fatalf("segment not escaped: %s", gotPath)
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		target error
		msg    string
	}{
		{http.StatusUnauthorized, `{"message":"invalid session"}`, ErrUnauthorized, "Refresh ARGOCD_AUTH_TOKEN"},
		{http.StatusForbidden, `{"message":"permission denied: applications, get"}`, ErrForbidden, "permission denied: applications, get"},
		{http.StatusNotFound, `{"error":"app not found","message":"applications.argoproj.io \"x\" not found"}`, ErrNotFound, "not found"},
		{http.StatusBadGateway, `<html>bad gateway</html>`, nil, "HTTP 502"},
		{http.StatusFound, ``, nil, "HTTP 302"},
	}
	for _, tc := range cases {
		c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.status == http.StatusFound {
				w.Header().Set("Location", "https://evil.example.com/")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.GetApplication(context.Background(), AppRef{Name: "x"})
		if err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%d: got %v", tc.status, err)
		}
		if tc.target != nil && !errors.Is(err, tc.target) {
			t.Errorf("%d: errors.Is(%v) false", tc.status, tc.target)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("%d: token leaked in error", tc.status)
		}
	}
}

func TestNonJSONResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>login</html>"))
	})
	if _, err := c.ListApplications(context.Background(), ListOptions{}); err == nil || !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("got %v", err)
	}
}

func TestMissingTokenNoRequest(t *testing.T) {
	called := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	c, _ := New(srv.URL, authargocd.Static(""), Options{HTTPClient: srv.Client()})
	if _, err := c.ListApplications(context.Background(), ListOptions{}); !errors.Is(err, authargocd.ErrNoToken) {
		t.Fatalf("got %v", err)
	}
	if called {
		t.Fatal("no request should be sent without a token")
	}
}

func TestNetworkErrorAndTimeout(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.ListApplications(ctx, ListOptions{})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cannot reach Argo CD") {
		t.Fatalf("got %v", err)
	}

	dead, _ := New("https://127.0.0.1:1", authargocd.Static("t"), Options{Timeout: time.Second})
	if _, err := dead.ListApplications(context.Background(), ListOptions{}); err == nil {
		t.Fatal("expected network error")
	}
}

func TestDefaultClientRefusesRedirects(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	hc, err := newHTTPClient(Options{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := New(srv.URL, authargocd.Static("t"), Options{HTTPClient: hc})
	if _, err := c.ListApplications(context.Background(), ListOptions{}); err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("redirect should not be followed: %v", err)
	}
}

func TestDotSegmentsRejected(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("must not send request") })
	for _, n := range []string{"..", ".", ""} {
		if _, err := c.GetApplication(context.Background(), AppRef{Name: n}); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestBasePathPreserved(t *testing.T) {
	var got string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		jsonResp(w, `{"items":[]}`)
	}))
	defer srv.Close()
	c, _ := New(srv.URL+"/argo-cd/", authargocd.Static("t"), Options{HTTPClient: srv.Client()})
	if _, err := c.ListApplications(context.Background(), ListOptions{}); err != nil || got != "/argo-cd/api/v1/applications" {
		t.Fatalf("%v %s", err, got)
	}
}

func TestGetLogsStream(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/api/v1/applications/web/logs" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"content":"line 1","timeStampStr":"2026-01-01T00:00:00Z","podName":"web-1"}}
{"result":{"content":"line 2","podName":"web-2"}}
{"result":{"content":"","last":true}}
{"result":{"content":"after last must be ignored"}}
`))
	})
	res, err := c.GetLogs(context.Background(), AppRef{Name: "web"}, LogOptions{Namespace: "web", Kind: "Deployment", Group: "apps", ResourceName: "web", TailLines: 100, Previous: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Lines) != 2 || res.Lines[1].Pod != "web-2" || res.Truncated {
		t.Fatalf("%+v", res)
	}
	for _, want := range []string{"follow=false", "kind=Deployment", "group=apps", "resourceName=web", "tailLines=100", "previous=true", "namespace=web"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %s", gotQuery, want)
		}
	}
}

func TestGetLogsTruncatesAndStreamErrors(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"content":"0123456789"}}{"result":{"content":"0123456789"}}`))
	})
	res, err := c.GetLogs(context.Background(), AppRef{Name: "web"}, LogOptions{PodName: "p", MaxBytes: 15})
	if err != nil || len(res.Lines) != 1 || !res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}

	c = newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"code":7,"message":"permission denied: logs, get"}}`))
	})
	if _, err := c.GetLogs(context.Background(), AppRef{Name: "web"}, LogOptions{PodName: "p"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestGetResource(t *testing.T) {
	var q string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications/web/resource" {
			t.Errorf("path %s", r.URL.Path)
		}
		q = r.URL.RawQuery
		jsonResp(w, `{"manifest":"{\"kind\":\"Pod\",\"status\":{\"phase\":\"Running\"}}"}`)
	})
	obj, err := c.GetResource(context.Background(), AppRef{Name: "web", Project: "p"}, ResourceQuery{Namespace: "ns", Name: "web-1", Version: "v1", Kind: "Pod"})
	if err != nil || obj["kind"] != "Pod" {
		t.Fatalf("%v %v", err, obj)
	}
	for _, want := range []string{"namespace=ns", "resourceName=web-1", "version=v1", "kind=Pod", "project=p"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %s", q, want)
		}
	}
	if strings.Contains(q, "group=") {
		t.Errorf("empty group must be omitted: %s", q)
	}
}
