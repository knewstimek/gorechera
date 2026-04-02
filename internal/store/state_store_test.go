package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorchera/internal/domain"
)

func TestStateStoreSaveLoadAndListChains(t *testing.T) {
	t.Parallel()

	state := NewStateStore(t.TempDir())
	older := &domain.JobChain{
		ID:           "chain-older",
		Goals:        []domain.ChainGoal{{Goal: "older", Status: "done", JobID: "job-1"}},
		CurrentIndex: 0,
		Status:       "done",
		CreatedAt:    time.Now().UTC().Add(-time.Minute),
		UpdatedAt:    time.Now().UTC().Add(-time.Minute),
	}
	newer := &domain.JobChain{
		ID: "chain-newer",
		Goals: []domain.ChainGoal{
			{Goal: "first", Provider: domain.ProviderMock, MaxSteps: 4, JobID: "job-2", Status: "running"},
			{Goal: "second", Provider: domain.ProviderCodex, MaxSteps: 6, Status: "pending"},
		},
		CurrentIndex: 0,
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	if err := state.SaveChain(context.Background(), older); err != nil {
		t.Fatalf("SaveChain older returned error: %v", err)
	}
	if err := state.SaveChain(context.Background(), newer); err != nil {
		t.Fatalf("SaveChain newer returned error: %v", err)
	}

	loaded, err := state.LoadChain(context.Background(), newer.ID)
	if err != nil {
		t.Fatalf("LoadChain returned error: %v", err)
	}
	if loaded.ID != newer.ID {
		t.Fatalf("expected chain id %q, got %q", newer.ID, loaded.ID)
	}
	if len(loaded.Goals) != 2 {
		t.Fatalf("expected 2 goals, got %d", len(loaded.Goals))
	}
	if loaded.Goals[0].JobID != "job-2" || loaded.Goals[0].Status != "running" {
		t.Fatalf("unexpected loaded first goal: %#v", loaded.Goals[0])
	}
	if loaded.Goals[1].Status != "pending" || loaded.Goals[1].JobID != "" {
		t.Fatalf("unexpected loaded second goal: %#v", loaded.Goals[1])
	}

	chains, err := state.ListChains(context.Background())
	if err != nil {
		t.Fatalf("ListChains returned error: %v", err)
	}
	if len(chains) != 2 {
		t.Fatalf("expected 2 chains, got %d", len(chains))
	}
	if chains[0].ID != newer.ID || chains[1].ID != older.ID {
		t.Fatalf("expected chains sorted by created_at descending, got %q then %q", chains[0].ID, chains[1].ID)
	}
}

func TestStateStoreSaveJobOverwritesExistingFile(t *testing.T) {
	t.Parallel()

	state := NewStateStore(t.TempDir())
	job := &domain.Job{
		ID:        "job-overwrite",
		Status:    domain.JobStatusStarting,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	if err := state.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("SaveJob first write returned error: %v", err)
	}

	job.Status = domain.JobStatusRunning
	job.UpdatedAt = time.Now().UTC()
	if err := state.SaveJob(context.Background(), job); err != nil {
		t.Fatalf("SaveJob overwrite returned error: %v", err)
	}

	loaded, err := state.LoadJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("LoadJob returned error: %v", err)
	}
	if loaded.Status != domain.JobStatusRunning {
		t.Fatalf("expected overwritten job status %q, got %q", domain.JobStatusRunning, loaded.Status)
	}
}

// TestValidateIDRejectsTraversalSequences verifies that IDs containing path traversal sequences
// are rejected before any file-system operation is performed (HIGH-01, HIGH-02).
func TestValidateIDRejectsTraversalSequences(t *testing.T) {
	t.Parallel()

	traversalIDs := []string{
		"../../etc/passwd",
		`..\..\windows\system32`,
		"../secret",
		"..",
		".",
		"",
		"job/malicious",
		"job\\malicious",
		"job id with spaces",
		"job:bad",
		"job*bad",
	}

	for _, id := range traversalIDs {
		id := id
		t.Run("id="+id, func(t *testing.T) {
			t.Parallel()
			if err := validateID(id); err == nil {
				t.Errorf("validateID(%q) expected error but got nil", id)
			}
		})
	}
}

// TestValidateIDAcceptsValidIDs verifies that well-formed IDs pass validation.
func TestValidateIDAcceptsValidIDs(t *testing.T) {
	t.Parallel()

	validIDs := []string{
		"job-20260402-072222.097",
		"chain-20260402-065955.763",
		"chain-older",
		"chain-newer",
		"job-overwrite",
		"job123",
		"a",
		"A_B-C.D",
	}

	for _, id := range validIDs {
		id := id
		t.Run("id="+id, func(t *testing.T) {
			t.Parallel()
			if err := validateID(id); err != nil {
				t.Errorf("validateID(%q) unexpected error: %v", id, err)
			}
		})
	}
}

// TestStateStoreRejectsTraversalJobID verifies that SaveJob and LoadJob reject traversal IDs.
func TestStateStoreRejectsTraversalJobID(t *testing.T) {
	t.Parallel()

	s := NewStateStore(t.TempDir())
	ctx := context.Background()

	badIDs := []string{"../../etc/passwd", `..\..\windows\system32`, "../secret"}
	for _, id := range badIDs {
		id := id
		t.Run("save_"+id, func(t *testing.T) {
			t.Parallel()
			job := &domain.Job{ID: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			err := s.SaveJob(ctx, job)
			if err == nil {
				t.Errorf("SaveJob(%q) expected error but got nil", id)
			}
			if err != nil && !strings.Contains(err.Error(), "invalid ID") {
				t.Errorf("SaveJob(%q) expected 'invalid ID' in error, got: %v", id, err)
			}
		})
		t.Run("load_"+id, func(t *testing.T) {
			t.Parallel()
			_, err := s.LoadJob(ctx, id)
			if err == nil {
				t.Errorf("LoadJob(%q) expected error but got nil", id)
			}
			if err != nil && !strings.Contains(err.Error(), "invalid ID") {
				t.Errorf("LoadJob(%q) expected 'invalid ID' in error, got: %v", id, err)
			}
		})
	}
}

// TestStateStoreRejectsTraversalChainID verifies that SaveChain and LoadChain reject traversal IDs.
func TestStateStoreRejectsTraversalChainID(t *testing.T) {
	t.Parallel()

	s := NewStateStore(t.TempDir())
	ctx := context.Background()

	badIDs := []string{"../../etc/passwd", `..\..\windows\system32`}
	for _, id := range badIDs {
		id := id
		t.Run("save_"+id, func(t *testing.T) {
			t.Parallel()
			chain := &domain.JobChain{ID: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
			err := s.SaveChain(ctx, chain)
			if err == nil {
				t.Errorf("SaveChain(%q) expected error but got nil", id)
			}
		})
		t.Run("load_"+id, func(t *testing.T) {
			t.Parallel()
			_, err := s.LoadChain(ctx, id)
			if err == nil {
				t.Errorf("LoadChain(%q) expected error but got nil", id)
			}
		})
	}
}
