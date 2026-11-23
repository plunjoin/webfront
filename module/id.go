// Package module defines the framework-neutral identity and graph types shared by
// the dev server, the build pipeline and (later) adapter implementations.
//
// A module ID is always a project-root-relative POSIX path. It is the same for
// /src/foo.tsx, /@fs/src/foo.tsx and an import resolved from another file, so
// the graph never grows duplicate nodes for one physical module.
package module

import (
	"path/filepath"
	"runtime"
	"strings"
)

// CanonicalID maps an absolute path to its project-root-relative ID.
//
// Existing paths are resolved through EvalSymlinks first, so a symlinked file
// and its target share one node. Paths outside root are rejected. The returned
// ID keeps the on-disk spelling; use Key for case-insensitive graph lookups on
// Windows.
func CanonicalID(root, abs string) (string, bool) {
	if root == "" || abs == "" {
		return "", false
	}
	root = filepath.Clean(root)
	abs = filepath.Clean(abs)
	// 只有两边都能 realpath 时才一起替换。删除事件里文件已经不存在，
	// 若只把 root 换成 long path、abs 保留 short path，相对路径会算错。
	rootReal, rootErr := filepath.EvalSymlinks(root)
	absReal, absErr := filepath.EvalSymlinks(abs)
	if rootErr == nil && absErr == nil {
		root = filepath.Clean(rootReal)
		abs = filepath.Clean(absReal)
	}
	rel, ok := relativeWithin(root, abs)
	if !ok || rel == "." {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Key is the map key for a module ID. Windows paths are case-insensitive, so
// ids that differ only in case must address the same node.
func Key(id string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(id)
	}
	return id
}

// Within reports whether abs is inside root (both are cleaned first).
func Within(root, abs string) (string, bool) {
	return relativeWithin(filepath.Clean(root), filepath.Clean(abs))
}

func relativeWithin(root, abs string) (string, bool) {
	rel, err := filepath.Rel(root, abs)
	if err != nil || filepath.IsAbs(rel) {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
