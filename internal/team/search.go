package team

import (
	"fmt"

	"github.com/yanai/yanai-harness/internal/orchestrator"
	"github.com/yanai/yanai-harness/internal/repository"
)

// searcher is what list_files and grep need: the planning target or the
// worker's guarded executor.
type searcher interface {
	ListFiles(dir, glob string) ([]repository.Listed, bool, error)
	Grep(pattern, dir, glob string, ignoreCase bool) ([]repository.Hit, bool, error)
}

// searchRepository runs one list_files or grep call. An error is one the
// model can correct (a bad pattern, glob or directory).
func searchRepository(s searcher, tool, arguments string) (map[string]any, error) {
	switch tool {
	case "list_files":
		req, err := orchestrator.ParseList(arguments)
		if err != nil {
			return nil, err
		}
		files, truncated, err := s.ListFiles(req.Path, req.Glob)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"files": files, "count": len(files)}
		if truncated {
			result["truncated"] = fmt.Sprintf("only the first %d files are listed; narrow path or glob", repository.MaxListed)
		}
		return result, nil
	case "grep":
		req, err := orchestrator.ParseGrep(arguments)
		if err != nil {
			return nil, err
		}
		hits, truncated, err := s.Grep(req.Pattern, req.Path, req.Glob, req.IgnoreCase)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"matches": hits, "count": len(hits)}
		if truncated {
			result["truncated"] = fmt.Sprintf("only the first %d matches are shown; narrow the pattern, path or glob", repository.MaxMatches)
		}
		return result, nil
	}
	return nil, fmt.Errorf("unknown search tool %q", tool)
}

// planningRead reads one path of a parent read_file call. It returns the
// result entry and the text that counts toward the planning read budget
// (empty when nothing was read).
func planningRead(target *repository.Target, req orchestrator.ReadRequest, path string, maxFile int) (map[string]any, string) {
	fail := func(format string, args ...any) (map[string]any, string) {
		return map[string]any{"path": path, "error": fmt.Sprintf(format, args...)}, ""
	}
	if req.Ranged() {
		data, err := target.ReadRange(path)
		if err != nil {
			return fail("%v", err)
		}
		text, err := repository.Lines(data, req.StartLine, req.EndLine)
		if err != nil {
			return fail("%v", err)
		}
		if len(text) > maxFile {
			return fail("the range is larger than repo.max_file_bytes (%d); request fewer lines", maxFile)
		}
		return map[string]any{"path": path, "start_line": max(req.StartLine, 1), "total_lines": repository.LineCount(data), "content": text}, text
	}
	data, err := target.ReadFile(path, maxFile+1)
	if err != nil {
		return fail("%v", err)
	}
	if len(data) > maxFile {
		lines := "many"
		if full, err := target.ReadRange(path); err == nil {
			lines = fmt.Sprint(repository.LineCount(full))
		}
		return fail("larger than repo.max_file_bytes (%d); the file has %s lines, read it in parts with start_line/end_line", maxFile, lines)
	}
	return map[string]any{"path": path, "content": string(data)}, string(data)
}
