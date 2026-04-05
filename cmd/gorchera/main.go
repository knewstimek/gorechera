package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"strings"
	"time"

	"gorchera/internal/api"
	"gorchera/internal/domain"
	"gorchera/internal/mcp"
	"gorchera/internal/orchestrator"
	"gorchera/internal/provider"
	"gorchera/internal/provider/mock"
	runtimeexec "gorchera/internal/runtime"
	"gorchera/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	workspaceRoot, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	service := buildService(workspaceRoot)

	ctx := context.Background()

	switch os.Args[1] {
	case "run":
		run(ctx, service, os.Args[2:])
	case "status":
		status(ctx, service, os.Args[2:])
	case "events":
		events(ctx, service, os.Args[2:])
	case "artifacts":
		artifacts(ctx, service, os.Args[2:])
	case "verification":
		verification(ctx, service, os.Args[2:])
	case "planning":
		planning(ctx, service, os.Args[2:])
	case "evaluator":
		evaluator(ctx, service, os.Args[2:])
	case "profile":
		profile(ctx, service, os.Args[2:])
	case "resume":
		resume(ctx, service, os.Args[2:])
	case "approve":
		approve(ctx, service, os.Args[2:])
	case "retry":
		retry(ctx, service, os.Args[2:])
	case "cancel":
		cancel(ctx, service, os.Args[2:])
	case "reject":
		reject(ctx, service, os.Args[2:])
	case "harness-start":
		harnessStart(ctx, service, os.Args[2:])
	case "harness-view":
		harnessView(ctx, service, os.Args[2:])
	case "harness-list":
		harnessList(ctx, service, os.Args[2:])
	case "harness-status":
		harnessStatus(ctx, service, os.Args[2:])
	case "harness-stop":
		harnessStop(ctx, service, os.Args[2:])
	case "stream":
		stream(os.Args[2:])
	case "serve":
		serve(service, os.Args[2:])
	case "stop":
		stop(os.Args[2:])
	case "mcp":
		runMCP(service, os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

type startupRecoverOptions struct {
	enabled bool
	jobIDs  []string
}

type serveOptions struct {
	addr      string
	workspace string
	recover   startupRecoverOptions
}

func parseServeOptions(args []string) (serveOptions, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	workspace := fs.String("workspace", "", "workspace directory (defaults to cwd)")
	recover := fs.Bool("recover", false, "recover interrupted jobs on startup")
	recoverJobs := fs.String("recover-jobs", "", "comma-separated job IDs to recover on startup")
	if err := fs.Parse(args); err != nil {
		return serveOptions{}, err
	}
	jobIDs := splitCSV(*recoverJobs)
	return serveOptions{
		addr:      *addr,
		workspace: *workspace,
		recover: startupRecoverOptions{
			enabled: *recover || len(jobIDs) > 0,
			jobIDs:  jobIDs,
		},
	}, nil
}

func parseMCPOptions(args []string) (startupRecoverOptions, error) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	recover := fs.Bool("recover", false, "recover interrupted jobs on startup")
	recoverJobs := fs.String("recover-jobs", "", "comma-separated job IDs to recover on startup")
	if err := fs.Parse(args); err != nil {
		return startupRecoverOptions{}, err
	}
	jobIDs := splitCSV(*recoverJobs)
	return startupRecoverOptions{
		enabled: *recover || len(jobIDs) > 0,
		jobIDs:  jobIDs,
	}, nil
}

func applyStartupRecovery(service *orchestrator.Service, opts startupRecoverOptions) {
	if !opts.enabled {
		service.InterruptRecoverableJobs(nil)
		return
	}
	if len(opts.jobIDs) > 0 {
		service.InterruptRecoverableJobs(opts.jobIDs)
		service.RecoverSelectedJobs(opts.jobIDs)
		return
	}
	service.RecoverJobs()
}

func run(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	goal := fs.String("goal", "", "job goal")
	techStack := fs.String("tech-stack", "go", "tech stack label")
	constraints := fs.String("constraints", "", "comma-separated constraints")
	doneCriteria := fs.String("done", "", "comma-separated done criteria")
	providerName := fs.String("provider", string(domain.ProviderMock), "provider name")
	profilesFile := fs.String("profiles-file", "", "path to a role profile JSON file")
	workspaceMode := fs.String("workspace-mode", string(domain.WorkspaceModeShared), "workspace mode: shared | isolated")
	maxSteps := fs.Int("max-steps", 8, "maximum worker steps")
	strictness := fs.String("strictness", "normal", "evaluator strictness level: strict | normal | lenient")
	skipPlanning := fs.Bool("skip-planning", false, "skip planner LLM call; verification contract is built from -done criteria directly")
	skipLeader := fs.Bool("skip-leader", false, "skip leader LLM loop; orchestrator drives executor->evaluator retries directly (lightest pipeline when combined with -skip-planning)")
	maxEvalRetries := fs.Int("max-eval-retries", 0, "max evaluator retries in -skip-leader mode (default 3 when 0)")
	fs.Parse(args)

	if strings.TrimSpace(*goal) == "" {
		log.Fatal("run requires -goal")
	}

	roleProfiles, err := loadRoleProfiles(*profilesFile, domain.ProviderName(*providerName))
	if err != nil {
		log.Fatal(err)
	}

	job, err := service.Start(ctx, orchestrator.CreateJobInput{
		Goal:            *goal,
		TechStack:       *techStack,
		WorkspaceDir:    currentWorkspace(),
		WorkspaceMode:   *workspaceMode,
		Constraints:     splitCSV(*constraints),
		DoneCriteria:    splitCSV(*doneCriteria),
		Provider:        domain.ProviderName(*providerName),
		RoleProfiles:    roleProfiles,
		MaxSteps:        *maxSteps,
		StrictnessLevel: *strictness,
		SkipPlanning:    *skipPlanning,
		SkipLeader:      *skipLeader,
		MaxEvalRetries:  *maxEvalRetries,
	})
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func status(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	all := fs.Bool("all", false, "list all jobs")
	fs.Parse(args)

	if *all {
		jobs, err := service.List(ctx)
		if err != nil {
			log.Fatal(err)
		}
		printJSON(jobs)
		return
	}

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("status requires -job or -all")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func events(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("events requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job.Events)
}

func artifacts(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("artifacts", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("artifacts requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(flattenArtifacts(job.Steps))
}

func verification(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("verification", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("verification requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.BuildVerificationView(job))
}

func planning(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("planning", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("planning requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.BuildPlanningView(job))
}

func evaluator(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("evaluator", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("evaluator requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.BuildEvaluatorView(job))
}

func profile(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("profile", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("profile requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.BuildProfileView(job))
}

func resume(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("resume requires -job")
	}

	job, err := service.Resume(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func retry(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("retry requires -job")
	}

	job, err := service.Retry(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func approve(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("approve", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("approve requires -job")
	}

	job, err := service.Approve(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func cancel(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	reason := fs.String("reason", "", "operator cancellation reason")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("cancel requires -job")
	}

	job, err := service.Cancel(ctx, *jobID, *reason)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func reject(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("reject", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	reason := fs.String("reason", "", "operator rejection reason")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("reject requires -job")
	}

	job, err := service.Reject(ctx, *jobID, *reason)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func harnessStart(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("harness-start", flag.ExitOnError)
	jobID := fs.String("job", "", "job id for job-scoped harness ownership")
	name := fs.String("name", "", "process name")
	category := fs.String("category", string(runtimeexec.CategoryCommand), "process category")
	command := fs.String("command", "", "command to run")
	argList := fs.String("args", "", "comma-separated command args")
	dir := fs.String("dir", "", "working directory")
	envList := fs.String("env", "", "comma-separated env entries")
	timeoutSeconds := fs.Int("timeout-seconds", 0, "timeout in seconds")
	maxOutputBytes := fs.Int64("max-output-bytes", 0, "max output bytes")
	logDir := fs.String("log-dir", "", "log directory")
	port := fs.Int("port", 0, "port")
	fs.Parse(args)

	if strings.TrimSpace(*command) == "" {
		log.Fatal("harness-start requires -command")
	}

	request := runtimeexec.StartRequest{
		Request: runtimeexec.Request{
			Category:       runtimeexec.Category(strings.TrimSpace(*category)),
			Command:        *command,
			Args:           splitCSV(*argList),
			Dir:            *dir,
			Env:            splitCSV(*envList),
			Timeout:        time.Duration(*timeoutSeconds) * time.Second,
			MaxOutputBytes: *maxOutputBytes,
		},
		Name:   strings.TrimSpace(*name),
		LogDir: strings.TrimSpace(*logDir),
		Port:   *port,
	}
	var (
		job any
		err error
	)
	if strings.TrimSpace(*jobID) == "" {
		job, err = service.StartHarnessProcess(ctx, request)
	} else {
		job, err = service.StartJobHarnessProcess(ctx, strings.TrimSpace(*jobID), request)
	}
	if err != nil {
		log.Fatal(err)
	}
	printJSON(job)
}

func harnessView(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("harness-view", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("harness-view requires -job")
	}

	job, err := service.Get(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	processes, err := service.ListJobHarnessProcesses(ctx, *jobID)
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.BuildRuntimeHarnessView(job, processes))
}

func harnessList(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("harness-list", flag.ExitOnError)
	jobID := fs.String("job", "", "job id for job-scoped harness listing")
	fs.Parse(args)

	var (
		processes []runtimeexec.ProcessHandle
		err       error
	)
	if strings.TrimSpace(*jobID) == "" {
		processes, err = service.ListHarnessProcesses(ctx)
	} else {
		processes, err = service.ListJobHarnessProcesses(ctx, strings.TrimSpace(*jobID))
	}
	if err != nil {
		log.Fatal(err)
	}
	printJSON(api.RuntimeProcessListView{
		Processes: processes,
		Note:      "runtime processes are managed by the orchestrator service",
	})
}

func harnessStatus(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("harness-status", flag.ExitOnError)
	jobID := fs.String("job", "", "job id for job-scoped harness lookup")
	pid := fs.Int("pid", 0, "process id")
	fs.Parse(args)

	if *pid <= 0 {
		log.Fatal("harness-status requires -pid")
	}

	var (
		handle any
		err    error
	)
	if strings.TrimSpace(*jobID) == "" {
		handle, err = service.GetHarnessProcess(ctx, *pid)
	} else {
		handle, err = service.GetJobHarnessProcess(ctx, strings.TrimSpace(*jobID), *pid)
	}
	if err != nil {
		log.Fatal(err)
	}
	printJSON(handle)
}

func harnessStop(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("harness-stop", flag.ExitOnError)
	jobID := fs.String("job", "", "job id for job-scoped harness stop")
	pid := fs.Int("pid", 0, "process id")
	fs.Parse(args)

	if *pid <= 0 {
		log.Fatal("harness-stop requires -pid")
	}

	var (
		handle any
		err    error
	)
	if strings.TrimSpace(*jobID) == "" {
		handle, err = service.StopHarnessProcess(ctx, *pid)
	} else {
		handle, err = service.StopJobHarnessProcess(ctx, strings.TrimSpace(*jobID), *pid)
	}
	if err != nil {
		log.Fatal(err)
	}
	printJSON(handle)
}

func stream(args []string) {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	jobID := fs.String("job", "", "job id")
	serverURL := fs.String("server", "http://127.0.0.1:8080", "API server URL")
	fs.Parse(args)

	if strings.TrimSpace(*jobID) == "" {
		log.Fatal("stream requires -job")
	}

	// SSE stream: no overall Client.Timeout (long-lived connection).
	// Apply timeouts only for connection establishment and response header receipt.
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
	resp, err := sseClient.Get(strings.TrimRight(*serverURL, "/") + "/jobs/" + *jobID + "/events/stream")
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("stream request failed: %s", resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			fmt.Println(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
}

func serve(service *orchestrator.Service, args []string) {
	opts, err := parseServeOptions(args)
	if err != nil {
		log.Fatal(err)
	}

	workspace := opts.workspace
	if workspace != "" {
		service = buildService(workspace)
	} else {
		workspace, _ = os.Getwd()
	}
	applyStartupRecovery(service, opts.recover)

	shutdownCh := make(chan struct{}, 1)

	// swappable handler for runtime workspace switch
	handler := &swappableHandler{current: api.NewServer(service).Handler()}
	var svcMu sync.Mutex
	currentService := service

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})
	mux.Handle("/", handler)
	mux.HandleFunc("POST /admin/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"shutting_down"}`)
		select {
		case shutdownCh <- struct{}{}:
		default:
		}
	})
	mux.HandleFunc("POST /admin/workspace", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Workspace == "" {
			http.Error(w, `{"error":"workspace required"}`, http.StatusBadRequest)
			return
		}
		info, statErr := os.Stat(req.Workspace)
		if statErr != nil || !info.IsDir() {
			http.Error(w, `{"error":"workspace directory not found"}`, http.StatusBadRequest)
			return
		}

		svcMu.Lock()
		newSvc := buildService(req.Workspace)
		oldSvc := currentService
		currentService = newSvc
		handler.swap(api.NewServer(newSvc).Handler())
		svcMu.Unlock()

		oldSvc.Shutdown()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"status\":\"switched\",\"workspace\":%q}\n", req.Workspace)
		log.Printf("workspace switched to %s", req.Workspace)
	})
	mux.HandleFunc("GET /admin/workspace", func(w http.ResponseWriter, r *http.Request) {
		svcMu.Lock()
		ws := currentService.WorkspaceRoot()
		svcMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"workspace\":%q}\n", ws)
	})

	srv := &http.Server{Addr: opts.addr, Handler: mux}

	// PID file
	pidFile := filepath.Join(workspace, ".gorchera", "serve.pid")
	writePIDFile(pidFile, os.Getpid(), opts.addr)
	defer removePIDFile(pidFile)

	// graceful shutdown on signal or admin request
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	go func() {
		select {
		case <-sigCh:
		case <-shutdownCh:
		}
		log.Println("gorchera serve shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	fmt.Printf("gorchera API listening on %s (workspace: %s)\n", opts.addr, workspace)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal(err)
	}

	svcMu.Lock()
	currentService.Shutdown()
	svcMu.Unlock()
}

func runMCP(service *orchestrator.Service, args []string) {
	recoverOpts, err := parseMCPOptions(args)
	if err != nil {
		log.Fatal(err)
	}
	applyStartupRecovery(service, recoverOpts)
	defer service.Shutdown()

	mcpServer := mcp.NewServer(service)
	mcpServer.RegisterTerminalCallback()
	if err := mcpServer.Run(); err != nil {
		log.Fatal(err)
	}
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func printJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
}

func usage() {
	fmt.Println("gorchera <run|status|events|artifacts|verification|planning|evaluator|profile|resume|approve|retry|cancel|reject|harness-start|harness-view|harness-list|harness-status|harness-stop|stream|serve|stop|mcp> [flags]")
	fmt.Println("  serve [-workspace DIR] [-addr HOST:PORT] -- API server with graceful shutdown")
	fmt.Println("  stop [-workspace DIR | -addr HOST:PORT] -- graceful shutdown of running serve")
	fmt.Println("  serve/mcp block stale interrupted jobs by default; use -recover or -recover-jobs job1,job2 to resume explicitly.")
	fmt.Println("  run accepts -workspace-mode shared|isolated; isolated creates a detached git worktree for the job.")
	fmt.Println("  run -skip-planning: skip planner LLM; use -done criteria as verification contract directly.")
	fmt.Println("  run -skip-leader:   skip leader LLM loop; orchestrator retries executor->evaluator up to -max-eval-retries (default 3).")
	fmt.Println("  run -skip-planning -skip-leader: lightest pipeline -- executor + evaluator only, no director LLM calls.")
}

func loadRoleProfiles(path string, base domain.ProviderName) (domain.RoleProfiles, error) {
	if strings.TrimSpace(path) == "" {
		return domain.DefaultRoleProfiles(base), nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return domain.RoleProfiles{}, fmt.Errorf("failed to read role profile file %q: %w", path, err)
	}

	var profiles domain.RoleProfiles
	if err := json.Unmarshal(data, &profiles); err != nil {
		return domain.RoleProfiles{}, fmt.Errorf("failed to parse role profile file %q: %w", path, err)
	}
	return profiles.Normalize(base), nil
}

func currentWorkspace() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func flattenArtifacts(steps []domain.Step) []string {
	var out []string
	for _, step := range steps {
		out = append(out, step.Artifacts...)
	}
	return out
}

func buildService(workspace string) *orchestrator.Service {
	rootDir := filepath.Join(workspace, ".gorchera")
	stateStore := store.NewStateStore(filepath.Join(rootDir, "state"))
	artifactStore := store.NewArtifactStore(filepath.Join(rootDir, "artifacts"))
	registry := provider.NewRegistry()
	registry.Register(mock.New())
	sessionManager := provider.NewSessionManager(registry)
	return orchestrator.NewService(sessionManager, stateStore, artifactStore, workspace)
}

// swappableHandler allows hot-swapping the underlying http.Handler (for workspace switch).
type swappableHandler struct {
	mu      sync.RWMutex
	current http.Handler
}

func (h *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	handler := h.current
	h.mu.RUnlock()
	handler.ServeHTTP(w, r)
}

func (h *swappableHandler) swap(next http.Handler) {
	h.mu.Lock()
	h.current = next
	h.mu.Unlock()
}

type pidInfo struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func writePIDFile(path string, pid int, addr string) {
	os.MkdirAll(filepath.Dir(path), 0o755)
	data, _ := json.Marshal(pidInfo{PID: pid, Addr: addr})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("warning: failed to write pid file: %v", err)
	}
}

func removePIDFile(path string) {
	os.Remove(path)
}

func stop(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	workspace := fs.String("workspace", "", "workspace directory (defaults to cwd)")
	addr := fs.String("addr", "", "serve address (overrides pid file lookup)")
	fs.Parse(args)

	if *addr != "" {
		sendShutdown(*addr)
		return
	}

	ws := *workspace
	if ws == "" {
		ws, _ = os.Getwd()
	}

	pidFile := filepath.Join(ws, ".gorchera", "serve.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		log.Fatalf("no running serve found (pid file: %s): %v", pidFile, err)
	}

	var info pidInfo
	if err := json.Unmarshal(data, &info); err != nil {
		log.Fatalf("corrupt pid file: %v", err)
	}

	sendShutdown(info.Addr)
	fmt.Printf("shutdown requested (pid %d, addr %s)\n", info.PID, info.Addr)
}

func sendShutdown(addr string) {
	resp, err := http.Post("http://"+addr+"/admin/shutdown", "application/json", nil)
	if err != nil {
		log.Fatalf("failed to contact serve at %s: %v", addr, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("shutdown request failed: %s", resp.Status)
	}
}
