package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vinaycharlie01/argocd-mcp-go/internal/auth"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/core/domain"
)

type loginInput struct {
	Force bool `json:"force,omitempty" jsonschema:"Start a new login even if already signed in."`
}

type loginOutput struct {
	Status   string         `json:"status"`
	Message  string         `json:"message"`
	LoginURL string         `json:"loginUrl,omitempty"`
	User     *auth.Identity `json:"user,omitempty"`
}

type emptyInput struct{}

type whoamiOutput struct {
	Authenticated bool           `json:"authenticated"`
	User          *auth.Identity `json:"user,omitempty"`
}

type logoutOutput struct {
	LoggedOut     bool   `json:"loggedOut"`
	Message       string `json:"message"`
	EndSessionURL string `json:"endSessionUrl,omitempty"`
}

type listAppsInput struct {
	Projects     []string `json:"projects,omitempty" jsonschema:"Only applications in these Argo CD projects."`
	Selector     string   `json:"selector,omitempty" jsonschema:"Kubernetes label selector, e.g. team=payments."`
	AppNamespace string   `json:"appNamespace,omitempty" jsonschema:"Namespace of the Application resources (apps-in-any-namespace)."`
	Search       string   `json:"search,omitempty" jsonschema:"Case-insensitive substring match on application name."`
}

type listAppsOutput struct {
	Count        int                  `json:"count"`
	Applications []domain.Application `json:"applications"`
}

var (
	readOnly    = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(true)}
	authOnlyAnn = &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(true)}
)

func ptr[T any](v T) *T { return &v }

func (s *Server) registerTools() {
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "login",
		Title:       "Sign in to Argo CD with Microsoft Entra ID",
		Description: "Starts a browser-based Microsoft Entra ID (OIDC, Authorization Code + PKCE) login for the current user. Show the returned loginUrl to the user if the browser did not open. Waits for the login to complete.",
		Annotations: authOnlyAnn,
	}, s.login)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "whoami",
		Title:       "Current Argo CD user",
		Description: "Shows whether the current MCP session is signed in and as whom (claims from the Entra ID token, including groups used by Argo CD RBAC).",
		Annotations: readOnly,
	}, s.whoami)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "logout",
		Title:       "Sign out",
		Description: "Discards this session's tokens from the MCP server's memory.",
		Annotations: authOnlyAnn,
	}, s.logout)

	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "list_applications",
		Title:       "List Argo CD applications",
		Description: "Lists Argo CD applications the signed-in user may see (Argo CD RBAC applies), with sync and health status.",
		Annotations: readOnly,
	}, s.listApplications)
}

func (s *Server) login(ctx context.Context, req *mcp.CallToolRequest, in loginInput) (*mcp.CallToolResult, loginOutput, error) {
	key, err := s.sessionKey(req)
	if err != nil {
		return nil, loginOutput{}, err
	}
	if !in.Force {
		if _, err := s.opts.Auth.BearerToken(ctx, key); err == nil {
			id, _ := s.opts.Auth.Identity(key)

			return nil, loginOutput{Status: "authenticated", Message: "Already signed in as " + id.DisplayName(), User: id}, nil
		}
	} else {
		s.opts.Auth.Logout(key)
	}

	if s.opts.Local && s.opts.EnsureCallback != nil {
		if err := s.opts.EnsureCallback(ctx); err != nil {
			return nil, loginOutput{}, err
		}
	}
	attempt, err := s.opts.Auth.StartLogin(ctx, key)
	if err != nil {
		return nil, loginOutput{}, err
	}

	opened := false
	if s.opts.Local && s.opts.OpenBrowser {
		if err := auth.OpenBrowser(context.WithoutCancel(ctx), attempt.AuthURL); err != nil {
			s.log.Warn("could not open browser", "error", err)
		} else {
			opened = true
		}
	}

	timer := time.NewTimer(s.opts.LoginWait)
	defer timer.Stop()
	select {
	case <-attempt.Done:
		id, err := attempt.Result()
		if err != nil {
			return nil, loginOutput{}, fmt.Errorf("login failed: %w", err)
		}

		return nil, loginOutput{Status: "authenticated", Message: "Signed in as " + id.DisplayName(), User: id}, nil
	case <-timer.C:
	case <-ctx.Done():
	}

	msg := "Open loginUrl in a browser and sign in with your Microsoft account, then call login again (or any tool) to continue."
	if opened {
		msg = "A browser window was opened for sign-in. Finish signing in there, then call login again to confirm. If no window appeared, open loginUrl."
	}

	return nil, loginOutput{Status: "pending", Message: msg, LoginURL: attempt.AuthURL}, nil
}

func (s *Server) whoami(ctx context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, whoamiOutput, error) {
	key, err := s.sessionKey(req)
	if err != nil {
		return nil, whoamiOutput{}, err
	}
	if _, err := s.opts.Auth.BearerToken(ctx, key); err != nil {
		return nil, whoamiOutput{Authenticated: false}, nil
	}
	id, _ := s.opts.Auth.Identity(key)

	return nil, whoamiOutput{Authenticated: true, User: id}, nil
}

func (s *Server) logout(_ context.Context, req *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, logoutOutput, error) {
	key, err := s.sessionKey(req)
	if err != nil {
		return nil, logoutOutput{}, err
	}
	endSession, was := s.opts.Auth.Logout(key)
	out := logoutOutput{LoggedOut: was, Message: "Tokens discarded from this MCP session."}
	if !was {
		out.Message = "This session was not signed in."
	}
	if endSession != "" {
		out.EndSessionURL = endSession
		out.Message += " To also end your browser SSO session, open endSessionUrl."
	}

	return nil, out, nil
}

func (s *Server) listApplications(ctx context.Context, req *mcp.CallToolRequest, in listAppsInput) (*mcp.CallToolResult, listAppsOutput, error) {
	key, err := s.sessionKey(req)
	if err != nil {
		return nil, listAppsOutput{}, err
	}
	apps, err := s.opts.Apps.List(ctx, key, domain.ApplicationFilter{
		Projects: in.Projects, Selector: in.Selector, AppNamespace: in.AppNamespace, Search: in.Search,
	})
	if err != nil {
		return nil, listAppsOutput{}, userError(err)
	}

	return nil, listAppsOutput{Count: len(apps), Applications: apps}, nil
}

// userError turns auth failures into actionable messages for the model.
func userError(err error) error {
	switch {
	case errors.Is(err, auth.ErrNotAuthenticated), errors.Is(err, domain.ErrUnauthorized):
		return fmt.Errorf("%w. Call the `login` tool to sign in with Microsoft Entra ID", err)
	default:
		return err
	}
}
