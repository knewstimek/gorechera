package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"gorechera/internal/domain"
	"gorechera/internal/orchestrator"
)

// Server is an MCP stdio server that exposes Gorechera as a set of tools.
// It reads newline-delimited JSON-RPC 2.0 requests from stdin and writes
// responses to stdout. stderr is left untouched so the host process can
// capture log output separately.
//
// All stdout writes go through writeMessage, which holds mu, so that the
// response goroutine (stdin loop) and the notification goroutine (listenEvents)
// never interleave their output lines.
type Server struct {
	service *orchestrator.Service
	mu      sync.Mutex
	writer  io.Writer
}

func NewServer(service *orchestrator.Service) *Server {
	return &Server{service: service}
}

// writeMessage serialises msg to JSON and writes a single newline-terminated
// line to s.writer under the stdout mutex. Must only be called after Run()
// has initialised s.writer.
func (s *Server) writeMessage(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		// Should never happen with well-formed structs.
		fmt.Fprintf(os.Stderr, "mcp: marshal error: %v\n", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.writer, string(data))
}

// listenEvents reads from the service event channel and forwards each event
// as an MCP notifications/message notification to the connected client.
// It runs in its own goroutine for the lifetime of Run().
func (s *Server) listenEvents() {
	for event := range s.service.EventChan() {
		notification := map[string]any{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params": map[string]any{
				"level":  "info",
				"logger": "gorechera",
				"data": map[string]string{
					"job_id":  event.JobID,
					"kind":    event.Kind,
					"message": event.Message,
				},
			},
		}
		s.writeMessage(notification)
	}
}

func (s *Server) Run() error {
	s.writer = os.Stdout

	// Start notification relay before processing any requests so that events
	// emitted by background jobs started earlier are not missed.
	go s.listenEvents()

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		resp := s.handleMessage([]byte(line))
		if resp != nil {
			s.writeMessage(resp)
		}
	}
	return scanner.Err()
}

// ---- JSON-RPC types --------------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---- MCP schema types ------------------------------------------------------

type toolInputSchema struct {
	Type       string                `json:"type"`
	Properties map[string]schemaProp `json:"properties,omitempty"`
	Required   []string              `json:"required,omitempty"`
}

type schemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Default     any    `json:"default,omitempty"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema toolInputSchema `json:"inputSchema"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []contentItem `json:"content"`
}

// ---- Routing ---------------------------------------------------------------

func (s *Server) handleMessage(raw []byte) *jsonRPCResponse {
	var req jsonRPCRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResp(nil, -32700, "parse error")
	}

	// Notifications have no id and require no response.
	if req.ID == nil && req.Method == "notifications/initialized" {
		return nil
	}
	if req.ID == nil && strings.HasPrefix(req.Method, "notifications/") {
		return nil
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized":
		// Notification variant (client sends without id); no response needed.
		return nil
	case "tools/list":
		return okResp(req.ID, map[string]any{"tools": toolList()})
	case "tools/call":
		return s.handleToolCall(req)
	default:
		return errorResp(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(req jsonRPCRequest) *jsonRPCResponse {
	result := map[string]any{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]string{
			"name":    "gorechera",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
			// logging capability signals that this server will emit
			// notifications/message frames for job state changes.
			"logging": map[string]any{},
		},
	}
	return okResp(req.ID, result)
}

// ---- Tool list -------------------------------------------------------------

func toolList() []toolDef {
	return []toolDef{
		{
			Name:        "gorechera_start_job",
			Description: "Start a new Gorechera job. The job runs asynchronously; use gorechera_status to poll progress.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"goal":             {Type: "string", Description: "Natural-language goal for the job (required)"},
					"provider":         {Type: "string", Description: "Provider name: mock | codex | claude", Default: "claude"},
					"workspace_dir":    {Type: "string", Description: "Absolute path of the workspace directory"},
					"max_steps":        {Type: "integer", Description: "Maximum leader steps", Default: 8},
					"strictness_level": {Type: "string", Description: "Evaluator strictness: strict | normal | lenient", Default: "normal"},
					"context_mode":     {Type: "string", Description: "Leader context mode: full | summary | minimal. full=entire job state, summary=recent steps+compressed history, minimal=last step+counts only", Default: "full"},
				},
				Required: []string{"goal"},
			},
		},
		{
			Name:        "gorechera_list_jobs",
			Description: "List all jobs.",
			InputSchema: toolInputSchema{Type: "object"},
		},
		{
			Name:        "gorechera_status",
			Description: "Get the current status of a job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_events",
			Description: "Get recent events for a job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
					"last_n": {Type: "integer", Description: "Number of most recent events to return", Default: 10},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_artifacts",
			Description: "Get artifact paths produced by a job (planning artifacts + step artifacts).",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_approve",
			Description: "Approve a pending approval on a job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_reject",
			Description: "Reject a pending approval on a job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
					"reason": {Type: "string", Description: "Rejection reason"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_retry",
			Description: "Retry a blocked or failed job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_cancel",
			Description: "Cancel a job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
					"reason": {Type: "string", Description: "Cancellation reason"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_resume",
			Description: "Resume a blocked job.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id": {Type: "string", Description: "Job ID"},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorechera_steer",
			Description: "Inject a supervisor directive into a running job. The next leader call will see this directive with highest priority.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id":  {Type: "string", Description: "Job ID"},
					"message": {Type: "string", Description: "Supervisor directive for the leader"},
				},
				Required: []string{"job_id", "message"},
			},
		},
	}
}

// ---- Tool dispatch ---------------------------------------------------------

func (s *Server) handleToolCall(req jsonRPCRequest) *jsonRPCResponse {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return errorResp(req.ID, -32602, "invalid params")
	}

	// args is a loose map; individual handlers pick what they need.
	var args map[string]any
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return errorResp(req.ID, -32602, "invalid arguments")
		}
	}
	if args == nil {
		args = map[string]any{}
	}

	ctx := context.Background()
	var (
		result any
		err    error
	)

	switch p.Name {
	case "gorechera_start_job":
		result, err = s.toolStartJob(ctx, args)
	case "gorechera_list_jobs":
		result, err = s.toolListJobs(ctx)
	case "gorechera_status":
		result, err = s.toolStatus(ctx, args)
	case "gorechera_events":
		result, err = s.toolEvents(ctx, args)
	case "gorechera_artifacts":
		result, err = s.toolArtifacts(ctx, args)
	case "gorechera_approve":
		result, err = s.toolApprove(ctx, args)
	case "gorechera_reject":
		result, err = s.toolReject(ctx, args)
	case "gorechera_retry":
		result, err = s.toolRetry(ctx, args)
	case "gorechera_cancel":
		result, err = s.toolCancel(ctx, args)
	case "gorechera_resume":
		result, err = s.toolResume(ctx, args)
	case "gorechera_steer":
		result, err = s.toolSteer(ctx, args)
	default:
		return errorResp(req.ID, -32602, fmt.Sprintf("unknown tool: %s", p.Name))
	}

	if err != nil {
		return okResp(req.ID, textResult(fmt.Sprintf("error: %s", err.Error())))
	}
	return okResp(req.ID, result)
}

// ---- Tool implementations --------------------------------------------------

func (s *Server) toolStartJob(ctx context.Context, args map[string]any) (toolResult, error) {
	goal := stringArg(args, "goal")
	if strings.TrimSpace(goal) == "" {
		return toolResult{}, fmt.Errorf("goal is required")
	}
	provider := domain.ProviderName(stringArgDefault(args, "provider", "claude"))
	workspaceDir := stringArg(args, "workspace_dir")
	maxSteps := intArgDefault(args, "max_steps", 8)
	strictnessLevel := stringArgDefault(args, "strictness_level", "normal")
	contextMode := stringArgDefault(args, "context_mode", "full")
	if err := orchestrator.ValidateWorkspaceDir(workspaceDir); err != nil {
		return toolResult{}, err
	}

	input := orchestrator.CreateJobInput{
		Goal:            goal,
		Provider:        provider,
		WorkspaceDir:    workspaceDir,
		MaxSteps:        maxSteps,
		StrictnessLevel: strictnessLevel,
		ContextMode:     contextMode,
		RoleProfiles:    domain.DefaultRoleProfiles(provider),
	}

	// StartAsync creates the job synchronously and runs the main loop in a
	// background goroutine. This is necessary because runLoop is blocking and
	// we need to return the job ID immediately over the MCP channel.
	job, err := s.service.StartAsync(ctx, input)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolListJobs(ctx context.Context) (toolResult, error) {
	jobs, err := s.service.List(ctx)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(jobs)
}

func (s *Server) toolStatus(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolEvents(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	lastN := intArgDefault(args, "last_n", 10)

	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}

	events := job.Events
	if lastN > 0 && len(events) > lastN {
		events = events[len(events)-lastN:]
	}
	return jsonResult(events)
}

func (s *Server) toolArtifacts(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}

	var all []string
	all = append(all, job.PlanningArtifacts...)
	for _, step := range job.Steps {
		all = append(all, step.Artifacts...)
	}
	return jsonResult(all)
}

func (s *Server) toolApprove(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	// Approve re-enters runLoop, which blocks until the job pauses again.
	// Run in a goroutine and return the pre-approval snapshot immediately so
	// the MCP caller is not blocked indefinitely.
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}
	go func() {
		s.service.Approve(context.Background(), jobID) //nolint:errcheck
	}()
	snapshot := map[string]any{
		"job_id":  job.ID,
		"status":  job.Status,
		"message": "approval submitted; job is resuming in background",
	}
	return jsonResult(snapshot)
}

func (s *Server) toolReject(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	reason := stringArg(args, "reason")
	job, err := s.service.Reject(ctx, jobID, reason)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolRetry(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	// Retry also calls runLoop; run in background for the same reason as Approve.
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}
	go func() {
		s.service.Retry(context.Background(), jobID) //nolint:errcheck
	}()
	snapshot := map[string]any{
		"job_id":  job.ID,
		"status":  job.Status,
		"message": "retry submitted; job is resuming in background",
	}
	return jsonResult(snapshot)
}

func (s *Server) toolCancel(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	reason := stringArg(args, "reason")
	job, err := s.service.Cancel(ctx, jobID, reason)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolResume(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	// Resume calls runLoop; run in background.
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}
	go func() {
		s.service.Resume(context.Background(), jobID) //nolint:errcheck
	}()
	snapshot := map[string]any{
		"job_id":  job.ID,
		"status":  job.Status,
		"message": "resume submitted; job is resuming in background",
	}
	return jsonResult(snapshot)
}

// ---- Helpers ---------------------------------------------------------------

func okResp(id any, result any) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errorResp(id any, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	}
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentItem{{Type: "text", Text: text}}}
}

func jsonResult(v any) (toolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolResult{}, fmt.Errorf("failed to serialize result: %w", err)
	}
	return textResult(string(data)), nil
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func stringArgDefault(args map[string]any, key, def string) string {
	v := stringArg(args, key)
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func requireStringArg(args map[string]any, key string) (string, error) {
	v := stringArg(args, key)
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}

func intArgDefault(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		n := int(v)
		if n <= 0 {
			return def
		}
		return n
	case int:
		if v <= 0 {
			return def
		}
		return v
	}
	return def
}

func (s *Server) toolSteer(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID := stringArg(args, "job_id")
	message := stringArg(args, "message")
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(message) == "" {
		return toolResult{}, fmt.Errorf("job_id and message are required")
	}
	job, err := s.service.Steer(ctx, jobID, message)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(map[string]any{
		"status":                 "steered",
		"leader_context_summary": job.LeaderContextSummary,
		"supervisor_directive":   job.SupervisorDirective,
	})
}
