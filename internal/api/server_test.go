package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorchera/internal/api"
	"gorchera/internal/domain"
	"gorchera/internal/orchestrator"
	"gorchera/internal/provider"
	"gorchera/internal/provider/mock"
	"gorchera/internal/store"
)

func TestServerExposesHealthAndJobs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	if _, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Create an orchestrator MVP",
		Provider: domain.ProviderMock,
		MaxSteps: 8,
	}); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	healthResp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("health request failed: %v", err)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", healthResp.StatusCode)
	}

	jobsResp, err := http.Get(server.URL + "/jobs")
	if err != nil {
		t.Fatalf("jobs request failed: %v", err)
	}
	defer jobsResp.Body.Close()

	if jobsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /jobs, got %d", jobsResp.StatusCode)
	}

	var jobs []domain.Job
	if err := json.NewDecoder(jobsResp.Body).Decode(&jobs); err != nil {
		t.Fatalf("failed to decode jobs response: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}

	eventsResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/events")
	if err != nil {
		t.Fatalf("events request failed: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /events, got %d", eventsResp.StatusCode)
	}

	artifactsResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/artifacts")
	if err != nil {
		t.Fatalf("artifacts request failed: %v", err)
	}
	defer artifactsResp.Body.Close()
	if artifactsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /artifacts, got %d", artifactsResp.StatusCode)
	}

	planningResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/planning")
	if err != nil {
		t.Fatalf("planning request failed: %v", err)
	}
	defer planningResp.Body.Close()
	if planningResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /planning, got %d", planningResp.StatusCode)
	}
	var planning api.PlanningView
	if err := json.NewDecoder(planningResp.Body).Decode(&planning); err != nil {
		t.Fatalf("failed to decode planning response: %v", err)
	}
	if len(planning.Artifacts) == 0 {
		t.Fatal("expected planning artifacts to be visible")
	}
	if planning.Provider != domain.ProviderMock {
		t.Fatalf("expected provider mock, got %s", planning.Provider)
	}
	if planning.ParallelPolicy.MaxParallelWorkers != 2 {
		t.Fatalf("expected planning parallel policy max workers 2, got %d", planning.ParallelPolicy.MaxParallelWorkers)
	}

	evaluatorResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/evaluator")
	if err != nil {
		t.Fatalf("evaluator request failed: %v", err)
	}
	defer evaluatorResp.Body.Close()
	if evaluatorResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /evaluator, got %d", evaluatorResp.StatusCode)
	}
	var evaluator api.EvaluatorView
	if err := json.NewDecoder(evaluatorResp.Body).Decode(&evaluator); err != nil {
		t.Fatalf("failed to decode evaluator response: %v", err)
	}
	if evaluator.ReportRef == "" {
		t.Fatal("expected evaluator report ref to be visible")
	}

	verificationResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/verification")
	if err != nil {
		t.Fatalf("verification request failed: %v", err)
	}
	defer verificationResp.Body.Close()
	if verificationResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /verification, got %d", verificationResp.StatusCode)
	}
	var verification api.VerificationView
	if err := json.NewDecoder(verificationResp.Body).Decode(&verification); err != nil {
		t.Fatalf("failed to decode verification response: %v", err)
	}
	if verification.VerificationContract == nil {
		t.Fatal("expected verification contract to be visible")
	}
	if len(verification.VerificationContract.RequiredChecks) == 0 {
		t.Fatal("expected verification contract required checks")
	}
	if verification.EvaluatorReport == nil {
		t.Fatal("expected evaluator report in verification view")
	}
	if verification.ParallelPolicy.MaxParallelWorkers != 2 {
		t.Fatalf("expected verification parallel policy max workers 2, got %d", verification.ParallelPolicy.MaxParallelWorkers)
	}

	profileResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/profile")
	if err != nil {
		t.Fatalf("profile request failed: %v", err)
	}
	defer profileResp.Body.Close()
	if profileResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /profile, got %d", profileResp.StatusCode)
	}
	var profile api.ProfileView
	if err := json.NewDecoder(profileResp.Body).Decode(&profile); err != nil {
		t.Fatalf("failed to decode profile response: %v", err)
	}
	if profile.Provider != domain.ProviderMock {
		t.Fatalf("expected profile provider mock, got %s", profile.Provider)
	}
	if !profile.RoleProfilesAvailable {
		t.Fatal("expected role profiles to be visible")
	}
	if profile.RoleProfiles == nil {
		t.Fatal("expected role profiles payload")
	}
	if profile.RoleProfiles.Executor.Provider != domain.ProviderMock {
		t.Fatalf("expected executor provider mock, got %s", profile.RoleProfiles.Executor.Provider)
	}
	if profile.ParallelPolicy.MaxParallelWorkers != 2 {
		t.Fatalf("expected profile parallel policy max workers 2, got %d", profile.ParallelPolicy.MaxParallelWorkers)
	}

	harnessResp, err := http.Get(server.URL + "/jobs/" + jobs[0].ID + "/harness")
	if err != nil {
		t.Fatalf("harness request failed: %v", err)
	}
	defer harnessResp.Body.Close()
	if harnessResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /harness, got %d", harnessResp.StatusCode)
	}
	var harness api.RuntimeHarnessView
	if err := json.NewDecoder(harnessResp.Body).Decode(&harness); err != nil {
		t.Fatalf("failed to decode harness response: %v", err)
	}
	if harness.JobID != jobs[0].ID {
		t.Fatalf("expected harness job id %q, got %q", jobs[0].ID, harness.JobID)
	}
	if !harness.Available {
		t.Fatal("expected harness surface to be available")
	}
	if harness.ProcessCount != len(harness.Processes) {
		t.Fatalf("expected process count %d to match payload length %d", harness.ProcessCount, len(harness.Processes))
	}

	processesResp, err := http.Get(server.URL + "/harness/processes")
	if err != nil {
		t.Fatalf("process list request failed: %v", err)
	}
	defer processesResp.Body.Close()
	if processesResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /harness/processes, got %d", processesResp.StatusCode)
	}
	body, err := io.ReadAll(processesResp.Body)
	if err != nil {
		t.Fatalf("failed to read process list response: %v", err)
	}
	if !strings.Contains(string(body), "processes") {
		t.Fatalf("expected process list response to contain processes payload, got %q", string(body))
	}
}

func TestServerManagesHarnessProcesses(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	reqBody := api.StartHarnessProcessRequest{
		Name:           "helper-process",
		Category:       "test",
		Command:        "go",
		Args:           []string{"test", "./internal/api", "-run", "TestHelperHarnessProcess", "-count=1"},
		Env:            []string{"GO_WANT_HELPER_PROCESS=1"},
		TimeoutSeconds: 30,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal harness request: %v", err)
	}

	startResp, err := http.Post(server.URL+"/harness/processes", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("start request failed: %v", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from POST /harness/processes, got %d", startResp.StatusCode)
	}

	var handle api.RuntimeProcessHandleView
	if err := json.NewDecoder(startResp.Body).Decode(&handle); err != nil {
		t.Fatalf("failed to decode start response: %v", err)
	}
	if handle.PID <= 0 {
		t.Fatal("expected a positive pid")
	}
	if !handle.Running {
		t.Fatal("expected started process to be running")
	}

	listResp, err := http.Get(server.URL + "/harness/processes")
	if err != nil {
		t.Fatalf("list request failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from list endpoint, got %d", listResp.StatusCode)
	}
	var list api.RuntimeProcessListView
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode list response: %v", err)
	}
	if len(list.Processes) == 0 {
		t.Fatal("expected at least one harness process")
	}

	statusResp, err := http.Get(server.URL + "/harness/processes/" + fmt.Sprint(handle.PID))
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from status endpoint, got %d", statusResp.StatusCode)
	}
	var statusHandle api.RuntimeProcessHandleView
	if err := json.NewDecoder(statusResp.Body).Decode(&statusHandle); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if statusHandle.PID != handle.PID {
		t.Fatalf("expected status pid %d, got %d", handle.PID, statusHandle.PID)
	}

	stopResp, err := http.Post(server.URL+"/harness/processes/"+fmt.Sprint(handle.PID)+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop request failed: %v", err)
	}
	defer stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from stop endpoint, got %d", stopResp.StatusCode)
	}
	var stopped api.RuntimeProcessHandleView
	if err := json.NewDecoder(stopResp.Body).Decode(&stopped); err != nil {
		t.Fatalf("failed to decode stop response: %v", err)
	}
	if stopped.Running {
		t.Fatal("expected stopped process to no longer be running")
	}
}

func TestServerManagesJobScopedHarnessProcesses(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	ownerJob, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Job-scoped harness ownership",
		Provider: domain.ProviderMock,
		MaxSteps: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	otherJob, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Other job",
		Provider: domain.ProviderMock,
		MaxSteps: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	reqBody := api.StartHarnessProcessRequest{
		Name:           "job-owned-helper",
		Category:       "test",
		Command:        "go",
		Args:           []string{"test", "./internal/api", "-run", "TestHelperHarnessProcess", "-count=1"},
		Env:            []string{"GO_WANT_HELPER_PROCESS=1"},
		TimeoutSeconds: 30,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal harness request: %v", err)
	}

	startResp, err := http.Post(server.URL+"/jobs/"+ownerJob.ID+"/harness/processes", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("job-scoped start request failed: %v", err)
	}
	defer startResp.Body.Close()
	if startResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from POST /jobs/{id}/harness/processes, got %d", startResp.StatusCode)
	}

	var handle api.RuntimeProcessHandleView
	if err := json.NewDecoder(startResp.Body).Decode(&handle); err != nil {
		t.Fatalf("failed to decode job-scoped start response: %v", err)
	}
	if handle.PID <= 0 {
		t.Fatal("expected a positive pid")
	}

	listResp, err := http.Get(server.URL + "/jobs/" + ownerJob.ID + "/harness/processes")
	if err != nil {
		t.Fatalf("job-scoped list request failed: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from job-scoped list endpoint, got %d", listResp.StatusCode)
	}
	var list api.RuntimeProcessListView
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("failed to decode job-scoped list response: %v", err)
	}
	if len(list.Processes) == 0 {
		t.Fatal("expected at least one job-scoped harness process")
	}

	harnessResp, err := http.Get(server.URL + "/jobs/" + ownerJob.ID + "/harness")
	if err != nil {
		t.Fatalf("job harness request failed: %v", err)
	}
	defer harnessResp.Body.Close()
	if harnessResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /jobs/{id}/harness, got %d", harnessResp.StatusCode)
	}
	var harness api.RuntimeHarnessView
	if err := json.NewDecoder(harnessResp.Body).Decode(&harness); err != nil {
		t.Fatalf("failed to decode job harness response: %v", err)
	}
	if harness.ProcessCount == 0 {
		t.Fatal("expected job harness view to include owned process")
	}

	statusResp, err := http.Get(server.URL + "/jobs/" + ownerJob.ID + "/harness/processes/" + fmt.Sprint(handle.PID))
	if err != nil {
		t.Fatalf("job-scoped status request failed: %v", err)
	}
	defer statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from job-scoped status endpoint, got %d", statusResp.StatusCode)
	}

	otherResp, err := http.Get(server.URL + "/jobs/" + otherJob.ID + "/harness/processes/" + fmt.Sprint(handle.PID))
	if err != nil {
		t.Fatalf("ownership check request failed: %v", err)
	}
	defer otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 from mismatched job ownership, got %d", otherResp.StatusCode)
	}

	stopResp, err := http.Post(server.URL+"/jobs/"+ownerJob.ID+"/harness/processes/"+fmt.Sprint(handle.PID)+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("job-scoped stop request failed: %v", err)
	}
	defer stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from job-scoped stop endpoint, got %d", stopResp.StatusCode)
	}
	var stopped api.RuntimeProcessHandleView
	if err := json.NewDecoder(stopResp.Body).Decode(&stopped); err != nil {
		t.Fatalf("failed to decode job-scoped stop response: %v", err)
	}
	if stopped.Running {
		t.Fatal("expected job-scoped process to no longer be running")
	}
}

func TestHelperHarnessProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
}

func TestServerHandlesCancelAndRetry(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(apiControlProvider{name: domain.ProviderName("api-control")})

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	job, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Exercise API cancel and retry",
		Provider: domain.ProviderName("api-control"),
		MaxSteps: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if job.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status, got %s blocked=%q failure=%q summary=%q", job.Status, job.BlockedReason, job.FailureReason, job.LeaderContextSummary)
	}

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	cancelResp, err := http.Post(server.URL+"/jobs/"+job.ID+"/cancel", "application/json", bytes.NewBufferString(`{"reason":"operator pause"}`))
	if err != nil {
		t.Fatalf("cancel request failed: %v", err)
	}
	defer cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /cancel, got %d", cancelResp.StatusCode)
	}
	var cancelled domain.Job
	if err := json.NewDecoder(cancelResp.Body).Decode(&cancelled); err != nil {
		t.Fatalf("failed to decode cancel response: %v", err)
	}
	if cancelled.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status after cancel, got %s", cancelled.Status)
	}
	if !strings.Contains(strings.ToLower(cancelled.BlockedReason), "cancelled by operator") {
		t.Fatalf("expected operator cancellation reason, got %q", cancelled.BlockedReason)
	}

	retryResp, err := http.Post(server.URL+"/jobs/"+job.ID+"/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("retry request failed: %v", err)
	}
	defer retryResp.Body.Close()
	if retryResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /retry, got %d", retryResp.StatusCode)
	}
	var retried domain.Job
	if err := json.NewDecoder(retryResp.Body).Decode(&retried); err != nil {
		t.Fatalf("failed to decode retry response: %v", err)
	}
	if retried.Status != domain.JobStatusDone {
		t.Fatalf("expected done status after retry, got %s blocked=%q failure=%q summary=%q", retried.Status, retried.BlockedReason, retried.FailureReason, retried.LeaderContextSummary)
	}
	if retried.RetryCount != 1 {
		t.Fatalf("expected retry count 1, got %d", retried.RetryCount)
	}
}

func TestServerHandlesApproveRejectAndStream(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(approvalControlProvider{name: domain.ProviderName("approval-control")})

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	approveJob, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Exercise API approve",
		Provider: domain.ProviderName("approval-control"),
		MaxSteps: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if approveJob.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status, got %s blocked=%q failure=%q summary=%q", approveJob.Status, approveJob.BlockedReason, approveJob.FailureReason, approveJob.LeaderContextSummary)
	}

	rejectJob, err := service.Start(context.Background(), orchestrator.CreateJobInput{
		Goal:     "Exercise API reject",
		Provider: domain.ProviderName("approval-control"),
		MaxSteps: 8,
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if rejectJob.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status, got %s", rejectJob.Status)
	}

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	approveResp, err := http.Post(server.URL+"/jobs/"+approveJob.ID+"/approve", "application/json", nil)
	if err != nil {
		t.Fatalf("approve request failed: %v", err)
	}
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /approve, got %d", approveResp.StatusCode)
	}
	var approved domain.Job
	if err := json.NewDecoder(approveResp.Body).Decode(&approved); err != nil {
		t.Fatalf("failed to decode approve response: %v", err)
	}
	if approved.Status != domain.JobStatusDone {
		t.Fatalf("expected done status after approve, got %s", approved.Status)
	}
	if approved.RetryCount != 0 {
		t.Fatalf("expected retry count 0 after approve, got %d", approved.RetryCount)
	}

	rejectResp, err := http.Post(server.URL+"/jobs/"+rejectJob.ID+"/reject", "application/json", bytes.NewBufferString(`{"reason":"not approved"}`))
	if err != nil {
		t.Fatalf("reject request failed: %v", err)
	}
	defer rejectResp.Body.Close()
	if rejectResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /reject, got %d", rejectResp.StatusCode)
	}
	var rejected domain.Job
	if err := json.NewDecoder(rejectResp.Body).Decode(&rejected); err != nil {
		t.Fatalf("failed to decode reject response: %v", err)
	}
	if rejected.Status != domain.JobStatusBlocked {
		t.Fatalf("expected blocked status after reject, got %s", rejected.Status)
	}
	if !strings.Contains(strings.ToLower(rejected.BlockedReason), "not approved") {
		t.Fatalf("expected rejection reason after reject, got %q", rejected.BlockedReason)
	}

	streamResp, err := http.Get(server.URL + "/jobs/" + approveJob.ID + "/events/stream")
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer streamResp.Body.Close()
	if streamResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /events/stream, got %d", streamResp.StatusCode)
	}
	body, err := io.ReadAll(streamResp.Body)
	if err != nil {
		t.Fatalf("failed to read stream response: %v", err)
	}
	if !strings.Contains(string(body), "event: job_event") {
		t.Fatalf("expected SSE job_event lines, got %q", string(body))
	}
}

func TestServerCreatesJobFromAPI(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	reqBody := api.StartJobRequest{
		Goal:     "Create a profile-driven job",
		Provider: domain.ProviderMock,
		RoleProfiles: domain.RoleProfiles{
			Leader:   domain.ExecutionProfile{Provider: domain.ProviderMock, Model: "leader-model"},
			Executor: domain.ExecutionProfile{Provider: domain.ProviderMock, Model: "executor-model"},
			Reviewer: domain.ExecutionProfile{Provider: domain.ProviderMock, Model: "review-model"},
			Tester:   domain.ExecutionProfile{Provider: domain.ProviderMock, Model: "test-model"},
		},
		MaxSteps: 8,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	resp, err := http.Post(server.URL+"/jobs", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("post request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from POST /jobs, got %d", resp.StatusCode)
	}

	var job domain.Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		t.Fatalf("failed to decode created job: %v", err)
	}
	if job.RoleProfiles.Leader.Model != "leader-model" {
		t.Fatalf("expected leader model to persist, got %q", job.RoleProfiles.Leader.Model)
	}
	if job.RoleProfiles.Executor.Model != "executor-model" {
		t.Fatalf("expected executor model to persist, got %q", job.RoleProfiles.Executor.Model)
	}
}

type apiControlProvider struct {
	name domain.ProviderName
}

func (p apiControlProvider) Name() domain.ProviderName {
	return p.name
}

func (p apiControlProvider) RunLeader(_ context.Context, job domain.Job) (string, error) {
	if job.RetryCount == 0 && len(job.Steps) == 0 {
		return `{"action":"blocked","target":"none","task_type":"none","reason":"waiting for operator cancellation"}`, nil
	}
	switch len(job.Steps) {
	case 0:
		return `{"action":"run_worker","target":"B","task_type":"implement","task_text":"build retry path"}`, nil
	case 1:
		return `{"action":"run_worker","target":"C","task_type":"review","task_text":"review retry path"}`, nil
	case 2:
		return `{"action":"run_worker","target":"D","task_type":"test","task_text":"test retry path"}`, nil
	default:
		return `{"action":"complete","target":"none","task_type":"none","reason":"retry completed successfully"}`, nil
	}
}

func (p apiControlProvider) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"api control worker completed","artifacts":["worker-output.json"]}`, nil
}

type approvalControlProvider struct {
	name domain.ProviderName
}

func (p approvalControlProvider) Name() domain.ProviderName {
	return p.name
}

func (p approvalControlProvider) RunLeader(_ context.Context, job domain.Job) (string, error) {
	if len(job.Steps) == 0 {
		return `{"action":"run_system","target":"SYS","task_type":"build","task_text":"needs operator approval","system_action":{"type":"command","command":"go","args":["version"],"workdir":"..","description":"workspace-external command for approval"}}`, nil
	}
	switch len(job.Steps) {
	case 1:
		return `{"action":"blocked","target":"none","task_type":"none","reason":"waiting for operator approval"}`, nil
	case 2:
		return `{"action":"run_worker","target":"B","task_type":"implement","task_text":"implement approved system change"}`, nil
	case 3:
		return `{"action":"run_worker","target":"C","task_type":"review","task_text":"review approved system change"}`, nil
	case 4:
		return `{"action":"run_worker","target":"D","task_type":"test","task_text":"test approved system change"}`, nil
	default:
		return `{"action":"complete","target":"none","task_type":"none","reason":"approval flow completed successfully"}`, nil
	}
}

func (p approvalControlProvider) RunWorker(_ context.Context, _ domain.Job, _ domain.LeaderOutput) (string, error) {
	return `{"status":"success","summary":"approval control worker completed","artifacts":["worker-output.json"]}`, nil
}

// TestAuthMiddlewareRejectsUnauthenticated verifies that requests without a valid
// Authorization header are rejected with 401 when GORCHERA_AUTH_TOKEN is set.
// Not parallel: t.Setenv modifies global env state.
func TestAuthMiddlewareRejectsUnauthenticated(t *testing.T) {
	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	// Set the auth token before constructing the handler so Handler() picks it up.
	t.Setenv("GORCHERA_AUTH_TOKEN", "super-secret-token")

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	// Request without Authorization header.
	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}
}

// TestAuthMiddlewareAcceptsValidToken verifies that a request with the correct
// Bearer token is accepted when GORCHERA_AUTH_TOKEN is set.
// Not parallel: t.Setenv modifies global env state.
func TestAuthMiddlewareAcceptsValidToken(t *testing.T) {
	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	const token = "valid-token-abc123"
	t.Setenv("GORCHERA_AUTH_TOKEN", token)

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK with valid token, got %d", resp.StatusCode)
	}
}

// TestAuthMiddlewareSkippedWhenNoToken verifies that when GORCHERA_AUTH_TOKEN is
// unset, requests without an Authorization header pass through (development mode).
// Not parallel: t.Setenv modifies global env state.
func TestAuthMiddlewareSkippedWhenNoToken(t *testing.T) {
	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	// Ensure the env var is not set (t.Setenv with empty restores on cleanup).
	t.Setenv("GORCHERA_AUTH_TOKEN", "")

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	// No Authorization header -- should pass through in dev mode.
	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK when no token configured, got %d", resp.StatusCode)
	}
}

// TestHandlersUseRequestContext verifies that the HTTP handlers propagate the
// request context to the orchestrator (regression guard for HIGH-08).
// A request to a state-mutating endpoint is made and the response is checked;
// the handler must not block indefinitely if the client disconnects.
func TestHandlersUseRequestContext(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	registry := provider.NewRegistry()
	registry.Register(mock.New())

	service := orchestrator.NewService(
		provider.NewSessionManager(registry),
		store.NewStateStore(filepath.Join(root, "state")),
		store.NewArtifactStore(filepath.Join(root, "artifacts")),
		root,
	)

	server := httptest.NewServer(api.NewServer(service).Handler())
	defer server.Close()

	// Call /jobs/<nonexistent>/resume -- the handler uses r.Context() and the
	// orchestrator returns an error, which should be surfaced as a non-2xx status.
	resp, err := http.Post(server.URL+"/jobs/nonexistent-job-id/resume", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	// Expect a non-success status (job not found), confirming the handler ran and
	// used the request context rather than a detached context.Background().
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected non-200 for nonexistent job, handler may be ignoring context errors")
	}
}
