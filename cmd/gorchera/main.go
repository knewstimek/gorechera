package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	rootDir := filepath.Join(".", ".gorchera")
	workspaceRoot, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	stateStore := store.NewStateStore(filepath.Join(rootDir, "state"))
	artifactStore := store.NewArtifactStore(filepath.Join(rootDir, "artifacts"))

	registry := provider.NewRegistry()
	registry.Register(mock.New())

	sessionManager := provider.NewSessionManager(registry)
	service := orchestrator.NewService(sessionManager, stateStore, artifactStore, workspaceRoot)

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
	case "mcp":
		mcpServer := mcp.NewServer(service)
		if err := mcpServer.Run(); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func run(ctx context.Context, service *orchestrator.Service, args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	goal := fs.String("goal", "", "job goal")
	techStack := fs.String("tech-stack", "go", "tech stack label")
	constraints := fs.String("constraints", "", "comma-separated constraints")
	doneCriteria := fs.String("done", "", "comma-separated done criteria")
	providerName := fs.String("provider", string(domain.ProviderMock), "provider name")
	profilesFile := fs.String("profiles-file", "", "path to a role profile JSON file")
	maxSteps := fs.Int("max-steps", 8, "maximum worker steps")
	strictness := fs.String("strictness", "normal", "evaluator strictness level: strict | normal | lenient")
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
		Constraints:     splitCSV(*constraints),
		DoneCriteria:    splitCSV(*doneCriteria),
		Provider:        domain.ProviderName(*providerName),
		RoleProfiles:    roleProfiles,
		MaxSteps:        *maxSteps,
		StrictnessLevel: *strictness,
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
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address")
	fs.Parse(args)

	server := api.NewServer(service)
	fmt.Printf("gorchera API listening on %s\n", *addr)
	log.Fatal(http.ListenAndServe(*addr, server.Handler()))
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
	fmt.Println("gorchera <run|status|events|artifacts|verification|planning|evaluator|profile|resume|approve|retry|cancel|reject|harness-start|harness-view|harness-list|harness-status|harness-stop|stream|serve|mcp> [flags]")
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
