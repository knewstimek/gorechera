package orchestrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ValidateWorkspaceDir(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("workspace directory must be an absolute path: %s", path)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			info, statErr := os.Lstat(path)
			if statErr == nil && info.Mode()&os.ModeSymlink == 0 {
				resolved = filepath.Clean(path)
				err = nil
			}
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace directory does not exist: %s", path)
		}
		return fmt.Errorf("resolve workspace directory %q: %w", path, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("workspace directory does not exist: %s", path)
		}
		return fmt.Errorf("stat workspace directory %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace directory does not exist: %s", path)
	}
	return nil
}
