package loganalysis

import (
	"regexp"
	"strings"
)

// Normalization turns dynamic values into placeholders so repeated messages
// group together ("user 42 logged in from 10.0.0.1" -> "user {n} logged in from {ip}").
var normalizers = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "{uuid}"},
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?`), "{ts}"},
	{regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`), "{time}"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "{email}"},
	{regexp.MustCompile(`\b(?:https?|amqps?|sqs|wss?)://[^\s"'<>]+`), "{url}"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`), "{ip}"},
	{regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{8,}\b`), "{hex}"},
	{regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|s|m|h|µs|ns|kb|mb|gb|b)?\b`), "{n}"},
	{regexp.MustCompile(`\s+`), " "},
}

const maxPatternLen = 300

// Normalize returns the message pattern of s.
func Normalize(s string) string {
	for _, n := range normalizers {
		s = n.re.ReplaceAllString(s, n.with)
	}
	s = strings.TrimSpace(s)
	if len(s) > maxPatternLen {
		s = s[:maxPatternLen] + "…"
	}

	return s
}

var pathIDRe = regexp.MustCompile(`^(?:\d+|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{16,}|[A-Za-z0-9_-]{20,})$`)

// NormalizePath replaces ID-like path segments with {id} for endpoint grouping.
func NormalizePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s != "" && pathIDRe.MatchString(s) {
			segs[i] = "{id}"
		}
	}

	return strings.Join(segs, "/")
}
