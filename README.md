# argocd-mcp-go

A read-only [MCP](https://modelcontextprotocol.io) server, written in Go, that lets AI clients such as Claude Desktop and VS Code inspect **Argo CD**. Every call runs with **your own Argo CD credentials**, so Argo CD's RBAC decides what you can see.

Jenkins support will be added to the same server as a second, independent integration.

## Tools

| Tool | Argo CD API | What it returns |
|---|---|---|
| `argocd_list_applications` | `GET /api/v1/applications` | Apps with sync/health status. Filters: `projects`, `appNamespace`, `selector`, `search` |
| `argocd_get_application` | `GET /api/v1/applications/{name}` | Sources, destination, sync policy, conditions, last operation, images, out-of-sync/unhealthy resources |
| `argocd_get_application_resources` | `GET …/{name}/resource-tree` | Live resources (Deployments, ReplicaSets, Pods, …) with health, sync status and parents. Filters: `kind`, `health` |
| `argocd_get_application_history` | `status.history` | Deployment history, newest first (ID, revision, time, initiator) |
| `argocd_get_application_manifests` | `GET …/{name}/manifests` | Rendered target manifests, optionally at a `revision`. **Secret data is masked by Argo CD.** Capped at 1 MB |
| `argocd_get_application_events` | `GET …/{name}/events` | Kubernetes events, newest first, for the app or one resource |
| `argocd_get_pod_logs` | `GET …/{name}/logs` | Container logs for one **pod** (`podName`) or a whole **workload** (`kind` + `resourceName`, all pods). Supports `tailLines`, `sinceSeconds`, `container`, `previous`, `filter`. Capped at 1 MB |
| `argocd_get_images` | `…/resource-tree` + `…/resource` (Pods) + `…/manifests` | Images per workload and container: **desired** (Git), **running tag and registry digest** (with pod names), mixed-rollout and drift flags. Optional `expected` images give a verdict (see below) |

## Verifying the deployed image (`argocd_get_images`)

For each workload container this tool reports:
- the image **Git wants** (from the rendered manifests);
- the image tag and **registry digest** each pod **actually runs**. The digest comes from the pod's `status.containerStatuses[].imageID`; local image IDs such as `docker://sha256:…` are ignored because they can't be compared;
- `mixedRollout`: pods disagree on the image;
- `drift`: the running image differs from Git.

Pass `expected` images, for example the output of `jenkins_get_build_images` from [jenkins-mcp-go](https://github.com/vinaycharlie01/jenkins-mcp-go), to get a deterministic verdict:

| Verdict | Meaning |
|---|---|
| `MATCH` | Every pod runs exactly the expected digest |
| `MATCH_TAG_ONLY` | The tags match, but no digest was available to verify |
| `MIXED_ROLLOUT` | Only some pods run the expected image: a rollout is in progress or stuck |
| `TAG_MATCH_DIGEST_DIFFERENT` | Same tag, different image: the tag was re-pushed, pods weren't restarted, or a multi-arch index digest is being compared with a platform digest |
| `DIFFERENT_TAG` | The expected image isn't running. The hint says whether Git already has it, so check the rollout, or not, so it isn't deployed yet |
| `NOT_FOUND` | No container of the app uses that repository |

Image names are normalized before comparing (`nginx` = `docker.io/library/nginx`). Pod lookups are capped by `maxPods` (default 30).

All tools are read-only. **No Kubernetes cluster access is needed.** Argo CD fetches events and logs on your behalf. Pod logs need the Argo CD RBAC permission `logs, get`.

## Authentication

```text
Browser SSO (Microsoft Entra ID) ─▶ Argo CD sets cookie argocd.token
                                          │ you copy the value once
                                          ▼
                                 ARGOCD_AUTH_TOKEN (or ARGOCD_AUTH_TOKEN_FILE)
                                          │
MCP tool ─▶ REST client ─▶ GET https://<server>/api/v1/...   Authorization: Bearer <token>
```

- The token is **your own** Argo CD session token, so Argo CD applies your RBAC. The server holds no shared or admin credential and implements no OAuth flow or callback.
- **No gRPC-Web is needed.** The `argocd` CLI needs `--grpc-web` because it speaks gRPC, which many ingresses don't pass through. This server uses the REST/JSON API that the Argo CD web UI uses, over plain HTTPS. If the UI works, this works.
- **Token rotation:** session tokens expire, usually after about an hour, depending on the SSO configuration. An expired token gives an `unauthorized` error telling you to refresh it. With `ARGOCD_AUTH_TOKEN_FILE`, the file is re-read on every request, so rotating means overwriting the file, with no restart.
- **Never exposed:** the token is never logged. Log attributes whose names look like secrets are redacted. Tokens never appear in tool results or error messages. HTTP redirects are not followed, so the token can't be sent to another host. Plain `http://` servers are refused.

### Getting your token

1. Sign in to the Argo CD web UI with SSO.
2. Open Chrome DevTools → **Application** → **Cookies** → your Argo CD host.
3. Copy the value of `argocd.token`.
4. Check it works: `argocd app list --server <host> --grpc-web --auth-token "$ARGOCD_AUTH_TOKEN"`

> [!WARNING]
> The token is a credential that acts as you. Keep it in `.env` (git-ignored), in a file readable only by you, or in your MCP client's secret input. Never commit it or paste it into chat.

> [!IMPORTANT]
> Copying a browser session token is meant for **local, single-user** use. Shared or production deployments must use an approved identity, for example an Argo CD project role token or an approved service identity with least-privilege RBAC. They must not use a person's session.

## Quick start

Requirements: Go 1.27+ and [Task](https://taskfile.dev) (`go install github.com/go-task/task/v3/cmd/task@latest`).

```bash
cp .env.example .env          # then fill in ARGOCD_SERVER and ARGOCD_AUTH_TOKEN
task build                    # -> dist/devops-mcp
task smoke                    # starts the server over stdio and lists the tools
task test                     # unit tests, no credentials needed
```

## Connecting a client (stdio)

### VS Code

Open this folder. `.vscode/mcp.json` defines the `devops` server. Start it from **MCP: List Servers**. VS Code prompts for the server host and the token, and stores the token as a secret.

### Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json` on macOS, or `%APPDATA%\Claude\claude_desktop_config.json` on Windows:

```json
{
  "mcpServers": {
    "devops": {
      "command": "/absolute/path/to/dist/devops-mcp",
      "env": {
        "ARGOCD_SERVER": "argocd.example.com",
        "ARGOCD_AUTH_TOKEN_FILE": "/Users/you/.config/devops-mcp/argocd-token"
      }
    }
  }
}
```

Using `ARGOCD_AUTH_TOKEN_FILE` means you can refresh the token without editing this file or restarting Claude.

### Claude Code

```bash
claude mcp add devops \
  -e ARGOCD_SERVER=argocd.example.com \
  -e ARGOCD_AUTH_TOKEN_FILE=$HOME/.config/devops-mcp/argocd-token \
  -- /absolute/path/to/dist/devops-mcp
```

### Docker (stdio)

```bash
task docker
docker run -i --rm --env-file .env devops-mcp:dev
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `ARGOCD_SERVER` | — | Host (`argocd.example.com`, `host:port`) or `https://` URL. Enables the Argo CD tools |
| `ARGOCD_AUTH_TOKEN` | — | Your Argo CD token (secret) |
| `ARGOCD_AUTH_TOKEN_FILE` | — | File containing the token, re-read on every request. Takes precedence over `ARGOCD_AUTH_TOKEN` |
| `ARGOCD_CA_FILE` | — | Extra CA bundle (PEM) for private CAs |
| `ARGOCD_INSECURE` | `false` | Skip TLS verification (lower environments only) |
| `ARGOCD_REQUEST_TIMEOUT` | `30s` | Per-request timeout |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` or `text`. Logs go to stderr, because stdout carries the MCP stream |

## Errors

Tool errors are returned as MCP error results whose text is a JSON envelope:

```json
{"error":{"code":"unauthorized","message":"Argo CD rejected the token (401): it is missing, invalid or expired. Refresh ARGOCD_AUTH_TOKEN"}}
```

| Code | Meaning |
|---|---|
| `invalid_input` | The tool arguments failed validation. Argo CD was not called |
| `unauthorized` | The token is missing, invalid or expired |
| `forbidden` | Argo CD RBAC denies the action (e.g. `logs, get`) |
| `not_found` | The application doesn't exist, or you can't see it |
| `not_configured` | No token is configured, or the token file is empty |
| `timeout` | The request timed out |
| `upstream_error` | Network error or unexpected Argo CD response |

## Architecture

```text
cmd/server/                 composition root: config → auth → clients → tools → transport
internal/config/            environment loading and validation; each integration is optional
internal/auth/argocd/       bearer token source (static, or file re-read per request)
internal/clients/argocd/    REST client: typed errors, path escaping, streaming logs
internal/tools/argocd/      MCP tool definitions, input validation, response shaping
internal/imageref/          image reference parsing/normalization (tag, registry digest)
internal/toolkit/           shared error envelope and validators
internal/mcp/               builds the MCP server from independent tool sets
internal/transport/         stdio (an HTTP transport can be added here)
internal/logging/           slog with secret redaction
```

- The tools depend on a small interface rather than the HTTP client, so they're tested without a network.
- Jenkins will add `auth/jenkins`, `clients/jenkins` and `tools/jenkins` without touching the Argo CD packages.

## Development

```bash
task test        # unit tests (httptest mock servers, no credentials)
task race        # with the race detector
task vet
task lint        # golangci-lint
```
