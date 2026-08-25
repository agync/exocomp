package web

import (
	"context"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/source"
)

// sourceForDir reverse-maps a job's working directory back to the registered
// source whose checkout root contains it (longest-prefix wins), so the job page
// can offer a "Run again" link into that source's launcher. Returns ok=false
// when the dir belongs to no source (an ad-hoc run, or a path outside them all).
func (s *Server) sourceForDir(ctx context.Context, dir string) (name string, ok bool) {
	if dir == "" {
		return "", false
	}
	dir = filepath.Clean(dir)
	srcs, err := s.store.ListSources(ctx)
	if err != nil {
		return "", false
	}
	var bestRoot string
	for _, src := range srcs {
		loc, err := source.Resolve(s.dataDir, src.Kind, src.Name, src.URI, src.Writable)
		if err != nil {
			continue
		}
		root := filepath.Clean(loc.Root)
		if root != dir && !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			continue
		}
		if len(root) > len(bestRoot) {
			bestRoot, name, ok = root, src.Name, true
		}
	}
	return name, ok
}
