package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorchera/internal/domain"
)

// TestSafeReadFileBlocksTraversal verifies that safeReadFile rejects paths outside the allowed root.
func TestSafeReadFileBlocksTraversal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create a legitimate file inside the root.
	validFile := filepath.Join(root, "artifact.json")
	if err := os.WriteFile(validFile, []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Create a file OUTSIDE the root to attempt to read via traversal.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret content"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Run("valid path within root succeeds", func(t *testing.T) {
		t.Parallel()
		data, err := safeReadFile(root, validFile)
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if string(data) != `{"ok":true}` {
			t.Fatalf("unexpected content: %q", data)
		}
	})

	t.Run("absolute path outside root is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := safeReadFile(root, outsideFile)
		if err == nil {
			t.Fatal("expected error for path outside root, got nil")
		}
		if !errors.Is(err, errPathTraversal) {
			t.Errorf("expected errPathTraversal sentinel, got: %v", err)
		}
	})

	t.Run("relative traversal path is rejected", func(t *testing.T) {
		t.Parallel()
		// Construct a traversal path relative to a subdirectory of root.
		traversalPath := filepath.Join(root, "subdir", "..", "..", "secret")
		_, err := safeReadFile(root, traversalPath)
		if err == nil {
			// The clean path resolves outside root -- must be an error.
			t.Fatal("expected error for traversal path, got nil")
		}
	})
}

// TestBuildEvaluatorViewBlocksTraversal verifies that BuildEvaluatorView rejects artifact refs
// pointing outside the job's workspace directory.
func TestBuildEvaluatorViewBlocksTraversal(t *testing.T) {
	t.Parallel()

	workspaceDir := t.TempDir()

	// Create a valid report inside the workspace.
	reportPath := filepath.Join(workspaceDir, "report.json")
	if err := os.WriteFile(reportPath, []byte(`{"verdict":"done","score":1,"evidence":[]}`), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Create a file outside the workspace.
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "passwd")
	if err := os.WriteFile(outsideFile, []byte("root:x:0:0"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	t.Run("valid ref inside workspace succeeds", func(t *testing.T) {
		t.Parallel()
		job := &domain.Job{
			ID:                 "job-test",
			WorkspaceDir:       workspaceDir,
			EvaluatorReportRef: reportPath,
		}
		view := BuildEvaluatorView(job)
		if view.Error != "" {
			t.Errorf("expected no error, got: %q", view.Error)
		}
	})

	t.Run("ref outside workspace is rejected", func(t *testing.T) {
		t.Parallel()
		job := &domain.Job{
			ID:                 "job-test",
			WorkspaceDir:       workspaceDir,
			EvaluatorReportRef: outsideFile,
		}
		view := BuildEvaluatorView(job)
		if view.Error == "" {
			t.Fatal("expected error for path outside workspace, got empty error")
		}
		// Client-facing message must NOT reveal filesystem paths -- only a generic message.
		if strings.Contains(view.Error, workspaceDir) || strings.Contains(view.Error, outsideFile) {
			t.Errorf("client error must not contain filesystem paths, got: %q", view.Error)
		}
		if view.Error != "file not found" {
			t.Errorf("expected generic 'file not found' error, got: %q", view.Error)
		}
	})

	t.Run("traversal ref is rejected", func(t *testing.T) {
		t.Parallel()
		job := &domain.Job{
			ID:                 "job-test",
			WorkspaceDir:       workspaceDir,
			EvaluatorReportRef: "../../etc/passwd",
		}
		view := BuildEvaluatorView(job)
		if view.Error == "" {
			t.Fatal("expected error for traversal path, got empty error")
		}
	})
}
