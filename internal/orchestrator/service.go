package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gorechera/internal/domain"
	"gorechera/internal/policy"
	"gorechera/internal/provider"
	runtimeexec "gorechera/internal/runtime"
	"gorechera/internal/schema"
	"gorechera/internal/store"
)

var ErrHarnessOwnershipMismatch = errors.New("harness process not owned by job")

// EventNotification carries a job state change that the MCP server can relay
// to connected clients as a JSON-RPC notification. It is separate from
// domain.Event so that the orchestrator package stays unaware of MCP details.
type EventNotification struct {
	JobID   string
	Kind    string
	Message string
}

type CreateJobInput struct {
	Goal            string
	TechStack       string
	WorkspaceDir    string
	Constraints     []string
	DoneCriteria    []string
	Provider        domain.ProviderName
	RoleProfiles    domain.RoleProfiles
	MaxSteps        int
	StrictnessLevel string // strict | normal | lenient; empty defaults to "normal"
}

type Service struct {
	sessions      *provider.SessionManager
	state         *store.StateStore
	artifacts     *store.ArtifactStore
	approval      policy.Policy
	runtime       *runtimeexec.Runner
	processes     *runtimeexec.ProcessManager
	workspaceRoot string

	// eventChan delivers job state change notifications to external listeners
	// (e.g. the MCP server). Buffered so callers never block when no listener
	// is attached. Excess notifications are silently dropped (see addEvent).
	eventChan chan EventNotification

	harnessMu     sync.Mutex
	harnesses     map[int]runtimeexec.ProcessHandle
	harnessOwners map[int]string
	jobHarnesses  map[string]map[int]struct{}
	// harnessInflight: IO 작업 중인 PID 집합. 소유권 체크와 IO 실행 사이의
	// TOCTOU 윈도우를 닫기 위해 "체크 통과 후 IO 완료 전" 구간을 마킹한다.
	// ownsHarness 측이 inflight PID의 소유자 변경을 블록하도록 체크한다.
	harnessInflight map[int]string // pid -> jobID (현재 IO 점유 중인 job)
}

func NewService(sessions *provider.SessionManager, state *store.StateStore, artifacts *store.ArtifactStore, workspaceRoot string) *Service {
	return &Service{
		sessions:      sessions,
		state:         state,
		artifacts:     artifacts,
		approval:      policy.New(),
		runtime:       runtimeexec.NewRunner(runtimeexec.NewDefaultPolicy()),
		processes:     runtimeexec.NewProcessManager(runtimeexec.NewDefaultPolicy()),
		workspaceRoot: workspaceRoot,
		// Buffer 100 notifications so that burst events during a fast-running job
		// do not stall the orchestrator goroutine even if the MCP listener lags.
		eventChan:       make(chan EventNotification, 100),
		harnesses:       make(map[int]runtimeexec.ProcessHandle),
		harnessOwners:   make(map[int]string),
		jobHarnesses:    make(map[string]map[int]struct{}),
		harnessInflight: make(map[int]string),
	}
}

// EventChan returns a read-only channel that receives a notification each time
// a job event is appended. The channel is never closed; callers should select
// with a done/context channel when they want to stop listening.
func (s *Service) EventChan() <-chan EventNotification {
	return s.eventChan
}

func (s *Service) Start(ctx context.Context, input CreateJobInput) (*domain.Job, error) {
	now := time.Now().UTC()
	if input.MaxSteps <= 0 {
		input.MaxSteps = 8
	}
	if input.Provider == "" {
		input.Provider = domain.ProviderMock
	}
	roleProfiles := input.RoleProfiles.Normalize(input.Provider)

	job := &domain.Job{
		ID:              newJobID(now),
		Goal:            strings.TrimSpace(input.Goal),
		TechStack:       strings.TrimSpace(input.TechStack),
		WorkspaceDir:    firstNonEmpty(strings.TrimSpace(input.WorkspaceDir), s.workspaceRoot),
		Constraints:     input.Constraints,
		DoneCriteria:    input.DoneCriteria,
		StrictnessLevel: normalizeStrictnessLevel(input.StrictnessLevel),
		RoleProfiles:    roleProfiles,
		Status:          domain.JobStatusStarting,
		Provider:        input.Provider,
		MaxSteps:        input.MaxSteps,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	job.LeaderContextSummary = fmt.Sprintf("Goal: %s", job.Goal)
	s.addEvent(job, "job_created", "job created")

	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return s.runLoop(ctx, job)
}

// StartAsync creates a job synchronously (so the caller gets the ID immediately)
// and runs the main loop in a background goroutine. Use this when the caller
// cannot block until the job finishes (e.g. an MCP stdio server).
func (s *Service) StartAsync(ctx context.Context, input CreateJobInput) (*domain.Job, error) {
	now := time.Now().UTC()
	if input.MaxSteps <= 0 {
		input.MaxSteps = 8
	}
	if input.Provider == "" {
		input.Provider = domain.ProviderMock
	}
	roleProfiles := input.RoleProfiles.Normalize(input.Provider)

	job := &domain.Job{
		ID:              newJobID(now),
		Goal:            strings.TrimSpace(input.Goal),
		TechStack:       strings.TrimSpace(input.TechStack),
		WorkspaceDir:    firstNonEmpty(strings.TrimSpace(input.WorkspaceDir), s.workspaceRoot),
		Constraints:     input.Constraints,
		DoneCriteria:    input.DoneCriteria,
		StrictnessLevel: normalizeStrictnessLevel(input.StrictnessLevel),
		RoleProfiles:    roleProfiles,
		Status:          domain.JobStatusStarting,
		Provider:        input.Provider,
		MaxSteps:        input.MaxSteps,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	job.LeaderContextSummary = fmt.Sprintf("Goal: %s", job.Goal)
	s.addEvent(job, "job_created", "job created")

	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}

	// Snapshot the ID before handing job to the goroutine to avoid a data race.
	go func() {
		s.runLoop(context.Background(), job) //nolint:errcheck
	}()
	return job, nil
}

func (s *Service) Resume(ctx context.Context, jobID string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Status == domain.JobStatusDone || job.Status == domain.JobStatusFailed {
		return job, nil
	}
	s.addEvent(job, "job_resumed", "job resumed")
	return s.runLoop(ctx, job)
}

func (s *Service) Cancel(ctx context.Context, jobID, reason string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Status == domain.JobStatusDone {
		return job, fmt.Errorf("cannot cancel completed job")
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "operator cancelled job"
	}
	job.Status = domain.JobStatusBlocked
	job.BlockedReason = fmt.Sprintf("cancelled by operator: %s", reason)
	job.FailureReason = ""
	job.PendingApproval = nil
	job.LeaderContextSummary = job.BlockedReason
	s.addEvent(job, "job_cancelled", job.BlockedReason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) Retry(ctx context.Context, jobID string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Status != domain.JobStatusBlocked && job.Status != domain.JobStatusFailed {
		return job, fmt.Errorf("retry is only allowed for blocked or failed jobs")
	}

	job.RetryCount++
	job.Status = domain.JobStatusRunning
	job.BlockedReason = ""
	job.FailureReason = ""
	job.PendingApproval = nil
	job.LeaderContextSummary = fmt.Sprintf("retry #%d requested", job.RetryCount)
	s.addEvent(job, "job_retry_requested", job.LeaderContextSummary)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return s.runLoop(ctx, job)
}

func (s *Service) Approve(ctx context.Context, jobID string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.PendingApproval == nil {
		return job, fmt.Errorf("no pending approval for job")
	}

	pending := *job.PendingApproval
	leader := domain.LeaderOutput{
		Action:       "run_system",
		Target:       pending.Target,
		TaskType:     pending.TaskType,
		TaskText:     pending.TaskText,
		SystemAction: pending.SystemAction,
	}
	job.PendingApproval = nil
	job.BlockedReason = ""
	job.FailureReason = ""
	job.Status = domain.JobStatusRunning
	job.LeaderContextSummary = fmt.Sprintf("operator approved step %d", pending.StepIndex)
	s.addEvent(job, "job_approved", job.LeaderContextSummary)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	if err := s.runSystemStepWithApproval(ctx, job, leader, true); err != nil {
		return nil, err
	}
	if job.Status == domain.JobStatusBlocked || job.Status == domain.JobStatusFailed {
		return job, nil
	}
	return s.runLoop(ctx, job)
}

func (s *Service) Reject(ctx context.Context, jobID, reason string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.PendingApproval == nil {
		return job, fmt.Errorf("no pending approval for job")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "operator rejected pending approval"
	}
	job.Status = domain.JobStatusBlocked
	job.BlockedReason = reason
	job.FailureReason = ""
	job.LeaderContextSummary = reason
	job.PendingApproval = nil
	s.addEvent(job, "job_rejected", reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) Get(ctx context.Context, jobID string) (*domain.Job, error) {
	return s.state.LoadJob(ctx, jobID)
}

func (s *Service) StartHarnessProcess(ctx context.Context, req runtimeexec.StartRequest) (runtimeexec.ProcessHandle, error) {
	handle, err := s.processes.Start(ctx, req)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle("", handle)
	return handle, nil
}

func (s *Service) GetHarnessProcess(ctx context.Context, pid int) (runtimeexec.ProcessHandle, error) {
	handle, err := s.processes.Status(ctx, pid)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle("", handle)
	return handle, nil
}

func (s *Service) StopHarnessProcess(ctx context.Context, pid int) (runtimeexec.ProcessHandle, error) {
	handle, err := s.processes.Stop(ctx, pid)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle("", handle)
	return handle, nil
}

func (s *Service) ListHarnessProcesses(ctx context.Context) ([]runtimeexec.ProcessHandle, error) {
	s.harnessMu.Lock()
	pids := make([]int, 0, len(s.harnesses))
	for pid := range s.harnesses {
		pids = append(pids, pid)
	}
	s.harnessMu.Unlock()

	handles := make([]runtimeexec.ProcessHandle, 0, len(pids))
	for _, pid := range pids {
		handle, err := s.processes.Status(ctx, pid)
		if err != nil {
			return nil, err
		}
		s.trackHarnessHandle("", handle)
		handles = append(handles, handle)
	}

	sort.Slice(handles, func(i, j int) bool {
		if handles[i].StartedAt.Equal(handles[j].StartedAt) {
			return handles[i].PID < handles[j].PID
		}
		return handles[i].StartedAt.Before(handles[j].StartedAt)
	})
	return handles, nil
}

func (s *Service) StartJobHarnessProcess(ctx context.Context, jobID string, req runtimeexec.StartRequest) (runtimeexec.ProcessHandle, error) {
	if _, err := s.state.LoadJob(ctx, jobID); err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	handle, err := s.processes.Start(ctx, req)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle(jobID, handle)
	return handle, nil
}

func (s *Service) GetJobHarnessProcess(ctx context.Context, jobID string, pid int) (runtimeexec.ProcessHandle, error) {
	// claimHarness: 소유권 확인과 inflight 등록을 하나의 락 구간에서 원자적으로 수행한다.
	// 이렇게 해야 "체크 통과 후 IO 실행 전" 사이에 다른 goroutine이 소유자를 바꾸는
	// TOCTOU 레이스를 차단할 수 있다.
	if err := s.claimHarness(jobID, pid); err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	defer s.releaseHarnessClaim(pid)

	handle, err := s.processes.Status(ctx, pid)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle(jobID, handle)
	return handle, nil
}

func (s *Service) StopJobHarnessProcess(ctx context.Context, jobID string, pid int) (runtimeexec.ProcessHandle, error) {
	// claimHarness: 소유권 확인과 inflight 등록을 하나의 락 구간에서 원자적으로 수행한다.
	// Stop은 취소 불가능한 IO이므로 TOCTOU 보호가 특히 중요하다.
	if err := s.claimHarness(jobID, pid); err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	defer s.releaseHarnessClaim(pid)

	handle, err := s.processes.Stop(ctx, pid)
	if err != nil {
		return runtimeexec.ProcessHandle{}, err
	}
	s.trackHarnessHandle(jobID, handle)
	return handle, nil
}

func (s *Service) ListJobHarnessProcesses(ctx context.Context, jobID string) ([]runtimeexec.ProcessHandle, error) {
	pids := s.jobHarnessPIDs(jobID)
	handles := make([]runtimeexec.ProcessHandle, 0, len(pids))
	for _, pid := range pids {
		// claimHarness로 소유권을 원자적으로 확인하고 inflight 마킹한다.
		// 목록 조회는 읽기 전용이므로 TOCTOU 위험이 낮지만, 루프 중간에
		// 다른 job이 소유권을 가져가면 잘못된 상태 정보를 반환할 수 있다.
		if err := s.claimHarness(jobID, pid); err != nil {
			// 목록 조회 중 소유권을 잃은 PID는 건너뛴다 (결과 집합에서 제외).
			continue
		}
		handle, err := s.processes.Status(ctx, pid)
		s.releaseHarnessClaim(pid)
		if err != nil {
			return nil, err
		}
		s.trackHarnessHandle(jobID, handle)
		handles = append(handles, handle)
	}

	sort.Slice(handles, func(i, j int) bool {
		if handles[i].StartedAt.Equal(handles[j].StartedAt) {
			return handles[i].PID < handles[j].PID
		}
		return handles[i].StartedAt.Before(handles[j].StartedAt)
	})
	return handles, nil
}

func (s *Service) List(ctx context.Context) ([]domain.Job, error) {
	return s.state.ListJobs(ctx)
}

func (s *Service) runLoop(ctx context.Context, job *domain.Job) (*domain.Job, error) {
	if len(job.PlanningArtifacts) == 0 || strings.TrimSpace(job.SprintContractRef) == "" {
		if err := s.ensurePlanning(ctx, job); err != nil {
			return nil, err
		}
		if job.Status == domain.JobStatusBlocked || job.Status == domain.JobStatusFailed {
			return job, nil
		}
	}

	leaderRetryPending := false
	completionRetryPending := false
	completionRetryStepCount := 0
	for job.CurrentStep < job.MaxSteps {
		job.Status = domain.JobStatusWaitingLeader
		s.touch(job)
		s.addEvent(job, "leader_requested", "requesting leader action")
		if !leaderRetryPending {
			if err := s.state.SaveJob(ctx, job); err != nil {
				return nil, err
			}
		} else {
			// The evaluator already persisted the blocked gate result. Avoid an
			// immediate second save before the retrying leader turn on Windows.
			leaderRetryPending = false
		}

		rawLeader, err := s.sessions.RunLeader(ctx, *job)
		if err != nil {
			return s.failJob(ctx, job, fmt.Sprintf("leader execution failed: %v", err))
		}

		var leader domain.LeaderOutput
		if err := json.Unmarshal([]byte(rawLeader), &leader); err != nil {
			return s.failJob(ctx, job, fmt.Sprintf("invalid leader json: %v", err))
		}
		if err := schema.ValidateLeaderOutput(leader); err != nil {
			return s.failJob(ctx, job, fmt.Sprintf("leader schema validation failed: %v", err))
		}

		switch leader.Action {
		case "run_worker":
			if completionRetryPending {
				job.BlockedReason = ""
				completionRetryPending = false
			}
			if err := s.runWorkerStep(ctx, job, leader); err != nil {
				return nil, err
			}
			if job.Status == domain.JobStatusBlocked || job.Status == domain.JobStatusFailed {
				return job, nil
			}
		case "run_workers":
			if completionRetryPending {
				job.BlockedReason = ""
				completionRetryPending = false
			}
			if err := s.runWorkerStep(ctx, job, leader); err != nil {
				return nil, err
			}
			if job.Status == domain.JobStatusBlocked || job.Status == domain.JobStatusFailed {
				return job, nil
			}
		case "run_system":
			if completionRetryPending {
				job.BlockedReason = ""
				completionRetryPending = false
			}
			if err := s.runSystemStep(ctx, job, leader); err != nil {
				return nil, err
			}
			if job.Status == domain.JobStatusBlocked || job.Status == domain.JobStatusFailed {
				return job, nil
			}
		case "summarize":
			job.Summary = leader.Reason
			job.LeaderContextSummary = leader.NextHint
			s.addEvent(job, "leader_summary", "leader emitted a summary")
			s.touch(job)
			if err := s.state.SaveJob(ctx, job); err != nil {
				return nil, err
			}
		case "complete":
			if completionRetryPending && len(job.Steps) == completionRetryStepCount {
				job.Status = domain.JobStatusBlocked
				return job, nil
			}
			report, err := s.evaluateCompletion(ctx, job)
			if err != nil {
				return nil, err
			}
			if !report.Passed {
				// An evaluator "blocked" result means the job is recoverable if the
				// leader schedules the missing work, so keep the loop alive.
				if report.Status == "blocked" {
					completionRetryPending = true
					completionRetryStepCount = len(job.Steps)
					leaderRetryPending = true
					continue
				}
				return job, nil
			}
			job.Status = domain.JobStatusDone
			job.Summary = leader.Reason
			s.addEvent(job, "job_completed", leader.Reason)
			s.touch(job)
			if err := s.state.SaveJob(ctx, job); err != nil {
				return nil, err
			}
			return job, nil
		case "fail":
			return s.failJob(ctx, job, leader.Reason)
		case "blocked":
			job.Status = domain.JobStatusBlocked
			job.BlockedReason = leader.Reason
			s.addEvent(job, "job_blocked", leader.Reason)
			s.touch(job)
			if err := s.state.SaveJob(ctx, job); err != nil {
				return nil, err
			}
			return job, nil
		default:
			return s.failJob(ctx, job, fmt.Sprintf("unrecognized leader action: %q", leader.Action))
		}
	}

	job.Status = domain.JobStatusBlocked
	job.BlockedReason = "max_steps_exceeded"
	s.addEvent(job, "job_blocked", "max_steps_exceeded")
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) runWorkerStep(ctx context.Context, job *domain.Job, leader domain.LeaderOutput) error {
	plans, err := buildWorkerPlans(leader)
	if err != nil {
		_, blockErr := s.blockJob(ctx, job, fmt.Sprintf("parallel fan-out blocked: %v", err))
		return blockErr
	}
	if len(plans) > 1 {
		return s.runParallelWorkerPlans(ctx, job, plans)
	}

	task := decorateTaskForVerification(*job, plans[0].Task)
	job.Status = domain.JobStatusWaitingWorker
	job.CurrentStep++
	step := domain.Step{
		Index:     job.CurrentStep,
		Target:    task.Target,
		TaskType:  task.TaskType,
		TaskText:  task.TaskText,
		Status:    domain.StepStatusActive,
		StartedAt: time.Now().UTC(),
	}
	job.Steps = append(job.Steps, step)
	s.addEvent(job, "worker_requested", fmt.Sprintf("%s:%s", task.Target, task.TaskType))
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return err
	}

	rawWorker, err := s.sessions.RunWorker(ctx, *job, task)
	if err != nil {
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("worker execution failed: %v", err))
		return failErr
	}

	var worker domain.WorkerOutput
	if err := json.Unmarshal([]byte(rawWorker), &worker); err != nil {
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("invalid worker json: %v", err))
		return failErr
	}
	if err := schema.ValidateWorkerOutput(worker); err != nil {
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("worker schema validation failed: %v", err))
		return failErr
	}

	artifactPaths, err := s.artifacts.MaterializeWorkerArtifacts(job.ID, step.Index, worker)
	if err != nil {
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("artifact materialization failed: %v", err))
		return failErr
	}

	last := &job.Steps[len(job.Steps)-1]
	last.Summary = worker.Summary
	last.Artifacts = artifactPaths
	last.BlockedReason = worker.BlockedReason
	last.ErrorReason = worker.ErrorReason
	last.FinishedAt = time.Now().UTC()

	switch worker.Status {
	case "success":
		last.Status = domain.StepStatusSucceeded
		job.Status = domain.JobStatusRunning
		s.addEvent(job, "worker_succeeded", worker.Summary)
	case "blocked":
		last.Status = domain.StepStatusBlocked
		job.Status = domain.JobStatusBlocked
		job.BlockedReason = worker.BlockedReason
		s.addEvent(job, "worker_blocked", worker.BlockedReason)
	case "failed":
		last.Status = domain.StepStatusFailed
		job.Status = domain.JobStatusRunning
		job.FailureReason = worker.ErrorReason
		s.addEvent(job, "worker_failed", worker.ErrorReason)
	}

	job.LeaderContextSummary = worker.Summary
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) runSystemStep(ctx context.Context, job *domain.Job, leader domain.LeaderOutput) error {
	return s.runSystemStepWithApproval(ctx, job, leader, false)
}

func (s *Service) runSystemStepWithApproval(ctx context.Context, job *domain.Job, leader domain.LeaderOutput, approvalGranted bool) error {
	job.Status = domain.JobStatusWaitingWorker
	job.CurrentStep++
	step := domain.Step{
		Index:     job.CurrentStep,
		Target:    leader.Target,
		TaskType:  leader.TaskType,
		TaskText:  leader.TaskText,
		Status:    domain.StepStatusActive,
		StartedAt: time.Now().UTC(),
	}
	job.Steps = append(job.Steps, step)
	s.addEvent(job, "system_requested", fmt.Sprintf("%s:%s", leader.Target, leader.TaskType))
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return err
	}

	req, approvalReq, err := s.buildSystemRequest(*job, leader)
	if err != nil {
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("invalid system action: %v", err))
		return failErr
	}

	decision := s.approval.Evaluate(approvalReq)
	if !approvalGranted && decision.Decision == policy.DecisionBlock {
		last := &job.Steps[len(job.Steps)-1]
		last.Status = domain.StepStatusBlocked
		last.BlockedReason = decision.Reason
		last.Summary = decision.Reason
		last.FinishedAt = time.Now().UTC()
		job.Status = domain.JobStatusBlocked
		job.BlockedReason = decision.Reason
		job.PendingApproval = &domain.PendingApproval{
			StepIndex:    last.Index,
			Reason:       decision.Reason,
			RequestedAt:  time.Now().UTC(),
			Target:       leader.Target,
			TaskType:     leader.TaskType,
			TaskText:     leader.TaskText,
			SystemAction: leader.SystemAction,
		}
		job.LeaderContextSummary = decision.Reason
		s.addEvent(job, "system_blocked", decision.Reason)
		s.touch(job)
		return s.state.SaveJob(ctx, job)
	}
	job.PendingApproval = nil

	result, runErr := s.runtime.Run(ctx, req)
	artifacts := []string(nil)
	if result.Command != "" {
		artifacts, err = s.artifacts.MaterializeSystemResult(job.ID, step.Index, result)
		if err != nil {
			_, failErr := s.failJob(ctx, job, fmt.Sprintf("runtime artifact materialization failed: %v", err))
			return failErr
		}
	}

	last := &job.Steps[len(job.Steps)-1]
	last.Artifacts = artifacts
	last.FinishedAt = time.Now().UTC()
	last.Summary = summarizeSystemResult(result)

	switch {
	case runErr == nil:
		last.Status = domain.StepStatusSucceeded
		job.Status = domain.JobStatusRunning
		s.addEvent(job, "system_succeeded", last.Summary)
	case errors.Is(runErr, runtimeexec.ErrNotAllowed):
		last.Status = domain.StepStatusBlocked
		last.BlockedReason = runErr.Error()
		job.Status = domain.JobStatusBlocked
		job.BlockedReason = runErr.Error()
		s.addEvent(job, "system_blocked", runErr.Error())
	case result.TimedOut:
		last.Status = domain.StepStatusFailed
		last.ErrorReason = runErr.Error()
		job.Status = domain.JobStatusRunning
		job.FailureReason = runErr.Error()
		s.addEvent(job, "system_failed", runErr.Error())
	default:
		last.Status = domain.StepStatusFailed
		if runErr != nil {
			last.ErrorReason = runErr.Error()
			job.FailureReason = runErr.Error()
			s.addEvent(job, "system_failed", runErr.Error())
		}
		job.Status = domain.JobStatusRunning
	}

	job.LeaderContextSummary = last.Summary
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) failJob(ctx context.Context, job *domain.Job, reason string) (*domain.Job, error) {
	job.Status = domain.JobStatusFailed
	job.FailureReason = reason
	s.addEvent(job, "job_failed", reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) blockJob(ctx context.Context, job *domain.Job, reason string) (*domain.Job, error) {
	job.Status = domain.JobStatusBlocked
	job.BlockedReason = reason
	job.FailureReason = ""
	job.LeaderContextSummary = reason
	s.addEvent(job, "job_blocked", reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) addEvent(job *domain.Job, kind, message string) {
	job.Events = append(job.Events, domain.Event{
		Time:    time.Now().UTC(),
		Kind:    kind,
		Message: message,
	})
	// Non-blocking send: if the buffer is full the notification is dropped
	// rather than stalling the orchestrator. Listeners that need guaranteed
	// delivery should poll via Get/List instead.
	select {
	case s.eventChan <- EventNotification{JobID: job.ID, Kind: kind, Message: message}:
	default:
	}
}

func (s *Service) trackHarnessHandle(jobID string, handle runtimeexec.ProcessHandle) {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	s.harnesses[handle.PID] = handle
	if strings.TrimSpace(jobID) == "" {
		return
	}
	// inflight 중인 PID에 대해서는 소유자 변경을 거부한다.
	// claimHarness가 소유권을 확인하고 IO를 수행하는 동안 다른 job이
	// 소유권을 가로채는 TOCTOU를 방지하기 위함이다.
	if inflightOwner, busy := s.harnessInflight[handle.PID]; busy && inflightOwner != jobID {
		// 현재 다른 job이 이 PID를 IO 점유 중이므로 소유자 갱신을 건너뛴다.
		return
	}
	s.harnessOwners[handle.PID] = jobID
	owned := s.jobHarnesses[jobID]
	if owned == nil {
		owned = make(map[int]struct{})
		s.jobHarnesses[jobID] = owned
	}
	owned[handle.PID] = struct{}{}
}

func (s *Service) ownsHarness(jobID string, pid int) bool {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	owner, ok := s.harnessOwners[pid]
	if !ok {
		return false
	}
	return owner == jobID
}

// claimHarness: 소유권 확인과 inflight 등록을 하나의 락 구간에서 원자적으로 수행한다.
// 이 함수가 성공하면 반드시 releaseHarnessClaim(pid)을 defer로 호출해야 한다.
// 이미 다른 job이 inflight 점유 중이거나 소유자가 다르면 ErrHarnessOwnershipMismatch를 반환한다.
func (s *Service) claimHarness(jobID string, pid int) error {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()

	// 다른 job이 이미 이 PID를 IO 점유 중이면 거부한다.
	if inflightOwner, busy := s.harnessInflight[pid]; busy && inflightOwner != jobID {
		return fmt.Errorf("%w: pid %d is currently in use by job %s", ErrHarnessOwnershipMismatch, pid, inflightOwner)
	}

	// 소유권 확인
	owner, ok := s.harnessOwners[pid]
	if !ok || owner != jobID {
		return fmt.Errorf("%w: pid %d is not owned by job %s", ErrHarnessOwnershipMismatch, pid, jobID)
	}

	// IO 점유 마킹 -- IO 완료 전까지 다른 job의 소유권 취득을 차단한다.
	s.harnessInflight[pid] = jobID
	return nil
}

// releaseHarnessClaim: claimHarness로 등록한 inflight 마킹을 해제한다.
// IO 완료(성공/실패 불문) 후 반드시 호출해야 한다.
func (s *Service) releaseHarnessClaim(pid int) {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	delete(s.harnessInflight, pid)
}

func (s *Service) jobHarnessPIDs(jobID string) []int {
	s.harnessMu.Lock()
	defer s.harnessMu.Unlock()
	owned := s.jobHarnesses[jobID]
	if len(owned) == 0 {
		return nil
	}
	pids := make([]int, 0, len(owned))
	for pid := range owned {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids
}

func (s *Service) touch(job *domain.Job) {
	job.UpdatedAt = time.Now().UTC()
}

func newJobID(now time.Time) string {
	return fmt.Sprintf("job-%s", now.Format("20060102-150405.000"))
}

func (s *Service) buildSystemRequest(job domain.Job, leader domain.LeaderOutput) (runtimeexec.Request, policy.Request, error) {
	if leader.SystemAction == nil {
		return runtimeexec.Request{}, policy.Request{}, fmt.Errorf("system_action is required")
	}

	workdir := resolveSystemWorkdir(firstNonEmpty(job.WorkspaceDir, s.workspaceRoot), leader.SystemAction.Workdir)
	category, actionType, err := mapSystemTask(leader.TaskType)
	if err != nil {
		return runtimeexec.Request{}, policy.Request{}, err
	}

	timeout := 2 * time.Minute
	req := runtimeexec.Request{
		Category: category,
		Command:  leader.SystemAction.Command,
		Args:     append([]string(nil), leader.SystemAction.Args...),
		Dir:      workdir,
		Timeout:  timeout,
	}
	policyReq := policy.Request{
		Action:       actionType,
		TargetScopes: []policy.ResourceScope{classifyScope(firstNonEmpty(job.WorkspaceDir, s.workspaceRoot), workdir)},
		Command:      leader.SystemAction.Command,
		Details:      leader.TaskText,
	}
	return req, policyReq, nil
}

func mapSystemTask(taskType string) (runtimeexec.Category, policy.ActionType, error) {
	switch taskType {
	case "build":
		return runtimeexec.CategoryBuild, policy.ActionBuild, nil
	case "test":
		return runtimeexec.CategoryTest, policy.ActionTest, nil
	case "lint":
		return runtimeexec.CategoryLint, policy.ActionLint, nil
	case "search":
		return runtimeexec.CategorySearch, policy.ActionSearch, nil
	case "command":
		return runtimeexec.CategoryCommand, policy.ActionCommand, nil
	default:
		return "", "", fmt.Errorf("unsupported system task type %q", taskType)
	}
}

func resolveSystemWorkdir(workspaceRoot, actionWorkdir string) string {
	base := firstNonEmpty(actionWorkdir, workspaceRoot)
	if filepath.IsAbs(base) {
		return filepath.Clean(base)
	}
	root := firstNonEmpty(workspaceRoot, ".")
	return filepath.Clean(filepath.Join(root, base))
}

func classifyScope(workspaceRoot, target string) policy.ResourceScope {
	rootAbs, err := filepath.Abs(firstNonEmpty(workspaceRoot, "."))
	if err != nil {
		return policy.ResourceUnknown
	}
	targetAbs, err := filepath.Abs(firstNonEmpty(target, rootAbs))
	if err != nil {
		return policy.ResourceUnknown
	}
	rel, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil {
		return policy.ResourceUnknown
	}
	if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..") {
		return policy.ResourceWorkspaceLocal
	}
	return policy.ResourceWorkspaceOutside
}

func summarizeSystemResult(result runtimeexec.Result) string {
	if result.Command == "" {
		return "system action produced no runtime result"
	}
	return fmt.Sprintf("%s %s exited with code %d", result.Category, result.Command, result.ExitCode)
}
