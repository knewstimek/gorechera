package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
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
var errServiceShuttingDown = errors.New("service is shutting down")

const (
	providerRetryLimit     = 3
	providerRetryBaseDelay = 250 * time.Millisecond
	recoveryConcurrency    = 2
	maxResumeExtraSteps    = 20
	// schemaRetryMax is the maximum number of additional provider calls made
	// when JSON parsing or schema validation fails. The total attempt count
	// is schemaRetryMax+1 (initial call + up to schemaRetryMax retries).
	schemaRetryMax = 2
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
	Goal             string
	TechStack        string
	WorkspaceDir     string
	WorkspaceMode    string
	Constraints      []string
	DoneCriteria     []string
	Provider         domain.ProviderName
	RoleProfiles     domain.RoleProfiles
	RoleOverrides    map[string]domain.RoleOverride
	MaxSteps         int
	PipelineMode     string
	StrictnessLevel  string            // strict | normal | lenient; empty defaults to "normal"
	AmbitionLevel    string            // low | medium | high | custom; empty or unrecognized defaults to "medium"
	AmbitionText     string            // custom text; replaces default when level=custom, prepended otherwise
	ContextMode      string            // full | summary | minimal; empty defaults to "full"
	PreBuildCommands []string          // run before engine build/test (best-effort)
	EngineBuildCmd   string            // overrides default "go build ./..."; empty = default
	EngineTestCmd    string            // overrides default "go test ./..."; empty = default
	PromptOverrides  map[string]string // per-role prompt fragments prepended at call time
	ChainID          string
	ChainGoalIndex   int
}

type ResumeOptions struct {
	ExtraSteps int
}

type Service struct {
	sessions      *provider.SessionManager
	state         *store.StateStore
	artifacts     *store.ArtifactStore
	approval      policy.Policy
	runtime       *runtimeexec.Runner
	processes     *runtimeexec.ProcessManager
	workspaceRoot string
	instanceID    string

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

	// jobCache holds in-memory snapshots of running jobs so that Get() and
	// List() reflect real-time progress without a disk round-trip. The runLoop
	// goroutine is the sole writer for a given job ID; readers get a deep clone
	// via cacheGet so no mutable pointer escapes.
	cacheMu  sync.RWMutex
	jobCache map[string]*domain.Job

	runMu       sync.Mutex
	runningJobs map[string]struct{}
	bgMu        sync.Mutex
	bgClosed    bool
	bgWG        sync.WaitGroup
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
		instanceID:    newServiceInstanceID(),
		// Buffer 100 notifications so that burst events during a fast-running job
		// do not stall the orchestrator goroutine even if the MCP listener lags.
		eventChan:       make(chan EventNotification, 100),
		harnesses:       make(map[int]runtimeexec.ProcessHandle),
		harnessOwners:   make(map[int]string),
		jobHarnesses:    make(map[string]map[int]struct{}),
		harnessInflight: make(map[int]string),
		jobCache:        make(map[string]*domain.Job),
		shutdownCtx:     shutdownCtx,
		shutdownCancel:  shutdownCancel,
		runningJobs:     make(map[string]struct{}),
	}
}

// Shutdown cancels the service-level context, signals background job
// goroutines to stop, and waits for them to exit. It is safe to call multiple
// times.
func (s *Service) Shutdown() {
	s.bgMu.Lock()
	s.bgClosed = true
	s.shutdownCancel()
	s.bgMu.Unlock()
	s.bgWG.Wait()
	s.InterruptOwnedJobs()
}

// InterruptRecoverableJobs marks persisted non-terminal jobs as blocked with
// an interruption reason instead of leaving them stranded in waiting_* states.
// Jobs listed in skipJobIDs are left untouched so callers can explicitly
// recover them instead.
func (s *Service) InterruptRecoverableJobs(skipJobIDs []string) {
	ctx := context.Background()
	jobs, err := s.state.ListJobs(ctx)
	if err != nil {
		log.Printf("[gorchera] interrupt sweep: failed to list jobs: %v", err)
		return
	}
	skip := make(map[string]struct{}, len(skipJobIDs))
	for _, jobID := range skipJobIDs {
		jobID = strings.TrimSpace(jobID)
		if jobID != "" {
			skip[jobID] = struct{}{}
		}
	}

	interrupted := 0
	now := time.Now().UTC()
	for i := range jobs {
		job := jobs[i]
		if !isRecoverableJobStatus(job.Status) {
			continue
		}
		if _, ok := skip[job.ID]; ok {
			continue
		}
		if !s.isStaleJob(job, now) {
			continue
		}
		if err := s.interruptJob(ctx, &job, startupInterruptionReason(job)); err != nil {
			log.Printf("[gorchera] interrupt sweep: failed to block job %s: %v", job.ID, err)
			continue
		}
		interrupted++
	}
	if interrupted > 0 {
		log.Printf("[gorchera] interrupt sweep: blocked %d recoverable jobs", interrupted)
	}
}

// InterruptOwnedJobs marks this service instance's recoverable jobs as blocked.
// It is intended for graceful shutdown so stale jobs do not linger.
func (s *Service) InterruptOwnedJobs() {
	ctx := context.Background()
	jobs, err := s.state.ListJobs(ctx)
	if err != nil {
		log.Printf("[gorchera] shutdown interrupt: failed to list jobs: %v", err)
		return
	}

	for i := range jobs {
		job := jobs[i]
		if !isRecoverableJobStatus(job.Status) || job.RunOwnerID != s.instanceID {
			continue
		}
		if err := s.interruptJob(ctx, &job, "orchestrator shutdown interrupted the active job"); err != nil {
			log.Printf("[gorchera] shutdown interrupt: failed to block job %s: %v", job.ID, err)
		}
	}
}

// RecoverJobs resumes any jobs that were in a non-terminal state when the
// server last stopped. Without this, jobs created just before an MCP restart
// would be stuck in "starting" or "waiting_*" forever because the goroutine
// that drives runLoop was lost. Call this once after NewService.
func (s *Service) RecoverJobs() {
	s.recoverJobs(nil)
}

func (s *Service) RecoverSelectedJobs(jobIDs []string) {
	s.recoverJobs(jobIDs)
}

func (s *Service) recoverJobs(jobIDs []string) {
	ctx := context.Background()
	jobs, err := s.state.ListJobs(ctx)
	if err != nil {
		log.Printf("[gorchera] recovery: failed to list jobs: %v", err)
		return
	}
	filter := make(map[string]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		jobID = strings.TrimSpace(jobID)
		if jobID != "" {
			filter[jobID] = struct{}{}
		}
	}
	recoverable := make([]domain.Job, 0, len(jobs))
	for i := range jobs {
		job := jobs[i]
		if len(filter) > 0 {
			if _, ok := filter[job.ID]; !ok {
				continue
			}
		}
		switch job.Status {
		case domain.JobStatusStarting, domain.JobStatusPlanning, domain.JobStatusRunning,
			domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
			recoverable = append(recoverable, job)
		}
	}
	if len(recoverable) == 0 {
		return
	}

	sort.Slice(recoverable, func(i, j int) bool {
		return recoverable[i].UpdatedAt.Before(recoverable[j].UpdatedAt)
	})

	if !s.startBackgroundTask(func() {
		sem := make(chan struct{}, recoveryConcurrency)
		for i := range recoverable {
			job := recoverable[i]
			log.Printf("[gorchera] recovery: scheduling job %s (status=%s)", job.ID, job.Status)
			sem <- struct{}{}
			j := job
			if !s.startBackgroundTask(func() {
				defer func() { <-sem }()
				if _, err := s.runLoop(s.shutdownCtx, &j); err != nil {
					log.Printf("[gorchera] recovery: job %s failed: %v", j.ID, err)
				}
			}) {
				<-sem
				return
			}
		}
	}) {
		return
	}

	if len(filter) > 0 {
		log.Printf("[gorchera] recovery: scheduled %d selected jobs with max concurrency %d", len(recoverable), recoveryConcurrency)
		return
	}
	log.Printf("[gorchera] recovery: scheduled %d jobs with max concurrency %d", len(recoverable), recoveryConcurrency)
}

func (s *Service) claimJobRun(jobID string) bool {
	if strings.TrimSpace(jobID) == "" {
		return true
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if _, exists := s.runningJobs[jobID]; exists {
		return false
	}
	s.runningJobs[jobID] = struct{}{}
	return true
}

func (s *Service) releaseJobRun(jobID string) {
	if strings.TrimSpace(jobID) == "" {
		return
	}
	s.runMu.Lock()
	delete(s.runningJobs, jobID)
	s.runMu.Unlock()
}

func (s *Service) latestJobSnapshot(job *domain.Job) *domain.Job {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return job
	}
	if cached := s.cacheGet(job.ID); cached != nil {
		return cached
	}
	latest, err := s.state.LoadJob(context.Background(), job.ID)
	if err != nil {
		return job
	}
	return latest
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
			PipelineMode:    domain.NormalizePipelineMode(goal.PipelineMode),
			StrictnessLevel: normalizeStrictnessLevel(goal.StrictnessLevel),
			AmbitionLevel:   domain.NormalizeAmbitionLevel(goal.AmbitionLevel),
			ContextMode:     normalizeContextMode(goal.ContextMode),
			MaxSteps:        goal.MaxSteps,
			RoleOverrides:   canonicalizeRoleOverrides(goal.RoleOverrides),
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
	jobID := newJobID(now)
	if input.MaxSteps <= 0 {
		input.MaxSteps = 8
	}
	if input.Provider == "" {
		input.Provider = domain.ProviderMock
	}
	roleProfiles := input.RoleProfiles.Normalize(input.Provider)
	workspaceDir, requestedWorkspaceDir, workspaceMode, err := prepareWorkspaceDir(s.workspaceRoot, input.WorkspaceDir, jobID, input.WorkspaceMode)
	if err != nil {
		return nil, err
	}

	job := &domain.Job{
		ID:                    jobID,
		Goal:                  strings.TrimSpace(input.Goal),
		TechStack:             strings.TrimSpace(input.TechStack),
		WorkspaceDir:          workspaceDir,
		RequestedWorkspaceDir: requestedWorkspaceDir,
		WorkspaceMode:         workspaceMode,
		Constraints:           input.Constraints,
		DoneCriteria:          input.DoneCriteria,
		PipelineMode:          domain.NormalizePipelineMode(input.PipelineMode),
		StrictnessLevel:       normalizeStrictnessLevel(input.StrictnessLevel),
		AmbitionLevel:         domain.NormalizeAmbitionLevel(input.AmbitionLevel),
		AmbitionText:          strings.TrimSpace(input.AmbitionText),
		ContextMode:           normalizeContextMode(input.ContextMode),
		RoleProfiles:          roleProfiles,
		RoleOverrides:         canonicalizeRoleOverrides(input.RoleOverrides),
		PreBuildCommands:      input.PreBuildCommands,
		EngineBuildCmd:        strings.TrimSpace(input.EngineBuildCmd),
		EngineTestCmd:         strings.TrimSpace(input.EngineTestCmd),
		PromptOverrides:       canonicalizePromptOverrides(input.PromptOverrides),
		ChainID:               strings.TrimSpace(input.ChainID),
		ChainGoalIndex:        input.ChainGoalIndex,
		Status:                domain.JobStatusStarting,
		Provider:              input.Provider,
		MaxSteps:              input.MaxSteps,
		CreatedAt:             now,
		UpdatedAt:             now,
		LeaderContextSummary:  fmt.Sprintf("Goal: %s", strings.TrimSpace(input.Goal)),
	}
	if err := ValidateWorkspaceDir(job.WorkspaceDir); err != nil {
		return nil, err
	}
	return job, nil
}

func canonicalizeRoleOverrides(overrides map[string]domain.RoleOverride) map[string]domain.RoleOverride {
	if len(overrides) == 0 {
		return nil
	}
	canonical := make(map[string]domain.RoleOverride, len(overrides)+2)
	for key, value := range overrides {
		trimmed := strings.ToLower(strings.TrimSpace(key))
		if trimmed == "" {
			continue
		}
		value.Provider = domain.ProviderName(strings.TrimSpace(string(value.Provider)))
		value.Model = strings.TrimSpace(value.Model)
		if value.Provider == "" && value.Model == "" {
			continue
		}
		canonical[trimmed] = value
	}
	if director, ok := canonical[string(domain.RoleDirector)]; ok {
		if _, exists := canonical[string(domain.RolePlanner)]; !exists {
			canonical[string(domain.RolePlanner)] = director
		}
		if _, exists := canonical[string(domain.RoleLeader)]; !exists {
			canonical[string(domain.RoleLeader)] = director
		}
	}
	return canonical
}

// canonicalizePromptOverrides normalizes keys to lowercase and drops entries
// where the value is blank, returning nil when the result is empty.
func canonicalizePromptOverrides(overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(overrides))
	for k, v := range overrides {
		key := strings.ToLower(strings.TrimSpace(k))
		val := strings.TrimSpace(v)
		if key != "" && val != "" {
			out[key] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Service) applyResumeExtraSteps(ctx context.Context, job *domain.Job, extraSteps int) error {
	if extraSteps <= 0 {
		return nil
	}
	if job.Status != domain.JobStatusBlocked || job.BlockedReason != "max_steps_exceeded" {
		return fmt.Errorf("extra_steps is only valid when resuming a max_steps_exceeded blocked job")
	}
	remaining := maxResumeExtraSteps - job.ResumeExtraStepsUsed
	if remaining <= 0 {
		return fmt.Errorf("resume extra step budget exhausted")
	}
	if extraSteps > remaining {
		return fmt.Errorf("extra_steps exceeds remaining budget: requested=%d remaining=%d", extraSteps, remaining)
	}
	job.MaxSteps += extraSteps
	job.ResumeExtraStepsUsed += extraSteps
	job.BlockedReason = ""
	job.FailureReason = ""
	s.addEvent(job, "job_resume_extra_steps", fmt.Sprintf("extended max steps by %d to %d", extraSteps, job.MaxSteps))
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) startPreparedJob(ctx context.Context, job *domain.Job) (*domain.Job, error) {
	s.addEvent(job, "job_created", "job created")
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	return s.runLoop(ctx, job)
}

func (s *Service) startPreparedJobAsync(ctx context.Context, job *domain.Job) error {
	if !s.reserveBackgroundTask() {
		return errServiceShuttingDown
	}
	s.addEvent(job, "job_created", "job created")
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		s.bgWG.Done()
		return err
	}
	go func() {
		defer s.bgWG.Done()
		if _, err := s.runLoop(s.shutdownCtx, job); err != nil {
			log.Printf("[gorchera] async job %s failed: %v", job.ID, err)
		}
	}()
	return nil
}

func (s *Service) reserveBackgroundTask() bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	if s.bgClosed {
		return false
	}
	s.bgWG.Add(1)
	return true
}

func (s *Service) startBackgroundTask(fn func()) bool {
	if !s.reserveBackgroundTask() {
		return false
	}
	go func() {
		defer s.bgWG.Done()
		fn()
	}()
	return true
}

func (s *Service) startChainGoal(ctx context.Context, chain *domain.JobChain, workspaceDir string, index int, chainCtx *domain.ChainContext) error {
	if index < 0 || index >= len(chain.Goals) {
		return fmt.Errorf("chain goal index out of range: %d", index)
	}
	goal := &chain.Goals[index]
	job, err := s.prepareJob(CreateJobInput{
		Goal:             goal.Goal,
		Provider:         goal.Provider,
		WorkspaceDir:     workspaceDir,
		MaxSteps:         goal.MaxSteps,
		PipelineMode:     goal.PipelineMode,
		StrictnessLevel:  goal.StrictnessLevel,
		AmbitionLevel:    goal.AmbitionLevel,
		AmbitionText:     goal.AmbitionText,
		ContextMode:      goal.ContextMode,
		RoleProfiles:     domain.DefaultRoleProfiles(goal.Provider),
		RoleOverrides:    goal.RoleOverrides,
		PreBuildCommands: goal.PreBuildCommands,
		EngineBuildCmd:   goal.EngineBuildCmd,
		EngineTestCmd:    goal.EngineTestCmd,
		ChainID:          chain.ID,
		ChainGoalIndex:   index,
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
	return s.ResumeWithOptions(ctx, jobID, ResumeOptions{})
}

func (s *Service) ResumeWithOptions(ctx context.Context, jobID string, options ResumeOptions) (*domain.Job, error) {
	job, err := s.state.LoadJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Status == domain.JobStatusDone || job.Status == domain.JobStatusFailed {
		return job, nil
	}
	if job.PendingApproval != nil {
		return job, fmt.Errorf("job has a pending approval; use approve or reject instead of resume")
	}
	if err := s.applyResumeExtraSteps(ctx, job, options.ExtraSteps); err != nil {
		return nil, err
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
	s.clearJobRuntimeState(job)
	s.addEvent(job, "job_cancelled", job.BlockedReason)
	s.touch(job)
	if err := s.state.SaveJob(ctx, job); err != nil {
		return nil, err
	}
	// Update cache with blocked state rather than removing -- the runLoop
	// goroutine may still be running and its defer will handle final eviction.
	s.cacheUpdate(job)
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
	// Prefer the in-memory cache for running jobs -- it reflects real-time
	// progress that has not yet been flushed to disk.
	if cached := s.cacheGet(jobID); cached != nil {
		return cached, nil
	}
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
	diskJobs, err := s.state.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	// Overlay cached (in-flight) snapshots on top of the disk list so callers
	// see real-time status for running jobs.
	s.cacheMu.RLock()
	if len(s.jobCache) == 0 {
		s.cacheMu.RUnlock()
		return diskJobs, nil
	}
	overlay := make(map[string]*domain.Job, len(s.jobCache))
	for k, v := range s.jobCache {
		overlay[k] = v
	}
	s.cacheMu.RUnlock()

	for i, dj := range diskJobs {
		if cached, ok := overlay[dj.ID]; ok {
			diskJobs[i] = *domain.CloneJob(cached)
			delete(overlay, dj.ID)
		}
	}
	// Append any cached jobs that were not on disk yet (unlikely but safe).
	for _, cached := range overlay {
		diskJobs = append(diskJobs, *domain.CloneJob(cached))
	}
	return diskJobs, nil
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

func (s *Service) runLoop(ctx context.Context, job *domain.Job) (result *domain.Job, err error) {
	if !s.claimJobRun(job.ID) {
		log.Printf("[gorchera] suppressing duplicate runLoop for job %s", job.ID)
		return s.latestJobSnapshot(job), nil
	}
	defer s.releaseJobRun(job.ID)
	stopLease := s.startJobLease(job)
	defer func() {
		stopLease()
		if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled)) && isRecoverableJobStatus(job.Status) {
			if interruptErr := s.interruptJob(context.Background(), job, "job interrupted while orchestrator context was cancelled"); interruptErr == nil {
				result = job
				err = nil
			}
		}
		s.finalizeJobLease(job)
		// Terminal jobs are fully persisted to disk; remove from cache so
		// subsequent Get() reads from the authoritative disk copy.
		if !isRecoverableJobStatus(job.Status) {
			s.cacheRemove(job.ID)
		}
	}()

	if len(job.PlanningArtifacts) == 0 || strings.TrimSpace(job.SprintContractRef) == "" {
		// Surface the "planning" phase to status API so supervisors see progress
		// instead of a stale "starting" that looks stuck.
		job.Status = domain.JobStatusPlanning
		s.cacheUpdate(job)
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
			if isShutdownInterruption(ctx, err) {
				return s.blockJobWithEvent(context.Background(), job, "job_interrupted", "orchestrator shutdown interrupted the leader phase")
			}
			if action == provider.ProviderErrorActionBlock {
				return s.blockJobWithEvent(ctx, job, "job_blocked", fmt.Sprintf("leader execution blocked: %v", err))
			}
			return s.failJob(ctx, job, fmt.Sprintf("leader execution failed: %v", err))
		}
		s.accumulateTokenUsage(job, job.CurrentStep, estimateProviderUsage(*job, domain.RoleDirector, rawLeader, *job))
		job.SupervisorDirective = ""

		// schemaRetry: retry up to schemaRetryMax times when JSON/schema
		// validation fails. The hint is injected into the next prompt so the
		// model can self-correct without counting as a new step.
		var leader domain.LeaderOutput
		for schemaAttempt := 0; ; schemaAttempt++ {
			parseErr := json.Unmarshal([]byte(rawLeader), &leader)
			if parseErr == nil {
				parseErr = schema.ValidateLeaderOutput(&leader)
			}
			if parseErr == nil {
				job.SchemaRetryHint = ""
				break
			}
			if schemaAttempt >= schemaRetryMax {
				job.SchemaRetryHint = ""
				return s.failJob(ctx, job, fmt.Sprintf("leader schema validation failed after %d attempts: %v", schemaRetryMax+1, parseErr))
			}
			hint := parseErr.Error()
			s.addEvent(job, "schema_retry", fmt.Sprintf("leader schema retry %d/%d: %s", schemaAttempt+1, schemaRetryMax, hint))
			job.SchemaRetryHint = hint
			rawLeader, _, err = s.executeProviderPhase(ctx, job, "leader", func() (string, error) {
				return s.sessions.RunLeader(ctx, *job)
			})
			if err != nil {
				job.SchemaRetryHint = ""
				return s.failJob(ctx, job, fmt.Sprintf("leader schema retry failed: %v", err))
			}
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
				// "blocked": evaluator lacks evidence -- recoverable, keep loop alive.
				// "failed": evaluator found concrete defects -- feed findings back to
				// the leader so it can dispatch fix steps before re-completing.
				if report.Status == "blocked" || report.Status == "failed" {
					job.LeaderContextSummary = report.Reason
					completionRetryPending = true
					completionRetryStepCount = len(job.Steps)
					leaderRetryPending = true
					continue
				}
				return job, nil
			}
			job.Status = domain.JobStatusDone
			job.Summary = leader.Reason
			s.clearJobRuntimeState(job)
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

	// Snapshot workspace before worker execution so we can detect changed files
	// even when the workspace is not a git repository.
	preSnapshot, _ := snapshotWorkspace(firstNonEmpty(job.WorkspaceDir, s.workspaceRoot))

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
		if isShutdownInterruption(ctx, err) {
			return s.interruptJob(context.Background(), job, "orchestrator shutdown interrupted the worker phase")
		}
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
	s.accumulateTokenUsage(job, step.Index, estimateProviderUsage(*job, domain.RoleForTaskType(task.TaskType), rawWorker, *job, task))

	// schemaRetry: retry up to schemaRetryMax times when JSON/schema
	// validation fails. The hint is injected into the next prompt so the
	// model can self-correct without counting as a new step.
	var worker domain.WorkerOutput
	for schemaAttempt := 0; ; schemaAttempt++ {
		parseErr := json.Unmarshal([]byte(rawWorker), &worker)
		if parseErr == nil {
			parseErr = schema.ValidateWorkerOutput(worker)
		}
		if parseErr == nil {
			job.SchemaRetryHint = ""
			break
		}
		if schemaAttempt >= schemaRetryMax {
			job.SchemaRetryHint = ""
			last := &job.Steps[len(job.Steps)-1]
			reason := fmt.Sprintf("worker schema validation failed after %d attempts: %v", schemaRetryMax+1, parseErr)
			structuredReason := classifyWorkerFailure(parseErr, rawWorker)
			last.Status = domain.StepStatusFailed
			last.ErrorReason = reason
			last.StructuredReason = structuredReason
			last.Summary = reason
			last.FinishedAt = time.Now().UTC()
			s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, structuredReason))
			_, failErr := s.failJob(ctx, job, reason)
			return failErr
		}
		hint := parseErr.Error()
		s.addEvent(job, "schema_retry", fmt.Sprintf("worker schema retry %d/%d: %s", schemaAttempt+1, schemaRetryMax, hint))
		job.SchemaRetryHint = hint
		rawWorker, _, err = s.executeProviderPhase(ctx, job, "worker", func() (string, error) {
			return s.sessions.RunWorker(ctx, *job, task)
		})
		if err != nil {
			job.SchemaRetryHint = ""
			last := &job.Steps[len(job.Steps)-1]
			reason := fmt.Sprintf("worker schema retry failed: %v", err)
			structuredReason := classifyWorkerFailure(err, "")
			last.Status = domain.StepStatusFailed
			last.ErrorReason = reason
			last.StructuredReason = structuredReason
			last.Summary = reason
			last.FinishedAt = time.Now().UTC()
			s.addEvent(job, "worker_failed", formatWorkerFailureEventMessage(reason, structuredReason))
			_, failErr := s.failJob(ctx, job, reason)
			return failErr
		}
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
		workDir := firstNonEmpty(job.WorkspaceDir, s.workspaceRoot)
		last.DiffSummary = collectWorkspaceDiffSummary(ctx, workDir)
		last.ChangedFiles = detectChangedFiles(ctx, workDir, preSnapshot)
		last.Status = domain.StepStatusSucceeded
		job.Status = domain.JobStatusRunning
		job.FailureReason = ""
		s.addEvent(job, "worker_succeeded", worker.Summary)
		if err := s.runEngineVerificationForStep(ctx, job, last); err != nil {
			return err
		}
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
	if strings.TrimSpace(last.Summary) != "" {
		job.LeaderContextSummary = last.Summary
	}
	job.LeaderContextSummary = sanitizeLeaderContext(job.LeaderContextSummary)
	s.touch(job)
	return s.state.SaveJob(ctx, job)
}

func (s *Service) runEngineVerificationForStep(ctx context.Context, job *domain.Job, step *domain.Step) error {
	if step == nil || !strings.EqualFold(step.TaskType, "implement") || step.Status != domain.StepStatusSucceeded {
		return nil
	}

	records, summary, failureReason, err := s.executeEngineVerification(ctx, *job, step.Index)
	if err != nil {
		if isShutdownInterruption(ctx, err) || (ctx.Err() != nil && errors.Is(err, ctx.Err())) {
			return s.interruptJob(context.Background(), job, "orchestrator shutdown interrupted engine verification")
		}
		_, failErr := s.failJob(ctx, job, fmt.Sprintf("engine verification failed: %v", err))
		return failErr
	}

	if len(records) == 0 {
		return nil
	}

	artifactPaths := make([]string, 0, len(records))
	for _, record := range records {
		path, materializeErr := s.artifacts.MaterializeJSONArtifact(job.ID, engineVerificationArtifactName(step.Index, record.Kind), record)
		if materializeErr != nil {
			_, failErr := s.failJob(ctx, job, fmt.Sprintf("engine verification artifact materialization failed: %v", materializeErr))
			return failErr
		}
		artifactPaths = append(artifactPaths, path)
	}
	step.Artifacts = append(step.Artifacts, artifactPaths...)
	step.Summary = appendStepSummary(step.Summary, summary)

	if failureReason != "" {
		step.Status = domain.StepStatusFailed
		step.ErrorReason = failureReason
		job.Status = domain.JobStatusRunning
		job.FailureReason = failureReason
		s.addEvent(job, "engine_verification_failed", failureReason)
		return nil
	}

	s.addEvent(job, "engine_verification_recorded", summary)
	return nil
}

// runPreBuildCommands executes each command in workspaceDir before engine
// verification. Commands are parsed by splitting on spaces (no shell expansion),
// so each entry should be a single executable with arguments, e.g. "go mod tidy".
// Failures are logged but do not abort the engine build/test run (best-effort).
func (s *Service) runPreBuildCommands(ctx context.Context, cmds []string, workspaceDir string) {
	if len(cmds) == 0 {
		return
	}
	for _, raw := range cmds {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Fields(raw)
		req := runtimeexec.Request{
			Category: runtimeexec.CategoryBuild,
			Command:  parts[0],
			Args:     parts[1:],
			Dir:      workspaceDir,
			Timeout:  2 * time.Minute,
		}
		result, err := s.runtime.Run(ctx, req)
		if err != nil {
			// Best-effort: log and continue. Context cancellation is the only
			// case where we should stop early (shutdown in progress).
			if ctx.Err() != nil {
				log.Printf("[engine] pre_build aborted (shutdown): %s", raw)
				return
			}
			log.Printf("[engine] pre_build command failed (continuing): %q: %v -- stderr: %s", raw, err, strings.TrimSpace(result.Stderr))
		} else {
			log.Printf("[engine] pre_build command ok: %q", raw)
		}
	}
}

func (s *Service) executeEngineVerification(ctx context.Context, job domain.Job, stepIndex int) ([]EngineCheckArtifact, string, string, error) {
	workspaceDir := firstNonEmpty(job.WorkspaceDir, s.workspaceRoot)
	if strings.TrimSpace(workspaceDir) == "" {
		return engineSkippedArtifacts(job.EngineBuildCmd, job.EngineTestCmd, "workspace directory is not available"), "engine verification skipped: workspace directory is not available", "", nil
	}

	// Resolve the build and test commands. Custom commands bypass the Go-specific
	// workspace checks so that non-Go projects (Node, Python, etc.) work correctly.
	buildCmd, buildArgs, buildCmdStr := resolveEngineCommand(job.EngineBuildCmd, "go", []string{"build", "./..."})
	testCmd, testArgs, testCmdStr := resolveEngineCommand(job.EngineTestCmd, "go", []string{"test", "./..."})

	// When using the default Go commands, require a valid Go workspace.
	// Custom commands skip this check -- the caller knows their own toolchain.
	if job.EngineBuildCmd == "" {
		if _, err := s.runtime.LookPath("go"); err != nil {
			return engineSkippedArtifacts(job.EngineBuildCmd, job.EngineTestCmd, "go executable is not available"), "engine verification skipped: go executable is not available", "", nil
		}
		if !hasGoWorkspace(workspaceDir) {
			return engineSkippedArtifacts(job.EngineBuildCmd, job.EngineTestCmd, "workspace is not configured for Go"), "engine verification skipped: workspace is not configured for Go", "", nil
		}
	}

	// Run pre-build commands before engine verification.
	// These are best-effort: a failure is logged but does not skip build/test.
	// Typical use: "go mod tidy", "npm install", "make generate".
	s.runPreBuildCommands(ctx, job.PreBuildCommands, workspaceDir)

	buildReq := runtimeexec.Request{
		Category: runtimeexec.CategoryBuild,
		Command:  buildCmd,
		Args:     buildArgs,
		Dir:      workspaceDir,
		Timeout:  5 * time.Minute,
	}
	buildResult, buildErr := s.runtime.Run(ctx, buildReq)
	if ctx.Err() != nil && (errors.Is(buildErr, context.Canceled) || errors.Is(buildErr, context.DeadlineExceeded)) {
		return nil, "", "", buildErr
	}
	buildRecord := engineCheckFromResult("build", buildReq, buildResult, buildErr)
	records := []EngineCheckArtifact{buildRecord}
	summaryParts := []string{formatEngineCheckSummary(buildRecord)}
	if buildErr != nil {
		testRecord := EngineCheckArtifact{
			Kind:    "test",
			Status:  engineCheckSkipped,
			Command: testCmdStr,
			Reason:  buildCmdStr + " failed",
		}
		records = append(records, testRecord)
		summaryParts = append(summaryParts, formatEngineCheckSummary(testRecord))
		return records, strings.Join(summaryParts, "; "), buildRecord.Reason, nil
	}

	testReq := runtimeexec.Request{
		Category: runtimeexec.CategoryTest,
		Command:  testCmd,
		Args:     testArgs,
		Dir:      workspaceDir,
		Timeout:  5 * time.Minute,
	}
	testResult, testErr := s.runtime.Run(ctx, testReq)
	if ctx.Err() != nil && (errors.Is(testErr, context.Canceled) || errors.Is(testErr, context.DeadlineExceeded)) {
		return nil, "", "", testErr
	}
	testRecord := engineCheckFromResult("test", testReq, testResult, testErr)
	records = append(records, testRecord)
	summaryParts = append(summaryParts, formatEngineCheckSummary(testRecord))
	if testErr != nil {
		return records, strings.Join(summaryParts, "; "), testRecord.Reason, nil
	}
	return records, strings.Join(summaryParts, "; "), "", nil
}

// resolveEngineCommand parses a custom command string (e.g. "npm run build") into
// executable + args using strings.Fields. Falls back to defaultCmd/defaultArgs when
// custom is empty. Returns the executable, args, and a human-readable command string.
func resolveEngineCommand(custom, defaultCmd string, defaultArgs []string) (string, []string, string) {
	if strings.TrimSpace(custom) == "" {
		return defaultCmd, defaultArgs, strings.Join(append([]string{defaultCmd}, defaultArgs...), " ")
	}
	parts := strings.Fields(custom)
	if len(parts) == 1 {
		return parts[0], nil, parts[0]
	}
	return parts[0], parts[1:], custom
}

func engineVerificationArtifactName(stepIndex int, kind string) string {
	return fmt.Sprintf("step-%02d-engine_%s.json", stepIndex, kind)
}

func engineSkippedArtifacts(buildCmd, testCmd, reason string) []EngineCheckArtifact {
	_, _, buildStr := resolveEngineCommand(buildCmd, "go", []string{"build", "./..."})
	_, _, testStr := resolveEngineCommand(testCmd, "go", []string{"test", "./..."})
	return []EngineCheckArtifact{
		{Kind: "build", Status: engineCheckSkipped, Command: buildStr, Reason: reason},
		{Kind: "test", Status: engineCheckSkipped, Command: testStr, Reason: reason},
	}
}

func engineCheckFromResult(kind string, req runtimeexec.Request, result runtimeexec.Result, runErr error) EngineCheckArtifact {
	record := EngineCheckArtifact{
		Kind:    kind,
		Status:  engineCheckPassed,
		Command: strings.TrimSpace(strings.Join(append([]string{req.Command}, req.Args...), " ")),
		Result:  &result,
	}
	if runErr != nil {
		record.Status = engineCheckFailed
		record.Reason = firstNonEmpty(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout), runErr.Error())
	}
	return record
}

func formatEngineCheckSummary(record EngineCheckArtifact) string {
	switch record.Status {
	case engineCheckSkipped:
		return fmt.Sprintf("%s skipped (%s)", record.Kind, record.Reason)
	case engineCheckFailed:
		return fmt.Sprintf("%s failed (%s)", record.Kind, record.Reason)
	default:
		return record.Kind + " passed"
	}
}

func appendStepSummary(base, extra string) string {
	base = strings.TrimSpace(base)
	extra = strings.TrimSpace(extra)
	switch {
	case base == "":
		return extra
	case extra == "":
		return base
	default:
		return base + "\nEngine verification: " + extra
	}
}

func hasGoWorkspace(workspaceDir string) bool {
	for _, name := range []string{"go.mod", "go.work"} {
		if _, err := os.Stat(filepath.Join(workspaceDir, name)); err == nil {
			return true
		}
	}
	return false
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
	if isShutdownInterruption(ctx, runErr) {
		return s.interruptJob(context.Background(), job, "orchestrator shutdown interrupted the system action")
	}
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
	s.clearJobRuntimeState(job)
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
	s.clearJobRuntimeState(job)
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

func isShutdownInterruption(ctx context.Context, err error) bool {
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled) && errors.Is(err, context.Canceled)
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
	s.cacheUpdate(job)
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
	case domain.JobStatusPlanning, domain.JobStatusRunning, domain.JobStatusWaitingLeader, domain.JobStatusWaitingWorker:
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

// cacheGet returns a deep clone of the cached job, or nil if not cached.
// Callers get an independent copy so they cannot corrupt the cache.
func (s *Service) cacheGet(jobID string) *domain.Job {
	s.cacheMu.RLock()
	cached, ok := s.jobCache[jobID]
	s.cacheMu.RUnlock()
	if !ok {
		return nil
	}
	return domain.CloneJob(cached)
}

// cacheUpdate stores a deep clone of the job into the in-memory cache.
// The cache owns its own copy so mutations to the caller's pointer do not
// silently alter the cached state.
func (s *Service) cacheUpdate(job *domain.Job) {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return
	}
	clone := domain.CloneJob(job)
	s.cacheMu.Lock()
	s.jobCache[job.ID] = clone
	s.cacheMu.Unlock()
}

// cacheRemove deletes a job from the in-memory cache. Called when the job
// reaches a terminal state and has been persisted to disk.
func (s *Service) cacheRemove(jobID string) {
	s.cacheMu.Lock()
	delete(s.jobCache, jobID)
	s.cacheMu.Unlock()
}

func (s *Service) addEvent(job *domain.Job, kind, message string) {
	job.Events = append(job.Events, domain.Event{
		Time:    time.Now().UTC(),
		Kind:    kind,
		Message: message,
	})
	// Keep the in-memory cache up to date so status API reflects progress
	// immediately, even before the next disk checkpoint.
	s.cacheUpdate(job)
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
	now := time.Now().UTC()
	job.UpdatedAt = now
	if isRecoverableJobStatus(job.Status) {
		job.RunOwnerID = s.instanceID
		job.RunHeartbeatAt = now
		s.cacheUpdate(job)
		return
	}
	job.RunOwnerID = ""
	job.RunHeartbeatAt = time.Time{}
	s.cacheUpdate(job)
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

	// Cap git diff --stat at 10 seconds so large worktrees cannot stall the
	// orchestrator core loop while it collects the diff summary.
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "git", "-C", workspaceDir, "diff", "--stat")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}

	return strings.TrimSpace(stdout.String())
}
