package orchestrator

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorchera/internal/domain"
)

// defaultExcludes lists directory/file names that are always skipped during
// workspace snapshot. These are build artifacts, caches, and VCS metadata
// that change frequently but are not part of the deliverable.
var defaultExcludes = []string{
	".git", ".gorchera", "node_modules", ".cache",
	"__pycache__", ".venv", "vendor", "dist", "build",
	".idea", ".vscode", ".DS_Store",
}

const snapshotMaxFileSize = 1 << 20 // 1 MB -- larger files are skipped

// WorkspaceSnapshot stores SHA-256 hashes of workspace files for change
// detection. A nil Hashes map means the snapshot was not taken (e.g. dir
// not found), not that the workspace is empty.
type WorkspaceSnapshot struct {
	Hashes map[string]string // relative path -> hex sha256
}

// snapshotWorkspace walks dir and records SHA-256 hashes of all qualifying
// files. Files larger than snapshotMaxFileSize are skipped. Directories
// matching defaultExcludes or .gitignore patterns are skipped entirely.
func snapshotWorkspace(dir string) (*WorkspaceSnapshot, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	excludes := append([]string(nil), defaultExcludes...)
	excludes = append(excludes, loadGitignorePatterns(dir)...)

	snap := &WorkspaceSnapshot{Hashes: make(map[string]string)}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Skip unreadable entries -- best-effort snapshot.
			return nil
		}
		name := info.Name()
		if info.IsDir() {
			if shouldExclude(name, excludes) {
				return filepath.SkipDir
			}
			return nil
		}
		if shouldExclude(name, excludes) {
			return nil
		}
		if info.Size() > snapshotMaxFileSize {
			return nil
		}
		h, err := hashFile(path)
		if err != nil {
			return nil // skip unreadable files
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		snap.Hashes[filepath.ToSlash(rel)] = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// diffSnapshots compares two snapshots and returns the list of changed files.
// A nil before snapshot produces all after files as "created".
func diffSnapshots(before, after *WorkspaceSnapshot) []domain.ChangedFile {
	if after == nil {
		return nil
	}
	var changes []domain.ChangedFile
	afterHashes := after.Hashes
	if afterHashes == nil {
		afterHashes = map[string]string{}
	}
	var beforeHashes map[string]string
	if before != nil && before.Hashes != nil {
		beforeHashes = before.Hashes
	} else {
		beforeHashes = map[string]string{}
	}

	// Detect created and modified files.
	for path, afterHash := range afterHashes {
		if beforeHash, ok := beforeHashes[path]; !ok {
			changes = append(changes, domain.ChangedFile{Path: path, Action: "created"})
		} else if beforeHash != afterHash {
			changes = append(changes, domain.ChangedFile{Path: path, Action: "modified"})
		}
	}
	// Detect deleted files.
	for path := range beforeHashes {
		if _, ok := afterHashes[path]; !ok {
			changes = append(changes, domain.ChangedFile{Path: path, Action: "deleted"})
		}
	}
	return changes
}

// parseGitDiffStatFiles parses the output of "git diff --stat" into a list
// of ChangedFile entries. Lines look like:
//
//	 path/to/file.go | 12 +++---
//	 path/to/new.go  |  5 +++++
//	 3 files changed, ...
//
// We treat every file line (containing " | ") as "modified" because git diff
// --stat does not distinguish created vs modified. The final summary line is
// skipped.
func parseGitDiffStatFiles(diffStat string) []domain.ChangedFile {
	var result []domain.ChangedFile
	for _, line := range strings.Split(diffStat, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Summary line: "N files changed, ..."
		if !strings.Contains(line, "|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		path := strings.TrimSpace(parts[0])
		if path == "" {
			continue
		}
		result = append(result, domain.ChangedFile{Path: path, Action: "modified"})
	}
	return result
}

// detectChangedFiles returns the list of files changed during a worker step.
// When git is available (workspace is inside a git repo), it uses git diff
// --stat which is fast and accurate. Otherwise it falls back to comparing
// the pre-execution snapshot against the current workspace state.
func detectChangedFiles(ctx context.Context, workspaceDir string, snapshot *WorkspaceSnapshot) []domain.ChangedFile {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" {
		return nil
	}

	// Try git diff --stat first (fast path, handles submodules correctly).
	diffStat := collectWorkspaceDiffSummaryWithTimeout(ctx, workspaceDir, 10*time.Second)
	if diffStat != "" {
		return parseGitDiffStatFiles(diffStat)
	}

	// Fallback: compare current state against the pre-execution snapshot.
	after, err := snapshotWorkspace(workspaceDir)
	if err != nil || after == nil {
		return nil
	}
	return diffSnapshots(snapshot, after)
}

// collectWorkspaceDiffSummaryWithTimeout runs "git diff --stat" with a
// timeout and returns the output. Returns "" on any error or timeout.
func collectWorkspaceDiffSummaryWithTimeout(ctx context.Context, workspaceDir string, timeout time.Duration) string {
	// Re-use the existing collectWorkspaceDiffSummary logic via a scoped
	// context so we don't duplicate exec.Command boilerplate.
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return collectWorkspaceDiffSummary(timeoutCtx, workspaceDir)
}

// hashFile computes a hex-encoded SHA-256 hash of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shouldExclude reports whether name matches any pattern in the excludes list.
// Only exact name matching is performed (no path glob). This is intentional:
// the excludes list targets well-known directory/file names, not arbitrary paths.
func shouldExclude(name string, excludes []string) bool {
	for _, ex := range excludes {
		if name == ex {
			return true
		}
	}
	return false
}

// loadGitignorePatterns reads .gitignore in dir and returns a list of simple
// exclude patterns. Only plain name patterns are returned (no leading slash,
// no negation, no wildcards). Complex patterns are silently ignored -- the
// goal is best-effort exclusion, not full gitignore compliance.
func loadGitignorePatterns(dir string) []string {
	f, err := os.Open(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Skip negation patterns and patterns with path separators or wildcards
		// that require full gitignore glob semantics.
		if strings.HasPrefix(line, "!") {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		// Strip trailing slash (directory marker) to get the bare name.
		name := strings.TrimRight(line, "/")
		// Skip patterns that contain path separators -- they require
		// anchored matching which we do not implement here.
		if strings.ContainsRune(name, '/') {
			continue
		}
		if name != "" {
			patterns = append(patterns, name)
		}
	}
	return patterns
}
