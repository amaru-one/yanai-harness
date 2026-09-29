package repository

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Search limits keep one tool result small enough to resend every turn.
const (
	MaxListed        = 500
	MaxMatches       = 200
	MaxMatchChars    = 300
	MaxGrepFileBytes = 2 << 20
	// MaxRangeSourceBytes bounds a file read in line ranges; the returned
	// range itself still has to fit the caller's per-file limit.
	MaxRangeSourceBytes = 8 << 20
)

// Listed is one file of a list_files result.
type Listed struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Hit is one grep match.
type Hit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GlobMatch reports whether path matches glob. "*" and "?" stay inside one
// path segment, "**" spans any number of them. A glob without "/" matches
// the file name alone, so "*.yml" finds YAML files at any depth.
func GlobMatch(glob, path string) (bool, error) {
	if glob == "" {
		return true, nil
	}
	subject := path
	if !strings.Contains(glob, "/") {
		subject = filepath.Base(path)
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; {
		case c == '*' && strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false, fmt.Errorf("invalid glob %q: %w", glob, err)
	}
	return re.MatchString(subject), nil
}

// scoped lists the readable files under dir ("" or "." for the whole
// repository) that match glob.
func (t *Target) scoped(dir, glob string) ([]string, error) {
	dir = strings.TrimSuffix(strings.TrimSpace(dir), "/")
	if dir != "" && dir != "." {
		if err := cleanRelative(dir); err != nil {
			return nil, err
		}
	}
	if _, err := GlobMatch(glob, ""); err != nil {
		return nil, err
	}
	files, err := t.Files()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range files {
		if dir != "" && dir != "." && path != dir && !strings.HasPrefix(path, dir+"/") {
			continue
		}
		if ok, _ := GlobMatch(glob, path); ok {
			out = append(out, path)
		}
	}
	return out, nil
}

// ListFiles lists readable files under dir matching glob, with sizes. The
// bool reports that the list was cut at MaxListed entries.
func (t *Target) ListFiles(dir, glob string) ([]Listed, bool, error) {
	paths, err := t.scoped(dir, glob)
	if err != nil {
		return nil, false, err
	}
	var out []Listed
	for _, path := range paths {
		if len(out) == MaxListed {
			return out, true, nil
		}
		info, err := os.Lstat(filepath.Join(t.Root, filepath.FromSlash(path)))
		if err != nil {
			continue
		}
		out = append(out, Listed{Path: path, Size: info.Size()})
	}
	return out, false, nil
}

// Grep searches readable text files under dir matching glob for an RE2
// pattern. Binary files and files over MaxGrepFileBytes are skipped. The
// bool reports that the matches were cut at MaxMatches.
func (t *Target) Grep(pattern, dir, glob string, ignoreCase bool) ([]Hit, bool, error) {
	if pattern == "" {
		return nil, false, fmt.Errorf("grep needs a pattern")
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("invalid pattern (RE2 syntax): %w", err)
	}
	paths, err := t.scoped(dir, glob)
	if err != nil {
		return nil, false, err
	}
	var out []Hit
	for _, path := range paths {
		data, err := t.readRegular(path, MaxGrepFileBytes+1)
		if err != nil || len(data) > MaxGrepFileBytes || Binary(data) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(out) == MaxMatches {
				return out, true, nil
			}
			if len(line) > MaxMatchChars {
				line = line[:MaxMatchChars] + "…"
			}
			out = append(out, Hit{Path: path, Line: i + 1, Text: strings.TrimRight(line, "\r")})
		}
	}
	return out, false, nil
}

// Binary reports content that is not text: a NUL byte in its first 8 KB.
func Binary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// LineCount counts lines the way Lines numbers them.
func LineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// Lines returns lines start..end (1-based, inclusive) of data. end 0 means
// the last line; an end past the file is clamped.
func Lines(data []byte, start, end int) (string, error) {
	total := LineCount(data)
	if start < 1 {
		start = 1
	}
	if end == 0 || end > total {
		end = total
	}
	if start > end {
		return "", fmt.Errorf("line range %d-%d is outside the file's %d lines", start, end, total)
	}
	lines := strings.SplitAfter(string(data), "\n")
	return strings.Join(lines[start-1:end], ""), nil
}

// ReadRange reads path within the target's rules for a line-range request.
func (t *Target) ReadRange(path string) ([]byte, error) {
	if err := t.CheckPath(path); err != nil {
		return nil, err
	}
	data, err := t.readRegular(path, MaxRangeSourceBytes+1)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRangeSourceBytes {
		return nil, fmt.Errorf("file is larger than %d bytes", MaxRangeSourceBytes)
	}
	return data, nil
}
