package argocd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/loganalysis"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

// Analysis modes.
const (
	ModeOverview   = "overview"
	ModeLevels     = "levels"
	ModeExceptions = "exceptions"
	ModePatterns   = "patterns"
	ModeHTTP       = "http"
	ModeQueueURLs  = "queue_urls"
	ModeSearch     = "search"
	ModeSample     = "sample"
	ModeAll        = "all"
)

var analysisModes = map[string]bool{ModeOverview: true, ModeLevels: true, ModeExceptions: true, ModePatterns: true,
	ModeHTTP: true, ModeQueueURLs: true, ModeSearch: true, ModeSample: true, ModeAll: true}

const (
	analysisMaxBytes   = 8 << 20 // log bytes fetched for analysis
	defaultTailAnalyze = 2000
	maxTailAnalyze     = 20000
	defaultMaxResults  = 20
	maxMaxResults      = 200
	maxOffset          = 100000
	maxContextLines    = 5
	maxSince           = 7 * 24 * time.Hour
	maxPodsListed      = 50
)

type analyzeInput struct {
	Application  string `json:"application" jsonschema:"Argo CD application name."`
	AppNamespace string `json:"app_namespace,omitempty" jsonschema:"Namespace of the Application resource (apps-in-any-namespace only)."`
	Project      string `json:"project,omitempty" jsonschema:"Argo CD project (optional)."`
	Deployment   string `json:"deployment,omitempty" jsonschema:"Deployment name: analyses all of its pods. Required unless pod is given."`
	Pod          string `json:"pod,omitempty" jsonschema:"Analyse only this pod."`
	Namespace    string `json:"namespace,omitempty" jsonschema:"Namespace of the deployment/pod (defaults to the application's destination namespace)."`
	Container    string `json:"container,omitempty" jsonschema:"Container name (default: the pod's default container)."`
	AnalysisType string `json:"analysis_type,omitempty" jsonschema:"overview (default), levels, exceptions, patterns, http, queue_urls, search, sample (raw lines), or all."`
	Search       string `json:"search,omitempty" jsonschema:"Keyword for analysis_type=search (case-insensitive). Also applied in 'all' when set."`
	Level        string `json:"level,omitempty" jsonschema:"Minimum level for search/sample: DEBUG, INFO, WARN, ERROR or FATAL."`
	Since        string `json:"since,omitempty" jsonschema:"Only logs newer than this: a duration like 15m, 2h, 1d (max 7d) or an RFC3339 timestamp."`
	TailLines    int    `json:"tail_lines,omitempty" jsonschema:"Lines per container to analyse, from the end (default 2000, max 20000)."`
	Previous     bool   `json:"previous,omitempty" jsonschema:"Analyse the previous (crashed) container instance."`
	ContextLines int    `json:"context_lines,omitempty" jsonschema:"For search: log lines of context before/after each match (0-5)."`
	MaxResults   int    `json:"max_results,omitempty" jsonschema:"Maximum items per result list (default 20, max 200)."`
	Offset       int    `json:"offset,omitempty" jsonschema:"Pagination offset for list results (use next_offset from the previous call)."`
}

type timeRange struct {
	Since string `json:"since,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

type analyzeSummary struct {
	LinesFetched  int      `json:"lines_fetched"`
	Entries       int      `json:"entries"`
	Pods          int      `json:"pods"`
	Errors        int      `json:"errors"`
	Warnings      int      `json:"warnings"`
	LogsTruncated bool     `json:"logs_truncated"`
	Notes         []string `json:"notes,omitempty"`
}

type analyzeResults struct {
	Overview   *loganalysis.Overview        `json:"overview,omitempty"`
	Levels     *loganalysis.LevelStats      `json:"levels,omitempty"`
	Exceptions []loganalysis.ExceptionGroup `json:"exceptions,omitempty"`
	Patterns   []loganalysis.PatternGroup   `json:"patterns,omitempty"`
	HTTP       *loganalysis.HTTPStats       `json:"http,omitempty"`
	QueueURLs  []loganalysis.QueueURL       `json:"queue_urls,omitempty"`
	Matches    []loganalysis.Match          `json:"matches,omitempty"`
}

type analyzeOutput struct {
	Application  string         `json:"application"`
	Deployment   string         `json:"deployment,omitempty"`
	Pod          string         `json:"pod,omitempty"`
	AnalysisType string         `json:"analysis_type"`
	TimeRange    timeRange      `json:"time_range"`
	Summary      analyzeSummary `json:"summary"`
	Results      analyzeResults `json:"results"`
	TotalMatches int            `json:"total_matches"`
	Truncated    bool           `json:"truncated"`
	NextOffset   *int           `json:"next_offset,omitempty"`
}

func registerAnalyzeLogs(s *mcp.Server, t *Tools) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "argocd_analyze_pod_logs",
		Title: "Analyse pod logs",
		Description: "Analyse the logs of a Deployment's pods (or one pod) through Argo CD and return compact structured results instead of raw logs. " +
			"analysis_type: overview (pods, lines, time span), levels (DEBUG/INFO/WARN/ERROR/FATAL counts), exceptions (grouped by type with request IDs), " +
			"patterns (repeated messages with dynamic values normalized), http (methods, paths, status codes, failed requests), " +
			"queue_urls (unique queue_ingest_url values), search (keyword + level filter, optional context), sample (raw recent lines, only when needed), or all. " +
			"Supports JSON and plain-text logs. Secrets are redacted. Results are bounded by max_results; page with offset/next_offset.",
		Annotations: readOnly,
	}, t.analyzeLogs)
}

// parseSince accepts a Go duration (plus a "d" day suffix) or an RFC3339 time.
func parseSince(s string, now time.Time) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		d = now.Sub(t)
	} else if strings.HasSuffix(s, "d") {
		var days float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &days); err != nil {
			return 0, toolkit.Invalid("since %q is not a duration (e.g. 15m, 2h, 1d) or RFC3339 time", s)
		}
		d = time.Duration(days * float64(24*time.Hour))
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, toolkit.Invalid("since %q is not a duration (e.g. 15m, 2h, 1d) or RFC3339 time", s)
	}
	if d < time.Second || d > maxSince {
		return 0, toolkit.Invalid("since must be between 1s and 7d in the past")
	}

	return d, nil
}

func (in *analyzeInput) validate() (client.AppRef, loganalysis.Level, time.Duration, error) {
	ref := client.AppRef{Name: in.Application, AppNamespace: in.AppNamespace, Project: in.Project}
	if err := toolkit.K8sName("application", in.Application); err != nil {
		return ref, "", 0, err
	}
	if err := toolkit.OptionalNamespace("app_namespace", in.AppNamespace); err != nil {
		return ref, "", 0, err
	}
	if err := toolkit.OptionalK8sName("project", in.Project); err != nil {
		return ref, "", 0, err
	}
	if in.Deployment == "" && in.Pod == "" {
		return ref, "", 0, toolkit.Invalid("deployment (or pod) is required")
	}
	for f, v := range map[string]string{"deployment": in.Deployment, "pod": in.Pod} {
		if err := toolkit.OptionalK8sName(f, v); err != nil {
			return ref, "", 0, err
		}
	}
	for f, v := range map[string]string{"namespace": in.Namespace, "container": in.Container} {
		if err := toolkit.OptionalNamespace(f, v); err != nil {
			return ref, "", 0, err
		}
	}
	if in.AnalysisType == "" {
		in.AnalysisType = ModeOverview
	}
	in.AnalysisType = strings.ToLower(in.AnalysisType)
	if !analysisModes[in.AnalysisType] {
		return ref, "", 0, toolkit.Invalid("analysis_type must be one of overview, levels, exceptions, patterns, http, queue_urls, search, sample, all")
	}
	if err := toolkit.Text("search", in.Search, 256); err != nil {
		return ref, "", 0, err
	}
	if in.AnalysisType == ModeSearch && strings.TrimSpace(in.Search) == "" && in.Level == "" {
		return ref, "", 0, toolkit.Invalid("analysis_type=search needs search and/or level")
	}
	var level loganalysis.Level
	if in.Level != "" {
		l, ok := loganalysis.ParseLevel(in.Level)
		if !ok {
			return ref, "", 0, toolkit.Invalid("level must be DEBUG, INFO, WARN, ERROR or FATAL")
		}
		level = l
	}
	since, err := parseSince(in.Since, time.Now())
	if err != nil {
		return ref, "", 0, err
	}
	if in.TailLines, err = toolkit.Limit("tail_lines", in.TailLines, defaultTailAnalyze, maxTailAnalyze); err != nil {
		return ref, "", 0, err
	}
	if in.MaxResults, err = toolkit.Limit("max_results", in.MaxResults, defaultMaxResults, maxMaxResults); err != nil {
		return ref, "", 0, err
	}
	if in.ContextLines < 0 || in.ContextLines > maxContextLines {
		return ref, "", 0, toolkit.Invalid("context_lines must be between 0 and %d", maxContextLines)
	}
	if in.Offset < 0 || in.Offset > maxOffset {
		return ref, "", 0, toolkit.Invalid("offset must be between 0 and %d", maxOffset)
	}

	return ref, level, since, nil
}

// page applies offset/limit to a list and reports whether more items exist.
func page[T any](items []T, offset, limit int) ([]T, *int) {
	if offset >= len(items) {
		return nil, nil
	}
	end := min(offset+limit, len(items))
	var next *int
	if end < len(items) {
		next = &end
	}

	return items[offset:end], next
}

func (t *Tools) analyzeLogs(ctx context.Context, _ *mcp.CallToolRequest, in analyzeInput) (*mcp.CallToolResult, analyzeOutput, error) {
	ref, level, since, err := in.validate()
	if err != nil {
		return nil, analyzeOutput{}, err
	}

	// Reuse the existing Argo CD log retrieval (no Kubernetes API access).
	opts := client.LogOptions{Namespace: in.Namespace, Container: in.Container, TailLines: int64(in.TailLines),
		SinceSeconds: int64(since / time.Second), Previous: in.Previous, MaxBytes: analysisMaxBytes}
	if in.Pod != "" {
		opts.PodName = in.Pod
	} else {
		opts.Kind, opts.Group, opts.ResourceName = "Deployment", "apps", in.Deployment
	}
	res, err := t.api.GetLogs(ctx, ref, opts)
	if err != nil {
		return nil, analyzeOutput{}, wrap(err)
	}

	raw := make([]loganalysis.Line, len(res.Lines))
	for i, l := range res.Lines {
		raw[i] = loganalysis.Line{Pod: l.Pod, Timestamp: l.Timestamp, Content: l.Content}
	}
	es := loganalysis.Parse(raw)
	ov := loganalysis.AnalyzeOverview(es, len(raw))

	out := analyzeOutput{Application: ref.Name, Deployment: in.Deployment, Pod: in.Pod, AnalysisType: in.AnalysisType}
	out.TimeRange.From, out.TimeRange.To = loganalysis.TimeRange(es)
	if since > 0 {
		out.TimeRange.Since = since.String()
	}
	out.Summary = analyzeSummary{LinesFetched: len(raw), Entries: len(es), Pods: len(ov.Pods), Errors: ov.Errors,
		Warnings: ov.Warnings, LogsTruncated: res.Truncated}
	if res.Truncated {
		out.Summary.Notes = append(out.Summary.Notes, fmt.Sprintf("log fetch stopped at %d MiB; narrow with since, tail_lines, pod or container", analysisMaxBytes>>20))
	}
	if len(raw) == 0 {
		out.Summary.Notes = append(out.Summary.Notes, "no log lines returned (wrong container/namespace, pod not running, or nothing in the time range)")
	}
	if len(raw) >= in.TailLines {
		out.Summary.Notes = append(out.Summary.Notes, fmt.Sprintf("analysed the last %d lines per container; raise tail_lines or use since for more history", in.TailLines))
	}

	mode := in.AnalysisType
	all := mode == ModeAll
	var next *int
	more := false
	n, off := in.MaxResults, in.Offset

	if mode == ModeOverview || all {
		if len(ov.Pods) > maxPodsListed {
			ov.Pods = ov.Pods[:maxPodsListed]
			more = true
		}
		out.Results.Overview = &ov
		out.TotalMatches = len(es)
	}
	if mode == ModeLevels || all {
		lv := loganalysis.AnalyzeLevels(es)
		out.Results.Levels = &lv
		out.TotalMatches = len(es)
	}
	if mode == ModeExceptions || all {
		g := loganalysis.AnalyzeExceptions(es)
		if !all {
			out.TotalMatches = len(g)
		}
		out.Results.Exceptions, next = page(g, off, n)
		more = more || next != nil
	}
	if mode == ModePatterns || all {
		g := loganalysis.AnalyzePatterns(es)
		if !all {
			out.TotalMatches = len(g)
		}
		var nx *int
		out.Results.Patterns, nx = page(g, off, n)
		next, more = firstNonNil(next, nx), more || nx != nil
	}
	if mode == ModeHTTP || all {
		h, endpoints := loganalysis.AnalyzeHTTP(es, n)
		if !all {
			out.TotalMatches = h.TotalRequests
		}
		out.Results.HTTP = &h
		more = more || endpoints > n || h.Failed > len(h.FailedSamples)
	}
	if mode == ModeQueueURLs || all {
		q := loganalysis.AnalyzeQueueURLs(es)
		if !all {
			out.TotalMatches = len(q)
		}
		var nx *int
		out.Results.QueueURLs, nx = page(q, off, n)
		next, more = firstNonNil(next, nx), more || nx != nil
	}
	if mode == ModeSearch || (all && (in.Search != "" || level != "")) {
		m, total := loganalysis.Search(es, loganalysis.SearchOptions{Keyword: in.Search, MinLevel: level, Context: in.ContextLines, Offset: off, Limit: n})
		if !all {
			out.TotalMatches = total
		}
		out.Results.Matches = m
		if off+len(m) < total {
			end := off + len(m)
			next, more = firstNonNil(next, &end), true
		}
	}
	if mode == ModeSample {
		m, total := loganalysis.Search(es, loganalysis.SearchOptions{MinLevel: level, Offset: off, Limit: n, Newest: true})
		out.TotalMatches = total
		out.Results.Matches = m
		if off+len(m) < total {
			end := off + len(m)
			next, more = &end, true
		}
		out.Summary.Notes = append(out.Summary.Notes, "sample returns redacted raw lines, newest first; prefer the analysis modes for large logs")
	}

	out.NextOffset = next
	out.Truncated = more || res.Truncated

	return nil, out, nil
}

func firstNonNil(a, b *int) *int {
	if a != nil {
		return a
	}

	return b
}
