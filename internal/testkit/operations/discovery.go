package operations

import (
	"context"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Logical paths always use slash separators, independently of the host OS.
// Eino's in-memory backend uses filepath.Clean but tests slash prefixes, which
// is not portable on Windows. Reuse its doublestar matcher, not that path layer.
func logicalPath(s string) string { return path.Clean("/" + strings.ReplaceAll(s, "\\", "/")) }
func underRoot(name, root string) (string, bool) {
	n, r := logicalPath(name), logicalPath(root)
	if n == r {
		return path.Base(n), true
	}
	prefix := strings.TrimSuffix(r, "/") + "/"
	if !strings.HasPrefix(n, prefix) {
		return "", false
	}
	return strings.TrimPrefix(n, prefix), true
}
func (m *Memory) List(ctx context.Context, r agent.ListRequest) (agent.ListResult, error) {
	m.count("list")
	if err := ctx.Err(); err != nil {
		return agent.ListResult{}, err
	}
	if r.Root == "" || r.Cursor != "" || r.Limit < 0 {
		return agent.ListResult{}, failure(product.CodeInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := map[string]agent.FileEntry{}
	for name, f := range m.files {
		if err := ctx.Err(); err != nil {
			return agent.ListResult{}, err
		}
		relative, ok := underRoot(name, r.Root)
		if !ok {
			continue
		}
		child, _, dir := strings.Cut(relative, "/")
		e := agent.FileEntry{Identity: path.Join(r.Root, child), Name: child, Kind: "file", Size: int64(len(f.data)), Version: f.version}
		if dir {
			e.Kind = "directory"
			e.Size = 0
			e.Version = ""
		}
		entries[child] = e
	}
	out := agent.ListResult{Entries: []agent.FileEntry{}}
	for _, e := range entries {
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Identity < out.Entries[j].Identity })
	return out, nil
}

// Search returns raw matches; the executor owns output aggregation, offset and
// complete JSON limits. Hosts must not silently pre-slice these raw matches.
func (m *Memory) Search(ctx context.Context, r agent.SearchRequest) (agent.SearchResult, error) {
	m.count("search")
	if err := ctx.Err(); err != nil {
		return agent.SearchResult{}, err
	}
	if r.Root == "" || r.Query == "" || r.Cursor != "" || r.Offset < 0 || r.Limit < 0 {
		return agent.SearchResult{}, failure(product.CodeInvalidArgument)
	}
	if r.Kind != "glob" && r.Kind != "grep" {
		return agent.SearchResult{}, failure(product.CodeInvalidArgument)
	}
	if r.OutputMode != "" && r.OutputMode != "content" && r.OutputMode != "count" && r.OutputMode != "files_with_matches" {
		return agent.SearchResult{}, failure(product.CodeInvalidArgument)
	}
	var re *regexp.Regexp
	if r.Kind == "glob" {
		if !doublestar.ValidatePattern(r.Query) || strings.HasPrefix(r.Query, "/") {
			return agent.SearchResult{}, failure(product.CodeInvalidArgument)
		}
	} else {
		query := r.Query
		if r.CaseInsensitive {
			query = "(?i)" + query
		}
		var err error
		re, err = regexp.Compile(query)
		if err != nil {
			return agent.SearchResult{}, failure(product.CodeInvalidArgument)
		}
	}
	if r.Glob != "" && !doublestar.ValidatePattern(r.Glob) {
		return agent.SearchResult{}, failure(product.CodeInvalidArgument)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := agent.SearchResult{Matches: []agent.SearchMatch{}}
	for name, f := range m.files {
		if err := ctx.Err(); err != nil {
			return agent.SearchResult{}, err
		}
		relative, ok := underRoot(name, r.Root)
		if !ok {
			continue
		}
		if r.Kind == "glob" {
			matched, _ := doublestar.Match(r.Query, relative)
			if matched {
				out.Matches = append(out.Matches, agent.SearchMatch{Identity: name})
			}
			continue
		}
		if r.Glob != "" {
			candidate := relative
			if !strings.Contains(r.Glob, "/") {
				candidate = path.Base(relative)
			}
			matched, _ := doublestar.Match(r.Glob, candidate)
			if !matched {
				continue
			}
		}
		for i, line := range strings.Split(string(f.data), "\n") {
			if err := ctx.Err(); err != nil {
				return agent.SearchResult{}, err
			}
			if re.MatchString(line) {
				out.Matches = append(out.Matches, agent.SearchMatch{Identity: name, Line: i + 1, Preview: line})
			}
		}
	}
	sort.Slice(out.Matches, func(i, j int) bool {
		if out.Matches[i].Identity == out.Matches[j].Identity {
			return out.Matches[i].Line < out.Matches[j].Line
		}
		return out.Matches[i].Identity < out.Matches[j].Identity
	})
	return out, nil
}
