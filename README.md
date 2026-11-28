# argocd-mcp-go

An [MCP](https://modelcontextprotocol.io) server for **Argo CD**, written in Go, with **per-user Microsoft Entra ID single sign-on**.

Each user signs in with their own Microsoft account. The server sends only that user's token to Argo CD, so **Argo CD's existing RBAC decides what they can see and do**. No shared admin token exists anywhere.

> [!NOTE]
> This first iteration exists to validate SSO. It ships `login`, `whoami`, `logout` and one platform tool, `list_applications`. More tools (app details, resources, logs, sync, refresh, rollback, projects, repositories) will follow once login is confirmed against the lower environment.

## Quick start (local, VS Code / Claude)

The only setting you must provide is the Argo CD URL:

```bash
export ARGOCD_SERVER_URL=https://argocd-lowerenv.qwikcilver.com
task build && ./dist/argocd-mcp      # or: go run ./cmd/argocd-mcp
```

In VS Code, open this folder and start the `argocd` server from **MCP: List Servers** (see `.vscode/mcp.json`). Then ask the agent to:

1. Run **`login`**. Your default browser (e.g. Chrome) opens the Microsoft sign-in page.
2. Sign in. The page shows "Login successful".
3. Run **`whoami`** to see your identity and groups, then **`list_applications`**.

Claude Code:

```bash
claude mcp add argocd -e ARGOCD_SERVER_URL=https://argocd-lowerenv.qwikcilver.com -- /path/to/dist/argocd-mcp
```

> [!TIP]
> Quick pre-check: if `argocd login argocd-lowerenv.qwikcilver.com --sso` works on your laptop, this server will work with no extra Entra configuration. It uses the same client and the same callback.

## How authentication works

```text
 MCP client ──tool call──▶ argocd-mcp ──GET /api/v1/settings (anonymous)──▶ Argo CD
                               │  discovers issuer, client ID, scopes
                               │
   browser ◀──── login URL ────┤  Authorization Code + PKCE (S256), state, nonce
      │                        │
      └─▶ Entra ID (or Argo CD's Dex → Entra ID) ──redirect──▶ http://localhost:8085/auth/callback
                               │  code exchange, ID-token signature/issuer/audience/nonce verified
                               │
                               └──Authorization: Bearer <user's ID token>──▶ Argo CD API (RBAC enforced)
```

The server resolves the OIDC client the same way `argocd login --sso` does (`pkg/apiclient.OIDCConfig` in argo-cd):

| Argo CD SSO setup | Issuer | Client ID | Scopes |
|---|---|---|---|
| `oidc.config` (Entra ID directly) | `oidcConfig.issuer`, e.g. `https://login.microsoftonline.com/<tenant>/v2.0` | `cliClientID`, else `clientID` | `oidcConfig.scopes`, else `openid profile email groups` |
| `dex.config` (Entra ID as a Dex connector) | `<argocd-url>/api/dex` | `argo-cd-cli` | default scopes plus `federated:id` |

`offline_access` is added when the provider supports it, so tokens refresh silently. Argo CD validates the **ID token**: its signature, its issuer, and that the audience is one of `clientID`, `cliClientID` or `allowedAudiences`. It maps the `groups` claim through `policy.csv`.

Security properties:

- **PKCE S256** with a fresh verifier per login, plus a 256-bit `state` and `nonce`. A `state` is single-use and expires after 10 minutes.
- The ID token is verified locally (JWKS signature, issuer, audience, expiry, nonce) before it is stored.
- Tokens are kept **in memory only**, per session. They are never written to disk or logs, and logs show only the username and group count.
- The callback page is served with `no-store`, a strict CSP and `no-referrer`, and error text is HTML-escaped.
- In local mode the callback listens on loopback only. Remote HTTP endpoints must use HTTPS.
- The callback URL (`/auth/callback`) is distinct from any logout URL. `logout` drops tokens and returns the IdP's `end_session_endpoint` (from discovery) if you also want to end the browser session.

## Entra ID / Argo CD administrator checklist

| Item | Where to find it | Needed for |
|---|---|---|
| Tenant ID | Part of `oidcConfig.issuer` (`…/<tenant-id>/v2.0`), or the Dex connector `tenant` | discovered automatically |
| Client ID | `cliClientID` / `clientID` in `argocd-cm` → `oidc.config` | discovered automatically |
| Redirect URI | `http://localhost:8085/auth/callback` (local). For HTTP mode: `<MCP_PUBLIC_URL>/auth/callback` | **must be registered** |
| Scopes | `oidc.config.requestedScopes` | discovered automatically |
| Groups claim | App registration → Token configuration → groups claim | Argo CD RBAC |

Admin actions, depending on the setup:

- **Direct OIDC:** on the app registration used as `cliClientID` (or `clientID`), add the platform **Mobile and desktop applications** with redirect URI `http://localhost:8085/auth/callback`, and enable **Allow public client flows**. The server needs no client secret. If you use a separate CLI app registration, put it in `cliClientID` so Argo CD accepts its audience.
- **Dex:** nothing for local mode, because Dex's built-in `argo-cd-cli` client already allows `http://localhost:8085/auth/callback`. HTTP mode needs an extra Dex static client with the public callback URL.
- **HTTP mode with Entra:** register `<MCP_PUBLIC_URL>/auth/callback` as a redirect URI. A web platform redirect requires `OIDC_CLIENT_SECRET`; a public-client redirect does not.

You can inspect the live configuration yourself:

```bash
curl -s https://argocd-lowerenv.qwikcilver.com/api/v1/settings | jq '{url, oidcConfig, dexConfig}'
```

## Transports

| Mode | Use | Identity per… |
|---|---|---|
| `stdio` (default) | Local, one user, launched by the MCP client | process |
| `http` (Streamable HTTP at `/mcp`) | Shared deployment | MCP session: every session logs in separately |

```bash
MCP_TRANSPORT=http MCP_PUBLIC_URL=https://argocd-mcp.example.com \
ARGOCD_SERVER_URL=https://argocd-lowerenv.qwikcilver.com ./dist/argocd-mcp
```

Docker:

```bash
task docker
docker run --rm -p 8080:8080 \
  -e ARGOCD_SERVER_URL=https://argocd-lowerenv.qwikcilver.com \
  -e MCP_PUBLIC_URL=https://argocd-mcp.example.com \
  argocd-mcp-go:dev
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `ARGOCD_SERVER_URL` | — (required) | Argo CD base URL (https) |
| `ARGOCD_CA_FILE` | | Extra CA bundle (PEM) |
| `ARGOCD_INSECURE_SKIP_VERIFY` | `false` | Skip TLS verification (lower envs only) |
| `ARGOCD_REQUEST_TIMEOUT` | `30s` | HTTP timeout |
| `OIDC_ISSUER_URL` / `OIDC_CLIENT_ID` | discovered | Override discovery |
| `OIDC_CLIENT_SECRET` | | Only for confidential clients |
| `OIDC_SCOPES` | discovered | Comma or space separated |
| `OIDC_REDIRECT_URL` | `http://localhost:8085/auth/callback`, or `<MCP_PUBLIC_URL>/auth/callback` in HTTP mode | OAuth2 callback |
| `OIDC_DOMAIN_HINT` | | Entra `domain_hint` (e.g. `qwikcilver.com`) |
| `OIDC_LOGIN_TIMEOUT` | `10m` | Validity of a started login |
| `OIDC_LOGIN_WAIT` | `2m` | How long `login` blocks waiting for the browser |
| `OIDC_OPEN_BROWSER` | `true` | Open the browser automatically (stdio) |
| `MCP_TRANSPORT` | `stdio` | `stdio` or `http` |
| `MCP_HTTP_ADDR` | `:8080` | Listen address (http) |
| `MCP_PUBLIC_URL` | | External base URL (required for http) |
| `MCP_JSON_RESPONSE` | `false` | JSON instead of SSE responses |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Structured logs (stderr) |

## Architecture (hexagonal)

```text
cmd/argocd-mcp            composition root (wiring, signals, HTTP server)
internal/core/domain      entities and domain errors
internal/core/ports       interfaces: ArgoCD API, TokenProvider
internal/core/service     use cases (list applications)
internal/adapters/argocd  outbound: Argo CD REST client and SSO discovery
internal/adapters/mcp     inbound: MCP tools, stdio and Streamable HTTP
internal/auth             OIDC/PKCE login, token store, refresh, callback
internal/config           environment configuration and validation
internal/logging          slog setup
```

## Development

```bash
task test     # unit tests (includes an end-to-end PKCE login against a fake IdP)
task race
task lint
task build
```

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `AADSTS50011` redirect URI mismatch | Register `http://localhost:8085/auth/callback` (Mobile and desktop) on the app registration |
| `AADSTS7000218` client_secret required | The redirect is registered as Web: register it as Mobile and desktop and enable public client flows, or set `OIDC_CLIENT_SECRET` |
| `listen on localhost:8085: address already in use` | Another `argocd login --sso` is running, or set `OIDC_REDIRECT_URL` to another registered port |
| `401` after login | The token's audience is not accepted by Argo CD: add the client ID to `cliClientID` or `allowedAudiences` |
| Empty application list | Signed in correctly, but RBAC grants no `applications, get` permission to your groups |
