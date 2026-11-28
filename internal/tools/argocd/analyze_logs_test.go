package argocd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	client "github.com/vinaycharlie01/argocd-mcp-go/internal/clients/argocd"
	"github.com/vinaycharlie01/argocd-mcp-go/internal/toolkit"
)

func sampleLogs() *client.LogResult {
	ln := func(pod, ts, c string) client.LogLine { return client.LogLine{Pod: pod, Timestamp: ts, Content: c} }

	return &client.LogResult{Lines: []client.LogLine{
		ln("web-1", "2026-03-01T10:00:00Z", `{"level":"info","msg":"server started","port":8080}`),
		ln("web-1", "2026-03-01T10:00:01Z", `{"level":"info","msg":"request","method":"GET","path":"/api/orders/12?token=TOPSECRET","status":200,"request_id":"rq-100001"}`),
		ln("web-1", "2026-03-01T10:00:02Z", `{"level":"error","msg":"request","method":"POST","path":"/api/pay","status":503,"request_id":"rq-100002","error":{"type":"UpstreamTimeoutError"}}`),
		ln("web-2", "2026-03-01T10:00:03Z", `{"level":"info","msg":"enqueue","queue_ingest_url":"https://sqs.example.com/123/ingest?X-Amz-Signature=abc"}`),
		ln("web-2", "2026-03-01T10:00:04Z", `2026-03-01 10:00:04 ERROR payment failed requestId=rq-100003 Authorization: Bearer abcdefghijklmnopqrst`),
		ln("web-2", "2026-03-01T10:00:04Z", `com.example.pay.PaymentDeclinedException: card declined`),
		ln("web-2", "2026-03-01T10:00:04Z", `	at com.example.pay.Svc.charge(Svc.java:88)`),
		ln("web-2", "2026-03-01T10:00:05Z", `WARN retrying payment attempt 2`),
		ln("web-2", "2026-03-01T10:00:06Z", `WARN retrying payment attempt 3`),
		ln("web-2", "2026-03-01T10:00:07Z", `{"level":"info", broken json`),
	}}
}

func analyze(t *testing.T, api *fakeAPI, args map[string]any) analyzeOutput {
	t.Helper()
	var out analyzeOutput
	call(t, session(t, api), "argocd_analyze_pod_logs", args, &out)

	return out
}

func TestAnalyzeFetchReusesGetLogs(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	out := analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "namespace": "prod", "container": "app", "since": "2h", "tail_lines": 500})
	o := api.lastLogs
	if o.Kind != "Deployment" || o.Group != "apps" || o.ResourceName != "web" || o.PodName != "" || o.Namespace != "prod" || o.Container != "app" ||
		o.TailLines != 500 || o.SinceSeconds != 7200 || o.MaxBytes != analysisMaxBytes {
		t.Fatalf("log options %+v", o)
	}
	if api.lastRef.Name != "shop" || out.AnalysisType != ModeOverview || out.TimeRange.Since != "2h0m0s" {
		t.Fatalf("%+v", out)
	}
	analyze(t, api, map[string]any{"application": "shop", "pod": "web-1"})
	if api.lastLogs.PodName != "web-1" || api.lastLogs.Kind != "" {
		t.Fatalf("pod mode %+v", api.lastLogs)
	}
}

func TestAnalyzeOverviewAndLevels(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	out := analyze(t, api, map[string]any{"application": "shop", "deployment": "web"})
	ov := out.Results.Overview
	if ov == nil || len(ov.Pods) != 2 || ov.TotalLines != 10 || ov.Entries != 8 || ov.FirstTimestamp != "2026-03-01T10:00:00Z" || ov.LastTimestamp != "2026-03-01T10:00:07Z" || ov.MalformedJSON != 1 {
		t.Fatalf("%+v", ov)
	}
	if out.Summary.Errors != 2 || out.Summary.Warnings != 2 || out.TimeRange.From == "" || out.Results.Levels != nil {
		t.Fatalf("%+v", out.Summary)
	}
	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "levels"})
	if c := out.Results.Levels.Counts; c["ERROR"] != 2 || c["WARN"] != 2 || c["INFO"] != 4 {
		t.Fatalf("%v", c)
	}
}

func TestAnalyzeExceptionsHTTPQueue(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	out := analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "exceptions"})
	types := map[string]bool{}
	for _, g := range out.Results.Exceptions {
		types[g.Type] = true
	}
	if !types["UpstreamTimeoutError"] || !types["com.example.pay.PaymentDeclinedException"] || out.TotalMatches != 2 {
		t.Fatalf("%+v", out.Results.Exceptions)
	}

	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "http"})
	h := out.Results.HTTP
	if h.TotalRequests != 2 || h.Failed != 1 || h.StatusDistribution["503"] != 1 || h.FailedSamples[0].RequestID != "rq-100002" {
		t.Fatalf("%+v", h)
	}

	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "queue_urls"})
	if len(out.Results.QueueURLs) != 1 || out.Results.QueueURLs[0].URL != "https://sqs.example.com/123/ingest?{query-redacted}" {
		t.Fatalf("%+v", out.Results.QueueURLs)
	}
}

func TestAnalyzeSearchSampleAndPagination(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	out := analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "search", "search": "payment", "max_results": 1, "context_lines": 1})
	if out.TotalMatches != 3 || len(out.Results.Matches) != 1 || !out.Truncated || out.NextOffset == nil || *out.NextOffset != 1 {
		t.Fatalf("%+v", out)
	}
	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "search", "search": "payment", "offset": 2})
	if len(out.Results.Matches) != 1 || out.NextOffset != nil || out.Truncated {
		t.Fatalf("last page: %+v", out)
	}
	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "search", "level": "error"})
	if out.TotalMatches != 2 {
		t.Fatalf("level-only search: %+v", out)
	}
	out = analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "sample", "max_results": 2})
	if len(out.Results.Matches) != 2 || out.Results.Matches[0].Index <= out.Results.Matches[1].Index {
		t.Fatalf("sample newest first: %+v", out.Results.Matches)
	}
}

func TestAnalyzeAllAndPatterns(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	out := analyze(t, api, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "all"})
	r := out.Results
	if r.Overview == nil || r.Levels == nil || r.Exceptions == nil || r.Patterns == nil || r.HTTP == nil || r.QueueURLs == nil || r.Matches != nil {
		t.Fatalf("all must include every analysis (search only with a keyword): %+v", r)
	}
	if out.TotalMatches != 8 { // 10 lines; the Java exception header and "at" line merge into the ERROR entry
		t.Fatalf("all counts entries: %d", out.TotalMatches)
	}
	found := false
	for _, p := range r.Patterns {
		if p.Pattern == "WARN retrying payment attempt {n}" && p.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("patterns %+v", r.Patterns)
	}
}

func TestAnalyzeNeverLeaksSecrets(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	for _, mode := range []string{"overview", "levels", "exceptions", "patterns", "http", "queue_urls", "sample", "all"} {
		res := call(t, session(t, api), "argocd_analyze_pod_logs", map[string]any{"application": "shop", "deployment": "web", "analysis_type": mode, "search": "payment", "max_results": 200}, nil)
		b, _ := json.Marshal(res)
		for _, secret := range []string{"TOPSECRET", "abcdefghijklmnopqrst", "X-Amz-Signature=abc"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s leaked %q", mode, secret)
			}
		}
	}
}

func TestAnalyzeTruncatedFetchAndEmpty(t *testing.T) {
	logs := sampleLogs()
	logs.Truncated = true
	out := analyze(t, &fakeAPI{logs: logs}, map[string]any{"application": "shop", "deployment": "web"})
	if !out.Truncated || !out.Summary.LogsTruncated || !strings.Contains(strings.Join(out.Summary.Notes, " "), "MiB") {
		t.Fatalf("%+v", out)
	}
	out = analyze(t, &fakeAPI{logs: &client.LogResult{}}, map[string]any{"application": "shop", "deployment": "web", "analysis_type": "all"})
	if out.Summary.Entries != 0 || !strings.Contains(strings.Join(out.Summary.Notes, " "), "no log lines") {
		t.Fatalf("%+v", out)
	}
}

func TestAnalyzeValidation(t *testing.T) {
	api := &fakeAPI{logs: sampleLogs()}
	cs := session(t, api)
	for _, args := range []map[string]any{
		{"application": "Bad_App", "deployment": "web"},
		{"application": "shop"},
		{"application": "shop", "deployment": "../x"},
		{"application": "shop", "deployment": "web", "analysis_type": "everything"},
		{"application": "shop", "deployment": "web", "analysis_type": "search"},
		{"application": "shop", "deployment": "web", "level": "loud"},
		{"application": "shop", "deployment": "web", "since": "30d"},
		{"application": "shop", "deployment": "web", "since": "yesterday"},
		{"application": "shop", "deployment": "web", "tail_lines": 50000},
		{"application": "shop", "deployment": "web", "max_results": 1000},
		{"application": "shop", "deployment": "web", "context_lines": 9},
		{"application": "shop", "deployment": "web", "offset": -1},
		{"application": "shop", "deployment": "web", "container": "A/B"},
	} {
		if c := errorCode(t, call(t, cs, "argocd_analyze_pod_logs", args, nil)); c != toolkit.CodeInvalidInput {
			t.Errorf("%v: %s", args, c)
		}
	}
	if api.calls != 0 {
		t.Fatalf("logs fetched for invalid input (%d calls)", api.calls)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{"15m": 15 * time.Minute, "1d": 24 * time.Hour, "1.5d": 36 * time.Hour, "2026-03-01T11:00:00Z": time.Hour, "": 0} {
		if got, err := parseSince(in, now); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"0s", "8d", "2026-03-01T13:00:00Z", "abc"} {
		if _, err := parseSince(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
