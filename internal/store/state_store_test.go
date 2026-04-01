package store

import (
	"context"
	"testing"
	"time"

	"gorechera/internal/domain"
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
