package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorchera/internal/domain"
	"gorchera/internal/policy"
	"gorchera/internal/provider"
	runtimeexec "gorchera/internal/runtime"
	"gorchera/internal/schema"
	"gorchera/internal/store"
)

var ErrHarnessOwnershipMismatch = errors.New("harness process not owned by job")

const (
	providerRetryLimit     = 3
	providerRetryBaseDelay = 250 * time.Millisecond
	roughCostPerTokenUSD   = 0.000001
)

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
	RoleOverrides   map[string]domain.RoleProfile
	MaxSteps        int
	StrictnessLevel string // strict | normal | lenient; empty defaults to "normal"
	ContextMode     string // full | summary | minimal; empty defaults to "full"
	ChainID         string
	ChainGoalIndex  int
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

	// shutdownCtx is cancelled by Shutdown() to signal all background job
	// goroutines (started by startPreparedJobAsync) to stop.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

func NewService(sessions *provider.SessionManager, state *store.StateStore, artifacts *store.ArtifactStore, workspaceRoot string) *Service {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
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
		shutdownCtx:     shutdownCtx,
		shutdownCancel:  shutdownCancel,
	}
}

// Shutdown cancels the service-level context, signalling all background job
// goroutines to stop. It is safe to call multiple times.
func (s *Service) Shutdown() {
	s.shutdownCancel()
}

// RecoverJobs resumes any jobs that were in a non-terminal state when the
// server last stopped. Without this, jobs created just before an MCP restart
// would be stuck in "starting" or "waiting_*" forever because the goroutine
// that drives runLoop was lost. Call this once after NewService.
func (s *Service) RecoverJobs() {
	ctx := context.Background()
	jobs, err := s.state.ListJobs(ctx)
	if err != nil {
		log.Printf("[gorchera] recovery: failed to list jobs: %v", err)
		return
	}
	recovered := 0
	for i := range jobs {
		job := &jobs[i]
		switch job.Status {
		case domain.JobStatusStarting, domain.JobStatusRunning,
			domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
			log.Printf("[gorchera] recovery: resuming job %s (status=%s)", job.ID, job.Status)
			go func(j *domain.Job) {
				s.runLoop(s.shutdownCtx, j) //nolint:errcheck
			}(job)
			recovered++
		}
	}
	if recovered > 0 {
		log.Printf("[gorchera] recovery: resumed %d jobs", recovered)
	}
}

// EventChan returns a read-only channel that receives a notification each time
// a job event is appended. The channel is never closed; callers should select
// with a done/context channel when they want to stop listening.
func (s *Service) EventChan() <-chan EventNotification {
	return s.eventChan
}

func (s *Service) Start(ctx context.Context, input CreateJobInput) (*domain.Job, error) {
	job, err := s.prepareJob(input)
	if err != nil {
		return nil, err
	}
	return s.startPreparedJob(ctx, job)
}

// StartAsync creates a job synchronously (so the caller gets the ID immediately)
// and runs the main loop in a background goroutine. Use this when the caller
// cannot block until the job finishes (e.g. an MCP stdio server).
func (s *Service) StartAsync(ctx context.Context, input CreateJobInput) (*domain.Job, error) {
	job, err := s.prepareJob(input)
	if err != nil {
		return nil, err
	}
	if err := s.startPreparedJobAsync(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) StartChain(ctx context.Context, goals []domain.ChainGoal, workspaceDir string) (*domain.JobChain, error) {
	if err := ValidateWorkspaceDir(workspaceDir); err != nil {
		return nil, err
	}
	if len(goals) == 0 {
		return nil, fmt.Errorf("at least one chain goal is required")
	}

	now := time.Now().UTC()
	chain := &domain.JobChain{
		ID:           newChainID(now),
		Goals:        make([]domain.ChainGoal, len(goals)),
		CurrentIndex: 0,
		Status:       domain.ChainStatusRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	for i, goal := range goals {
		chain.Goals[i] = domain.ChainGoal{
			Goal:            strings.TrimSpace(goal.Goal),
			Provider:        goal.Provider,
			StrictnessLevel: normalizeStrictnessLevel(goal.StrictnessLevel),
			ContextMode:     normalizeContextMode(goal.ContextMode),
			MaxSteps:        goal.MaxSteps,
			RoleOverrides:   goal.RoleOverrides,
			Status:          domain.ChainGoalStatusPending,
		}
		if chain.Goals[i].Goal == "" {
			return nil, fmt.Errorf("goals[%d].goal is required", i)
		}
		if chain.Goals[i].Provider == "" {
			chain.Goals[i].Provider = domain.ProviderMock
		}
		if chain.Goals[i].MaxSteps <= 0 {
			chain.Goals[i].MaxSteps = 8
		}
	}

	if err := s.state.SaveChain(ctx, chain); err != nil {
		return nil, err
	}
	if err := s.startChainGoal(ctx, chain, workspaceDir, 0, nil); err != nil {
		return nil, err
	}
	return chain, nil
}

func (s *Service) prepareJob(input CreateJobInput) (*domain.Job, error) {
	now := time.Now().UTC()
	if input.MaxSteps <= 0 {
		input.MaxSteps = 8
	}
	if input.Provider == "" {
		input.Provider = domain.ProviderMock
	}
	roleProfiles := input.RoleProfiles.Normalize(input.Provider)

	job := &domain.Job{
		ID:                   newJobID(now),
		Goal:                 strings.TrimSpace(input.Goal),
		TechStack:            strings.TrimSpace(input.TechStack),
		WorkspaceDir:         firstNonEmpty(strings.TrimSpace(input.WorkspaceDir), s.workspaceRoot),
		Constraints:          input.Constraints,
		DoneCriteria:         input.DoneCriteria,
		StrictnessLevel:      normalizeStrictnessLevel(input.StrictnessLevel),
		ContextMode:          normalizeContextMode(input.ContextMode),
		RoleProfiles:         roleProfiles,
		RoleOverrides:        input.RoleOverrides,
		ChainID:              strings.TrimSpace(input.ChainID),
		ChainGoalIndex:       input.ChainGoalIndex,
		Status:               domain.JobStatusStarting,
		Provider:             input.Provider,
		MaxSteps:             input.MaxSteps,
		CreatedAt:            now,
		UpdatedAt:            now,
		LeaderContextSummary: fmt.Sprintf("Goal: %s", strings.TrimSpace(input.Goal)),
	}
	if err := ValidateWorkspaceDir(job.WorkspaceDir); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) startPreparedJob(ctx context.Context, job *domain.Job) (*domain.Job, error) {
	s.addEvent(job, "job_created", "job created")
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return s.runLoop(ctx, job)
}

func (s *Service) startPreparedJobAsync(ctx context.Context, job *domain.Job) error {
	s.addEvent(job, "job_created", "job created")
	if err := s.state.SaveJob(ctx, job); err != nil {
		return err
	}
	go func() {
		s.runLoop(s.shutdownCtx, job) //nolint:errcheck
	}()
	return nil
}

func (s *Service) startChainGoal(ctx context.Context, chain *domain.JobChain, workspaceDir string, index int, chainCtx *domain.ChainContext) error {
	if index < 0 || index >= len(chain.Goals) {
		return fmt.Errorf("chain goal index out of range: %d", index)
	}
	goal := &chain.Goals[index]
	job, err := s.prepareJob(CreateJobInput{
		Goal:            goal.Goal,
		Provider:        goal.Provider,
		WorkspaceDir:    workspaceDir,
		MaxSteps:        goal.MaxSteps,
		StrictnessLevel: goal.StrictnessLevel,
		ContextMode:     goal.ContextMode,
		RoleProfiles:    domain.DefaultRoleProfiles(goal.Provider),
		RoleOverrides:   goal.RoleOverrides,
		ChainID:         chain.ID,
		ChainGoalIndex:  index,
	})
	if err != nil {
		goal.Status = domain.ChainGoalStatusFailed
		chain.Status = domain.ChainStatusFailed
		s.touchChain(chain)
		_ = s.state.SaveChain(ctx, chain)
		return err
	}

	// Attach previous chain step results so the planner can build on prior work.
	job.ChainContext = chainCtx
	goal.JobID = job.ID
	goal.Status = domain.ChainGoalStatusRunning
	chain.CurrentIndex = index
	chain.Status = domain.ChainStatusRunning
	s.touchChain(chain)
	if err := s.state.SaveChain(ctx, chain); err != nil {
		return err
	}
	if err := s.startPreparedJobAsync(ctx, job); err != nil {
		goal.Status = domain.ChainGoalStatusFailed
		chain.Status = domain.ChainStatusFailed
		s.touchChain(chain)
		_ = s.state.SaveChain(ctx, chain)
		return err
	}
	return nil
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
	if err := s.handleChainTerminalState(ctx, job); err != nil {
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
	if err := s.handleChainTerminalState(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) Get(ctx context.Context, jobID string) (*domain.Job, error) {
	return s.state.LoadJob(ctx, jobID)
}

func (s *Service) GetChain(ctx context.Context, chainID string) (*domain.JobChain, error) {
	return s.state.LoadChain(ctx, chainID)
}

func (s *Service) PauseChain(ctx context.Context, chainID string) (*domain.JobChain, error) {
	chain, err := s.state.LoadChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusCancelled {
		return chain, nil
	}
	chain.Status = domain.ChainStatusPaused
	s.touchChain(chain)
	if err := s.state.SaveChain(ctx, chain); err != nil {
		return nil, err
	}
	return chain, nil
}

func (s *Service) ResumeChain(ctx context.Context, chainID string) (*domain.JobChain, error) {
	chain, err := s.state.LoadChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusCancelled {
		return chain, nil
	}
	chain.Status = domain.ChainStatusRunning
	s.touchChain(chain)
	if err := s.state.SaveChain(ctx, chain); err != nil {
		return nil, err
	}
	if !s.chainCurrentGoalHasCompleted(ctx, chain) {
		return chain, nil
	}
	if err := s.advanceChain(ctx, chain); err != nil {
		return nil, err
	}
	return s.state.LoadChain(ctx, chain.ID)
}

func (s *Service) CancelChain(ctx context.Context, chainID, reason string) (*domain.JobChain, error) {
	chain, err := s.state.LoadChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusCancelled {
		return chain, nil
	}

	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "operator cancelled chain"
	}
	if _, err := s.interruptChainGoalJob(ctx, chain, fmt.Sprintf("cancelled by chain operator: %s", reason)); err != nil {
		return nil, err
	}
	if chain.CurrentIndex >= 0 && chain.CurrentIndex < len(chain.Goals) {
		current := &chain.Goals[chain.CurrentIndex]
		if current.Status == domain.ChainGoalStatusPending || current.Status == domain.ChainGoalStatusRunning {
			current.Status = domain.ChainGoalStatusFailed
		}
	}
	chain.Status = domain.ChainStatusCancelled
	s.touchChain(chain)
	if err := s.state.SaveChain(ctx, chain); err != nil {
		return nil, err
	}
	return chain, nil
}

func (s *Service) SkipChainGoal(ctx context.Context, chainID string) (*domain.JobChain, error) {
	chain, err := s.state.LoadChain(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusCancelled {
		return chain, nil
	}
	if chain.CurrentIndex < 0 || chain.CurrentIndex >= len(chain.Goals) {
		return nil, fmt.Errorf("chain current index out of range: %d", chain.CurrentIndex)
	}

	workdir, err := s.interruptChainGoalJob(ctx, chain, "chain goal skipped by operator")
	if err != nil {
		return nil, err
	}

	current := &chain.Goals[chain.CurrentIndex]
	current.Status = domain.ChainGoalStatusSkipped
	if chain.CurrentIndex == len(chain.Goals)-1 {
		chain.Status = domain.ChainStatusDone
		s.touchChain(chain)
		if err := s.state.SaveChain(ctx, chain); err != nil {
			return nil, err
		}
		return chain, nil
	}

	chain.Status = domain.ChainStatusRunning
	s.touchChain(chain)
	if err := s.state.SaveChain(ctx, chain); err != nil {
		return nil, err
	}
	if err := s.startChainGoal(ctx, chain, workdir, chain.CurrentIndex+1, nil); err != nil {
		return nil, err
	}
	return s.state.LoadChain(ctx, chain.ID)
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

func (s *Service) ListChains(ctx context.Context) ([]*domain.JobChain, error) {
	chains, err := s.state.ListChains(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.JobChain, 0, len(chains))
	for i := range chains {
		chain := chains[i]
		out = append(out, &chain)
	}
	return out, nil
}

func (s *Service) advanceChain(ctx context.Context, chain *domain.JobChain) error {
	if chain == nil || chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusPaused || chain.Status == domain.ChainStatusCancelled {
		return nil
	}
	if chain.CurrentIndex < 0 || chain.CurrentIndex >= len(chain.Goals) {
		return fmt.Errorf("chain current index out of range: %d", chain.CurrentIndex)
	}

	current := &chain.Goals[chain.CurrentIndex]
	if current.JobID == "" {
		return fmt.Errorf("chain goal %d has no job id", chain.CurrentIndex)
	}
	job, err := s.state.LoadJob(ctx, current.JobID)
	if err != nil {
		return err
	}

	switch job.Status {
	case domain.JobStatusDone:
		current.Status = domain.ChainGoalStatusDone
		if chain.CurrentIndex == len(chain.Goals)-1 {
			chain.Status = domain.ChainStatusDone
			s.touchChain(chain)
			return s.state.SaveChain(ctx, chain)
		}
		s.touchChain(chain)
		if err := s.state.SaveChain(ctx, chain); err != nil {
			return err
		}
		latest, err := s.state.LoadChain(ctx, chain.ID)
		if err != nil {
			return err
		}
		if latest.Status == domain.ChainStatusPaused || latest.Status == domain.ChainStatusCancelled || latest.Status == domain.ChainStatusFailed || latest.Status == domain.ChainStatusDone {
			return nil
		}
		// Pass the completed job's summary and evaluator report ref so the next
		// goal's planner can build on what the previous chain step accomplished.
		var prevCtx *domain.ChainContext
		if job.Summary != "" || job.EvaluatorReportRef != "" {
			prevCtx = &domain.ChainContext{
				Summary:            job.Summary,
				EvaluatorReportRef: job.EvaluatorReportRef,
			}
		}
		return s.startChainGoal(ctx, latest, job.WorkspaceDir, latest.CurrentIndex+1, prevCtx)
	case domain.JobStatusBlocked, domain.JobStatusFailed:
		current.Status = domain.ChainGoalStatusFailed
		chain.Status = domain.ChainStatusFailed
		s.touchChain(chain)
		return s.state.SaveChain(ctx, chain)
	default:
		return nil
	}
}

func (s *Service) handleChainCompletion(ctx context.Context, job *domain.Job) error {
	if strings.TrimSpace(job.ChainID) == "" {
		return nil
	}
	chain, err := s.state.LoadChain(ctx, job.ChainID)
	if err != nil {
		return err
	}
	if chain.Status == domain.ChainStatusCancelled {
		return nil
	}
	if chain.Status == domain.ChainStatusPaused {
		if job.ChainGoalIndex >= 0 && job.ChainGoalIndex < len(chain.Goals) {
			chain.Goals[job.ChainGoalIndex].Status = domain.ChainGoalStatusDone
			s.touchChain(chain)
			return s.state.SaveChain(ctx, chain)
		}
		return nil
	}
	return s.advanceChain(ctx, chain)
}

func (s *Service) handleChainTerminalState(ctx context.Context, job *domain.Job) error {
	if strings.TrimSpace(job.ChainID) == "" {
		return nil
	}
	chain, err := s.state.LoadChain(ctx, job.ChainID)
	if err != nil {
		return err
	}
	if chain.Status == domain.ChainStatusFailed || chain.Status == domain.ChainStatusDone || chain.Status == domain.ChainStatusCancelled {
		return nil
	}
	if job.ChainGoalIndex >= 0 && job.ChainGoalIndex < len(chain.Goals) {
		chain.Goals[job.ChainGoalIndex].Status = domain.ChainGoalStatusFailed
	}
	chain.Status = domain.ChainStatusFailed
	s.touchChain(chain)
	return s.state.SaveChain(ctx, chain)
}

func (s *Service) chainCurrentGoalHasCompleted(ctx context.Context, chain *domain.JobChain) bool {
	if chain == nil || chain.CurrentIndex < 0 || chain.CurrentIndex >= len(chain.Goals) {
		return false
	}
	current := chain.Goals[chain.CurrentIndex]
	if current.Status == domain.ChainGoalStatusDone {
		return true
	}
	if strings.TrimSpace(current.JobID) == "" {
		return false
	}
	job, err := s.state.LoadJob(ctx, current.JobID)
	if err != nil {
		return false
	}
	return job.Status == domain.JobStatusDone
}

func (s *Service) interruptChainGoalJob(ctx context.Context, chain *domain.JobChain, reason string) (string, error) {
	if chain == nil || chain.CurrentIndex < 0 || chain.CurrentIndex >= len(chain.Goals) {
		return s.workspaceRoot, nil
	}
	current := chain.Goals[chain.CurrentIndex]
	if strings.TrimSpace(current.JobID) == "" {
		return s.workspaceRoot, nil
	}
	job, err := s.state.LoadJob(ctx, current.JobID)
	if err != nil {
		return "", err
	}
	if job.Status == domain.JobStatusDone || job.Status == domain.JobStatusFailed || job.Status == domain.JobStatusBlocked {
		return firstNonEmpty(job.WorkspaceDir, s.workspaceRoot), nil
	}

	job.Status = domain.JobStatusBlocked
	job.BlockedReason = reason
	job.FailureReason = ""
	job.PendingApproval = nil
	job.LeaderContextSummary = reason
	s.addEvent(job, "job_cancelled", reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return "", err
	}
	return firstNonEmpty(job.WorkspaceDir, s.workspaceRoot), nil
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
	consecutiveSummarizes := 0
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

		rawLeader, action, err := s.executeProviderPhase(ctx, job, "leader", func() (string, error) {
			return s.sessions.RunLeader(ctx, *job)
		})
		if err != nil {
			if action == provider.ProviderErrorActionBlock {
				return s.blockJobWithEvent(ctx, job, "job_blocked", fmt.Sprintf("leader execution blocked: %v", err))
			}
			return s.failJob(ctx, job, fmt.Sprintf("leader execution failed: %v", err))
		}
		s.accumulateTokenUsage(job, job.CurrentStep, estimateProviderUsage(rawLeader, *job))
		job.SupervisorDirective = ""

		var leader domain.LeaderOutput
		if err := json.Unmarshal([]byte(rawLeader), &leader); err != nil {
			return s.failJob(ctx, job, fmt.Sprintf("invalid leader json: %v", err))
		}
		if err := schema.ValidateLeaderOutput(leader); err != nil {
			return s.failJob(ctx, job, fmt.Sprintf("leader schema validation failed: %v", err))
		}

		if leader.Action == "summarize" {
			if consecutiveSummarizes >= 2 {
				s.addEvent(job, "leader_summarize_capped", "leader summarize capped; forcing completion evaluation")
				leader.Action = "complete"
				consecutiveSummarizes = 0
			} else {
				consecutiveSummarizes++
			}
		} else {
			consecutiveSummarizes = 0
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
			if completionRetryPending {
				job.BlockedReason = ""
				completionRetryPending = false
			}
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
				s.touch(job)
				if err := s.state.SaveJob(ctx, job); err != nil {
					return nil, err
				}
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
			if err := s.handleChainCompletion(ctx, job); err != nil {
				return nil, err
			}
			return job, nil
		case "fail":
			return s.failJob(ctx, job, leader.Reason)
		case "blocked":
			return s.blockJobWithEvent(ctx, job, "job_blocked", leader.Reason)
		default:
			return s.failJob(ctx, job, fmt.Sprintf("unrecognized leader action: %q", leader.Action))
		}
	}

	return s.blockJobWithEvent(ctx, job, "job_blocked", "max_steps_exceeded")
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

	rawWorker, action, err := s.executeProviderPhase(ctx, job, "worker", func() (string, error) {
		return s.sessions.RunWorker(ctx, *job, task)
	})
	if err != nil {
		last := &job.Steps[len(job.Steps)-1]
		reason := fmt.Sprintf("worker execution failed: %v", err)
		if action == provider.ProviderErrorActionBlock {
			reason = fmt.Sprintf("worker execution blocked: %v", err)
		}
		structuredReason := classifyWorkerFailure(err, "")
		last.FinishedAt = time.Now().UTC()
		last.StructuredReason = structuredReason
		if action == provider.ProviderErrorActionBlock {
			last.Status = domain.StepStatusBlocked
			last.BlockedReason = reason
			last.Summary = last.BlockedReason
			s.addEvent(job, "worker_blocked", formatWorkerFailureEventMessage(reason, structuredReason))
			_, blockErr := s.blockJob(ctx, job, reason)
			return blockErr
		}
		last.Status = domain.StepStatusFailed
		last.ErrorReason = reason
		last.Summary = last.ErrorReason
		s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, structuredReason))
		_, failErr := s.failJob(ctx, job, last.ErrorReason)
		return failErr
	}
	s.accumulateTokenUsage(job, step.Index, estimateProviderUsage(rawWorker, *job, task))

	var worker domain.WorkerOutput
	if err := json.Unmarshal([]byte(rawWorker), &worker); err != nil {
		last := &job.Steps[len(job.Steps)-1]
		reason := fmt.Sprintf("invalid worker json: %v", err)
		structuredReason := classifyWorkerFailure(err, rawWorker)
		last.Status = domain.StepStatusFailed
		last.ErrorReason = reason
		last.StructuredReason = structuredReason
		last.Summary = reason
		last.FinishedAt = time.Now().UTC()
		s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, structuredReason))
		_, failErr := s.failJob(ctx, job, reason)
		return failErr
	}
	if err := schema.ValidateWorkerOutput(worker); err != nil {
		last := &job.Steps[len(job.Steps)-1]
		reason := fmt.Sprintf("worker schema validation failed: %v", err)
		structuredReason := classifyWorkerFailure(err, rawWorker)
		last.Status = domain.StepStatusFailed
		last.ErrorReason = reason
		last.StructuredReason = structuredReason
		last.Summary = reason
		last.FinishedAt = time.Now().UTC()
		s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, structuredReason))
		_, failErr := s.failJob(ctx, job, reason)
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
	last.StructuredReason = nil
	last.FinishedAt = time.Now().UTC()

	switch worker.Status {
	case "success":
		last.DiffSummary = collectWorkspaceDiffSummary(ctx, firstNonEmpty(job.WorkspaceDir, s.workspaceRoot))
		last.Status = domain.StepStatusSucceeded
		job.Status = domain.JobStatusRunning
		s.addEvent(job, "worker_succeeded", worker.Summary)
	case "blocked":
		reason := firstNonEmpty(worker.BlockedReason, worker.Summary, "worker blocked")
		last.StructuredReason = classifyWorkerFailure(nil, strings.Join([]string{worker.BlockedReason, worker.Summary, rawWorker}, "\n"))
		last.Status = domain.StepStatusBlocked
		last.BlockedReason = reason
		s.addEvent(job, "worker_blocked", formatWorkerFailureEventMessage(reason, last.StructuredReason))
		_, blockErr := s.blockJob(ctx, job, reason)
		return blockErr
	case "failed":
		reason := firstNonEmpty(worker.ErrorReason, worker.Summary, "worker failed")
		last.StructuredReason = classifyWorkerFailure(nil, strings.Join([]string{worker.ErrorReason, worker.Summary, rawWorker}, "\n"))
		last.Status = domain.StepStatusFailed
		last.ErrorReason = reason
		job.Status = domain.JobStatusRunning
		job.FailureReason = reason
		s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, last.StructuredReason))
	}

	job.LeaderContextSummary = worker.Summary
	job.LeaderContextSummary = sanitizeLeaderContext(job.LeaderContextSummary)
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
		_, blockErr := s.blockJobWithEvent(ctx, job, "system_blocked", decision.Reason)
		return blockErr
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
		_, blockErr := s.blockJobWithEvent(ctx, job, "system_blocked", runErr.Error())
		return blockErr
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
	job.LeaderContextSummary = sanitizeLeaderContext(job.LeaderContextSummary)
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
	if err := s.handleChainTerminalState(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) blockJob(ctx context.Context, job *domain.Job, reason string) (*domain.Job, error) {
	return s.blockJobWithEvent(ctx, job, "job_blocked", reason)
}

func (s *Service) blockJobWithEvent(ctx context.Context, job *domain.Job, eventKind, reason string) (*domain.Job, error) {
	if blockedReasonStrikeCount(job, reason) >= 3 {
		job.PendingApproval = nil
		job.BlockedReason = ""
		return s.failJob(ctx, job, fmt.Sprintf("blocked reason repeated 3 times: %s", reason))
	}
	job.Status = domain.JobStatusBlocked
	job.BlockedReason = reason
	job.FailureReason = ""
	job.LeaderContextSummary = sanitizeLeaderContext(reason)
	s.addEvent(job, eventKind, reason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	if err := s.handleChainTerminalState(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Service) executeProviderPhase(ctx context.Context, job *domain.Job, phase string, invoke func() (string, error)) (string, provider.ProviderErrorAction, error) {
	for attempt := 0; ; attempt++ {
		raw, err := invoke()
		if err == nil {
			return raw, "", nil
		}

		action := providerActionForError(err)
		if action == provider.ProviderErrorActionRetry && attempt < providerRetryLimit {
			delay := providerRetryBackoff(attempt)
			s.addEvent(job, "provider_retry", fmt.Sprintf("%s retry %d/%d after %s: %v", phase, attempt+1, providerRetryLimit, delay, err))
			if err := waitForProviderRetry(ctx, delay); err != nil {
				return "", provider.ProviderErrorActionFail, fmt.Errorf("%s retry interrupted: %w", phase, err)
			}
			continue
		}
		if action == provider.ProviderErrorActionRetry {
			return "", provider.ProviderErrorActionFail, fmt.Errorf("%s execution failed after %d retries: %w", phase, providerRetryLimit, err)
		}
		return "", action, err
	}
}

func estimateTokens(input, output string) domain.TokenUsage {
	inputTokens := estimateTokenCount(input)
	outputTokens := estimateTokenCount(output)
	totalTokens := inputTokens + outputTokens
	return domain.TokenUsage{
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		TotalTokens:      totalTokens,
		EstimatedCostUSD: float64(totalTokens) * roughCostPerTokenUSD,
	}
}

func estimateProviderUsage(output string, inputs ...any) domain.TokenUsage {
	return estimateTokens(buildTokenUsageInput(inputs...), output)
}

func buildTokenUsageInput(inputs ...any) string {
	if len(inputs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, input := range inputs {
		if i > 0 {
			b.WriteString("\n")
		}
		switch value := input.(type) {
		case string:
			b.WriteString(value)
		default:
			raw, err := json.Marshal(value)
			if err != nil {
				b.WriteString(fmt.Sprintf("%v", value))
				continue
			}
			b.Write(raw)
		}
	}
	return b.String()
}

func estimateTokenCount(text string) int {
	charCount := utf8.RuneCountInString(text)
	if charCount == 0 {
		return 0
	}
	return (charCount + 3) / 4
}

func (s *Service) accumulateTokenUsage(job *domain.Job, stepIndex int, usage domain.TokenUsage) {
	job.TokenUsage = mergeTokenUsage(job.TokenUsage, usage)
	if step := stepByIndex(job, stepIndex); step != nil {
		step.TokenUsage = mergeTokenUsage(step.TokenUsage, usage)
	}
}

func mergeTokenUsage(current, delta domain.TokenUsage) domain.TokenUsage {
	current.InputTokens += delta.InputTokens
	current.OutputTokens += delta.OutputTokens
	current.TotalTokens += delta.TotalTokens
	current.EstimatedCostUSD += delta.EstimatedCostUSD
	return current
}

// Steer injects a supervisor directive into the job's leader context.
// The next leader call will see this directive with highest priority.
func (s *Service) Steer(ctx context.Context, jobID string, message string) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	switch job.Status {
	case domain.JobStatusRunning, domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
	default:
		return job, fmt.Errorf("cannot steer job with status %s", job.Status)
	}
	job.SupervisorDirective = "[SUPERVISOR] " + strings.TrimSpace(message)
	s.addEvent(job, "supervisor_steer", message)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

func stepByIndex(job *domain.Job, stepIndex int) *domain.Step {
	if stepIndex <= 0 {
		return nil
	}
	for i := range job.Steps {
		if job.Steps[i].Index == stepIndex {
			return &job.Steps[i]
		}
	}
	return nil
}

func providerActionForError(err error) provider.ProviderErrorAction {
	var perr *provider.ProviderError
	if errors.As(err, &perr) && perr.RecommendedAction != "" {
		return perr.RecommendedAction
	}
	return provider.ProviderErrorActionFail
}

func providerRetryBackoff(attempt int) time.Duration {
	return providerRetryBaseDelay * time.Duration(1<<attempt)
}

func waitForProviderRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func blockedReasonStrikeCount(job *domain.Job, reason string) int {
	count := 1
	for i := len(job.Events) - 1; i >= 0; i-- {
		event := job.Events[i]
		if !strings.HasSuffix(event.Kind, "_blocked") {
			continue
		}
		if blockedEventReason(event.Message) != reason {
			break
		}
		count++
	}
	return count
}

func sanitizeLeaderContext(text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(line, "[SUPERVISOR]") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

type workerFailureEventMessage struct {
	Reason           string                   `json:"reason"`
	StructuredReason *domain.StructuredReason `json:"structured_reason"`
}

func classifyWorkerFailure(err error, workerOutput string) *domain.StructuredReason {
	detail := strings.TrimSpace(workerOutput)
	if err != nil {
		detail = strings.TrimSpace(err.Error())
	}
	combined := strings.ToLower(strings.TrimSpace(strings.Join([]string{
		detail,
		workerOutput,
	}, "\n")))

	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(combined, context.DeadlineExceeded.Error()):
		return &domain.StructuredReason{
			Category:        "timeout",
			Detail:          firstNonEmpty(detail, "worker execution timed out"),
			SuggestedAction: "increase_timeout",
		}
	case isJSONUnmarshalError(err) ||
		strings.Contains(combined, "json unmarshal") ||
		strings.Contains(combined, "schema validation") ||
		strings.Contains(combined, "is required") ||
		strings.Contains(combined, "invalid") ||
		strings.Contains(combined, "validation failed"):
		return &domain.StructuredReason{
			Category:        "schema_violation",
			Detail:          firstNonEmpty(detail, "worker output schema violation"),
			SuggestedAction: "retry",
		}
	case strings.Contains(combined, "permission") || strings.Contains(combined, "access"):
		return &domain.StructuredReason{
			Category:        "file_access",
			Detail:          firstNonEmpty(detail, "worker file access failure"),
			SuggestedAction: "check_permissions",
		}
	case strings.Contains(combined, "test"):
		return &domain.StructuredReason{
			Category:        "test_failure",
			Detail:          firstNonEmpty(detail, "worker test failure"),
			SuggestedAction: "fix_code",
		}
	case strings.Contains(combined, "build") || strings.Contains(combined, "compile"):
		return &domain.StructuredReason{
			Category:        "build_failure",
			Detail:          firstNonEmpty(detail, "worker build failure"),
			SuggestedAction: "fix_code",
		}
	default:
		return nil
	}
}

func formatWorkerFailureEventMessage(reason string, structured *domain.StructuredReason) string {
	payload := workerFailureEventMessage{
		Reason:           reason,
		StructuredReason: structured,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprintf(`{"reason":%q,"structured_reason":null}`, reason)
	}
	return string(raw)
}

func blockedEventReason(message string) string {
	var payload workerFailureEventMessage
	if err := json.Unmarshal([]byte(message), &payload); err == nil && strings.TrimSpace(payload.Reason) != "" {
		return payload.Reason
	}
	return message
}

func isJSONUnmarshalError(err error) bool {
	if err == nil {
		return false
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
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

func (s *Service) touchChain(chain *domain.JobChain) {
	chain.UpdatedAt = time.Now().UTC()
}

func newJobID(now time.Time) string {
	return fmt.Sprintf("job-%s", now.Format("20060102-150405.000"))
}

func newChainID(now time.Time) string {
	return fmt.Sprintf("chain-%s", now.Format("20060102-150405.000"))
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

func collectWorkspaceDiffSummary(ctx context.Context, workspaceDir string) string {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" {
		return ""
	}

	cmd := exec.CommandContext(ctx, "git", "-C", workspaceDir, "diff", "--stat")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}

	return strings.TrimSpace(stdout.String())
}
