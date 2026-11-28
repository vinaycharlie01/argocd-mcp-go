package argocd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// LogOptions selects which container logs to fetch. Either PodName, or
// Kind + ResourceName (e.g. Deployment "web", aggregating all of its pods).
type LogOptions struct {
	Namespace    string // namespace of the pod/resource
	PodName      string
	Kind         string
	Group        string
	ResourceName string
	Container    string
	TailLines    int64
	SinceSeconds int64
	Previous     bool   // logs of the previous (crashed) container instance
	Filter       string // server-side substring filter; prefix "!" to exclude
	// MaxBytes bounds the total log content read (0 = 1 MiB).
	MaxBytes int
}

// LogLine is one log line.
type LogLine struct {
	Pod       string `json:"pod,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Content   string `json:"content"`
}

// LogResult is the collected (non-follow) log output.
type LogResult struct {
	Lines     []LogLine
	Truncated bool
}

const defaultMaxLogBytes = 1 << 20

// streamEnvelope is one grpc-gateway server-stream message.
type streamEnvelope struct {
	Result *struct {
		Content      string `json:"content"`
		TimeStampStr string `json:"timeStampStr"`
		PodName      string `json:"podName"`
		Last         bool   `json:"last"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// GetLogs fetches container logs through Argo CD (never follows the stream).
func (c *Client) GetLogs(ctx context.Context, ref AppRef, o LogOptions) (*LogResult, error) {
	q := ref.query()
	q.Set("follow", "false")
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("namespace", o.Namespace)
	set("container", o.Container)
	set("filter", o.Filter)
	if o.PodName != "" {
		set("podName", o.PodName)
	} else {
		set("kind", o.Kind)
		set("group", o.Group)
		set("resourceName", o.ResourceName)
	}
	if o.TailLines > 0 {
		q.Set("tailLines", strconv.FormatInt(o.TailLines, 10))
	}
	if o.SinceSeconds > 0 {
		q.Set("sinceSeconds", strconv.FormatInt(o.SinceSeconds, 10))
	}
	if o.Previous {
		q.Set("previous", "true")
	}
	maxBytes := o.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}

	resp, err := c.send(ctx, q, "applications", ref.Name, "logs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return readLogStream(resp.Body, maxBytes)
}

func readLogStream(r io.Reader, maxBytes int) (*LogResult, error) {
	res := &LogResult{Lines: []LogLine{}}
	size := 0
	dec := json.NewDecoder(bufio.NewReader(io.LimitReader(r, maxResponseBytes)))
	for {
		var env streamEnvelope
		err := dec.Decode(&env)
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode Argo CD log stream: %w", err)
		}
		if env.Error != nil {
			return nil, streamError(env.Error.Code, env.Error.Message)
		}
		if env.Result == nil {
			continue
		}
		if env.Result.Last {
			return res, nil
		}
		if size+len(env.Result.Content) > maxBytes {
			res.Truncated = true

			return res, nil
		}
		size += len(env.Result.Content)
		res.Lines = append(res.Lines, LogLine{Pod: env.Result.PodName, Timestamp: env.Result.TimeStampStr, Content: env.Result.Content})
	}
}

// streamError maps gRPC codes inside a stream to the same errors as HTTP statuses.
func streamError(code int, msg string) error {
	switch code {
	case 16: // Unauthenticated
		return &APIError{Status: 401, Message: msg}
	case 7: // PermissionDenied
		return &APIError{Status: 403, Message: msg}
	case 5: // NotFound
		return &APIError{Status: 404, Message: msg}
	case 3: // InvalidArgument
		return &APIError{Status: 400, Message: msg}
	default:
		return &APIError{Status: 500, Message: msg}
	}
}
