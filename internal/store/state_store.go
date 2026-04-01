package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorechera/internal/domain"
)

type StateStore struct {
	root string
}

func NewStateStore(root string) *StateStore {
	return &StateStore{root: root}
}

func (s *StateStore) SaveJob(_ context.Context, job *domain.Job) error {
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
