package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/auth"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/service"
)

type fakeAuth struct{ token string }

func (f *fakeAuth) StartLogin(context.Context, string) (*auth.LoginAttempt, error) {
	return nil, nil
}

func (f *fakeAuth) BearerToken(context.Context, string) (string, error) {
	if f.token == "" {
		return "", auth.ErrNotAuthenticated
	}

	return f.token, nil
}

func (f *fakeAuth) Identity(string) (*auth.Identity, bool) {
	return &auth.Identity{Subject: "u1", PreferredUsername: "jane"}, f.token != ""
}
func (f *fakeAuth) Logout(string) (string, bool)  { return "", true }
func (f *fakeAuth) CallbackHandler() http.Handler { return http.NotFoundHandler() }
func (f *fakeAuth) RedirectURL(context.Context) (string, error) {
	return "http://localhost:8085/auth/callback", nil
}

type fakeAPI struct{ gotToken string }

func (f *fakeAPI) SSOSettings(context.Context) (*domain.SSOSettings, error) {
	return &domain.SSOSettings{}, nil
}
func (f *fakeAPI) ListApplications(_ context.Context, bearer string, _ domain.ApplicationFilter) ([]domain.Application, error) {
	f.gotToken = bearer

	return []domain.Application{{Name: "b-app", Project: "p"}, {Name: "a-app", Project: "p"}}, nil
}

func connect(t *testing.T, s *Server) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	if _, err := s.MCPServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return cs
}

func TestToolsRegistered(t *testing.T) {
	cs := connect(t, New(Options{Auth: &fakeAuth{}, Local: true}))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "list_applications,login,logout,whoami" {
		t.Fatalf("tools: %v", names)
	}
}

func TestListApplicationsRequiresLogin(t *testing.T) {
	api := &fakeAPI{}
	fa := &fakeAuth{}
	cs := connect(t, New(Options{Auth: fa, Apps: service.NewApplications(api, fa), Local: true}))
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_applications", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*sdk.TextContent).Text, "login") {
		t.Fatalf("expected login hint error, got %+v", res)
	}
}

func TestListApplicationsUsesUserToken(t *testing.T) {
	api := &fakeAPI{}
	fa := &fakeAuth{token: "jane-id-token"}
	cs := connect(t, New(Options{Auth: fa, Apps: service.NewApplications(api, fa), Local: true}))
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "list_applications", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if api.gotToken != "jane-id-token" {
		t.Fatalf("API called with %q", api.gotToken)
	}
	var out listAppsOutput
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	if out.Count != 2 || out.Applications[0].Name != "a-app" {
		t.Fatalf("unexpected output %+v", out)
	}
}

func TestWhoamiLoggedOut(t *testing.T) {
	cs := connect(t, New(Options{Auth: &fakeAuth{}, Local: true}))
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(res.Content[0].(*sdk.TextContent).Text, `"authenticated":false`) {
		t.Fatalf("got %+v", res.Content[0])
	}
}
