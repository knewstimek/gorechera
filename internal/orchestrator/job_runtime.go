package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gorchera/internal/domain"
)

// leaseValidIDRegexp mirrors store.validIDRegexp to prevent path traversal via jobID.
// Allows only characters that are safe as a file-system path component.
var leaseValidIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9_\-.]+$`)

// validateLeaseID returns an error if jobID could cause path traversal.
func validateLeaseID(id string) error {
	if id == "." || id == ".." {
		return fmt.Errorf("invalid job ID %q: reserved filesystem path component", id)
	}
	if !leaseValidIDRegexp.MatchString(id) {
		return fmt.Errorf("invalid job ID %q: must match ^[a-zA-Z0-9_-.]+$", id)
	}
	return nil
}

const (
	jobLeaseHeartbeatInterval = 15 * time.Second
	jobLeaseStaleAfter        = 45 * time.Second
)

type jobLease struct {
	JobID        string    `json:"job_id"`
	RunOwnerID   string    `json:"run_owner_id"`
	HeartbeatAt  time.Time `json:"heartbeat_at"`
	WorkspaceDir string    `json:"workspace_dir,omitempty"`
}

func newServiceInstanceID() string {
	return fmt.Sprintf("svc-%d", time.Now().UTC().UnixNano())
}

func (s *Service) leaseDir() string {
	return filepath.Join(firstNonEmpty(s.workspaceRoot, "."), ".gorchera", "leases")
}

func (s *Service) leasePath(jobID string) string {
	return filepath.Join(s.leaseDir(), strings.TrimSpace(jobID)+".json")
}

func (s *Service) startJobLease(job *domain.Job) func() {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return func() {}
	}

	now := time.Now().UTC()
	job.RunOwnerID = s.instanceID
	job.RunHeartbeatAt = now
	if err := s.state.SaveJob(context.Background(), job); err != nil {
		log.Printf("[gorchera] lease: failed to persist owner for job %s: %v", job.ID, err)
	}
	if err := s.writeLease(job.ID, job.WorkspaceDir, now); err != nil {
		log.Printf("[gorchera] lease: failed to create lease for job %s: %v", job.ID, err)
	}

	stop := make(chan struct{})
	go func(jobID string, workspaceDir string) {
		ticker := time.NewTicker(jobLeaseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := s.writeLease(jobID, workspaceDir, time.Now().UTC()); err != nil {
					log.Printf("[gorchera] lease: failed to refresh lease for job %s: %v", jobID, err)
				}
			case <-stop:
				return
			}
		}
	}(job.ID, job.WorkspaceDir)

	return func() {
		close(stop)
	}
}

func (s *Service) writeLease(jobID, workspaceDir string, heartbeatAt time.Time) error {
	// Validate before using jobID as a path component to prevent traversal.
	if err := validateLeaseID(jobID); err != nil {
		return err
	}
	if err := os.MkdirAll(s.leaseDir(), 0o755); err != nil {
		return err
	}
	payload := jobLease{
		JobID:        strings.TrimSpace(jobID),
		RunOwnerID:   s.instanceID,
		HeartbeatAt:  heartbeatAt.UTC(),
		WorkspaceDir: strings.TrimSpace(workspaceDir),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.leasePath(jobID), data, 0o644)
}

func (s *Service) clearJobRuntimeState(job *domain.Job) {
	if job == nil {
		return
	}
	job.RunOwnerID = ""
	job.RunHeartbeatAt = time.Time{}
	// validateLeaseID before Remove to stay consistent; invalid IDs are simply
	// ignored since there is no lease file to clean up.
	if err := validateLeaseID(job.ID); err != nil {
		log.Printf("[gorchera] lease: skipping remove for invalid job ID %q: %v", job.ID, err)
		return
	}
	if err := os.Remove(s.leasePath(job.ID)); err != nil && !os.IsNotExist(err) {
		log.Printf("[gorchera] lease: failed to remove lease for job %s: %v", job.ID, err)
	}
}

func (s *Service) finalizeJobLease(job *domain.Job) {
	if job == nil {
		return
	}
	s.clearJobRuntimeState(job)
	if err := s.state.SaveJob(context.Background(), job); err != nil {
		log.Printf("[gorchera] lease: failed to clear owner for job %s: %v", job.ID, err)
	}
}

func (s *Service) interruptJob(ctx context.Context, job *domain.Job, reason string) error {
	if job == nil || !isRecoverableJobStatus(job.Status) {
		return nil
	}

	reason = firstNonEmpty(strings.TrimSpace(reason), "job interrupted by orchestrator shutdown")
	if last := activeStep(job); last != nil {
		last.Status = domain.StepStatusBlocked
		last.BlockedReason = reason
		last.Summary = reason
		if last.FinishedAt.IsZero() {
			last.FinishedAt = time.Now().UTC()
		}
	}

	job.Status = domain.JobStatusBlocked
	job.BlockedReason = reason
	job.FailureReason = ""
	job.PendingApproval = nil
	job.LeaderContextSummary = sanitizeLeaderContext(reason)
	s.clearJobRuntimeState(job)
	s.addEvent(job, "job_interrupted", reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return err
	}
	return s.handleChainTerminalState(ctx, job)
}

func (s *Service) isStaleJob(job domain.Job, now time.Time) bool {
	if !isRecoverableJobStatus(job.Status) {
		return false
	}
	lease, err := s.readLease(job.ID)
	switch {
	case err == nil:
		return now.Sub(lease.HeartbeatAt) > jobLeaseStaleAfter
	case os.IsNotExist(err):
		if strings.TrimSpace(job.RunOwnerID) == "" {
			return false
		}
		if !job.RunHeartbeatAt.IsZero() {
			return now.Sub(job.RunHeartbeatAt) > jobLeaseStaleAfter
		}
		return now.Sub(job.UpdatedAt) > jobLeaseStaleAfter
	default:
		log.Printf("[gorchera] stale sweep: failed to read lease for job %s: %v", job.ID, err)
		return false
	}
}

func (s *Service) readLease(jobID string) (jobLease, error) {
	// Validate before using jobID as a path component to prevent traversal.
	if err := validateLeaseID(jobID); err != nil {
		return jobLease{}, err
	}
	data, err := os.ReadFile(s.leasePath(jobID))
	if err != nil {
		return jobLease{}, err
	}
	var lease jobLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return jobLease{}, err
	}
	return lease, nil
}

func isRecoverableJobStatus(status domain.JobStatus) bool {
	switch status {
	case domain.JobStatusStarting, domain.JobStatusPlanning, domain.JobStatusRunning, domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
		return true
	default:
		return false
	}
}

func activeStep(job *domain.Job) *domain.Step {
	if job == nil {
		return nil
	}
	for i := len(job.Steps) - 1; i >= 0; i-- {
		if job.Steps[i].Status == domain.StepStatusActive {
			return &job.Steps[i]
		}
	}
	return nil
}

func startupInterruptionReason(job domain.Job) string {
	return fmt.Sprintf("job interrupted while in %s; operator must resume or retry explicitly", job.Status)
}
