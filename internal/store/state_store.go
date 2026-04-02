package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gorchera/internal/domain"
)

// validIDRegexp allows only safe characters in IDs used as file-system path components.
// Prevents path traversal via IDs containing "..", "/", "\", etc.
var validIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9_\-.]+$`)

// validateID returns an error if id contains characters that could allow path traversal.
// Explicitly rejects "." and ".." which are special filesystem path components even though
// their characters individually satisfy the allowlist regexp.
func validateID(id string) error {
	if id == "." || id == ".." {
		return fmt.Errorf("invalid ID %q: reserved filesystem path component", id)
	}
	if !validIDRegexp.MatchString(id) {
		return fmt.Errorf("invalid ID %q: must match ^[a-zA-Z0-9_-.]+$", id)
	}
	return nil
}

type StateStore struct {
	root string
}

func NewStateStore(root string) *StateStore {
	return &StateStore{root: root}
}

func (s *StateStore) SaveJob(_ context.Context, job *domain.Job) error {
	if err := validateID(job.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.jobsDir(), 0o755); err != nil {
		return err
	}
	path := s.jobPath(job.ID)
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomically(path, data)
}

func (s *StateStore) SaveChain(_ context.Context, chain *domain.JobChain) error {
	if err := validateID(chain.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.chainsDir(), 0o755); err != nil {
		return err
	}
	path := s.chainPath(chain.ID)
	data, err := json.MarshalIndent(chain, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomically(path, data)
}

func (s *StateStore) LoadJob(_ context.Context, jobID string) (*domain.Job, error) {
	if err := validateID(jobID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.jobPath(jobID))
	if err != nil {
		return nil, err
	}
	var job domain.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *StateStore) LoadChain(_ context.Context, chainID string) (*domain.JobChain, error) {
	if err := validateID(chainID); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.chainPath(chainID))
	if err != nil {
		return nil, err
	}
	var chain domain.JobChain
	if err := json.Unmarshal(data, &chain); err != nil {
		return nil, err
	}
	return &chain, nil
}

func (s *StateStore) ListJobs(_ context.Context) ([]domain.Job, error) {
	if err := os.MkdirAll(s.jobsDir(), 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.jobsDir())
	if err != nil {
		return nil, err
	}

	jobs := make([]domain.Job, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.jobsDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		var job domain.Job
		if err := json.Unmarshal(data, &job); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}

	sort.Slice(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt.After(jobs[j].CreatedAt)
	})
	return jobs, nil
}

func (s *StateStore) ListChains(_ context.Context) ([]domain.JobChain, error) {
	if err := os.MkdirAll(s.chainsDir(), 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.chainsDir())
	if err != nil {
		return nil, err
	}

	chains := make([]domain.JobChain, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.chainsDir(), entry.Name()))
		if err != nil {
			return nil, err
		}
		var chain domain.JobChain
		if err := json.Unmarshal(data, &chain); err != nil {
			return nil, err
		}
		chains = append(chains, chain)
	}

	sort.Slice(chains, func(i, j int) bool {
		return chains[i].CreatedAt.After(chains[j].CreatedAt)
	})
	return chains, nil
}

func (s *StateStore) jobPath(jobID string) string {
	return filepath.Join(s.jobsDir(), fmt.Sprintf("%s.json", jobID))
}

func (s *StateStore) chainPath(chainID string) string {
	return filepath.Join(s.chainsDir(), fmt.Sprintf("%s.json", chainID))
}

func (s *StateStore) jobsDir() string {
	return filepath.Join(s.root, "jobs")
}

func (s *StateStore) chainsDir() string {
	return filepath.Join(s.root, "chains")
}

func writeAtomically(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := tmpFile.Name()
	defer os.Remove(tmp)
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err == nil {
		return nil
	}

	// Windows does not atomically replace an existing file with os.Rename.
	// Use rename-to-.bak strategy to avoid a TOCTOU window where the target is
	// absent between Remove and Rename: rename the live file to .bak first, then
	// rename tmp into place, and finally remove the .bak. Concurrent readers may
	// briefly see the stale .bak file but never an absent target.
	bak := path + ".bak"
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		// Step 1: displace the current file to .bak (ignore error if absent).
		_ = os.Rename(path, bak)

		// Step 2: move tmp to the target location.
		if renameErr := os.Rename(tmp, path); renameErr == nil {
			// Success -- clean up the backup; failure here is non-fatal.
			_ = os.Remove(bak)
			return nil
		} else {
			lastErr = renameErr
		}

		// Step 3: restore from .bak so the target is never absent.
		_ = os.Rename(bak, path)

		time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
	}
	return lastErr
}
