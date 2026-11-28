package auth

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

var resultPage = template.Must(template.New("result").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title>
<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#1f2328}
h1{font-size:1.4rem}.ok{color:#1a7f37}.err{color:#cf222e}</style></head>
<body><h1 class="{{.Class}}">{{.Title}}</h1><p>{{.Message}}</p></body></html>`))

// CallbackHandler serves the OAuth2 redirect URI. It is mounted on the MCP
// HTTP server in remote mode, or on a loopback listener in local mode.
func (m *Manager) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		q := r.URL.Query()
		identity, err := m.complete(r.Context(), q.Get("state"), q.Get("code"), q.Get("error"), q.Get("error_description"))
		if err != nil {
			m.log.Warn("login callback failed", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			_ = resultPage.Execute(w, map[string]string{
				"Title": "Login failed", "Class": "err",
				"Message": err.Error() + ". Return to your MCP client and run the login tool again.",
			})

			return
		}
		_ = resultPage.Execute(w, map[string]string{
			"Title": "Login successful", "Class": "ok",
			"Message": fmt.Sprintf("Signed in as %s. You can close this tab and return to your MCP client.", identity.DisplayName()),
		})
	})
}

// LocalCallbackServer serves the redirect URI on the loopback interface for
// the stdio transport. It starts on first use and stays up for the process
// lifetime so a user can finish a login after the login tool returns.
type LocalCallbackServer struct {
	m *Manager

	once sync.Once
	srv  *http.Server
	err  error
}

// NewLocalCallbackServer returns a lazily-started loopback callback server.
func NewLocalCallbackServer(m *Manager) *LocalCallbackServer {
	return &LocalCallbackServer{m: m}
}

// Ensure starts the listener if it is not running yet.
func (l *LocalCallbackServer) Ensure(ctx context.Context) error {
	l.once.Do(func() {
		redirect, err := l.m.RedirectURL(ctx)
		if err != nil {
			l.err = err

			return
		}
		u, err := url.Parse(redirect)
		if err != nil {
			l.err = fmt.Errorf("parse redirect URL: %w", err)

			return
		}
		if !isLoopback(u.Hostname()) {
			l.err = fmt.Errorf("local mode needs a loopback redirect URL (http://localhost:<port>/...), got %q", redirect)

			return
		}
		mux := http.NewServeMux()
		mux.Handle(u.Path, l.m.CallbackHandler())
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", u.Host)
		if err != nil {
			l.err = fmt.Errorf("listen on %s for OAuth callback: %w", u.Host, err)

			return
		}
		l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := l.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				l.m.log.Error("callback server stopped", "error", err)
			}
		}()
		l.m.log.Info("OAuth callback listener started", "addr", ln.Addr().String(), "path", u.Path)
	})

	return l.err
}

// Close stops the listener.
func (l *LocalCallbackServer) Close(ctx context.Context) error {
	if l.srv == nil {
		return nil
	}

	return l.srv.Shutdown(ctx)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// OpenBrowser tries to open url in the user's default browser.
func OpenBrowser(ctx context.Context, u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", u)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", u)
	}

	return cmd.Start()
}
