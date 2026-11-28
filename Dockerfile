# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG GIT_COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/vinaycharlie01/argocd-mcp-go/pkg/version.Version=${VERSION} -X github.com/vinaycharlie01/argocd-mcp-go/pkg/version.GitCommit=${GIT_COMMIT}" \
    -o /out/argocd-mcp ./cmd/argocd-mcp

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/argocd-mcp /usr/local/bin/argocd-mcp
ENV MCP_TRANSPORT=http MCP_HTTP_ADDR=:8080 LOG_FORMAT=json
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/argocd-mcp"]
