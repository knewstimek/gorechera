package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorchera/internal/domain"
	"gorchera/internal/orchestrator"
)

// Server is an MCP stdio server that exposes Gorchera as a set of tools.
// It reads newline-delimited JSON-RPC 2.0 requests from stdin and writes
// responses to stdout. stderr is left untouched so the host process can
// capture log output separately.
//
// All stdout writes go through writeMessage, which holds mu, so that the
// response goroutine (stdin loop) and the notification goroutine (listenEvents)
// never interleave their output lines.
type Server struct {
	service      *orchestrator.Service
	mu           sync.Mutex
	writer       io.Writer
	pendingLines [][]byte

	// done is closed by Run() when the stdin loop exits, signalling listenEvents
	// to stop so the goroutine does not leak after the server shuts down.
	done chan struct{}

	terminalMu   sync.Mutex
	lastTerminal map[string]string
}

var (
	statusWaitPollInterval = 2 * time.Second
	statusWaitTimeout      = 5 * time.Minute
	statusWaitDefault      = 30 * time.Second

	terminalNotificationPollInterval = 25 * time.Millisecond
	terminalNotificationTimeout      = 3 * time.Second
)

func NewServer(service *orchestrator.Service) *Server {
	return &Server{
		service:      service,
		done:         make(chan struct{}),
		lastTerminal: make(map[string]string),
	}
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
	s.writeRawLine(data)
}

func (s *Server) writeRawLine(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		s.pendingLines = append(s.pendingLines, append([]byte(nil), data...))
		return
	}
	if _, err := s.writer.Write(append(append([]byte(nil), data...), '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "mcp: write error: %v\n", err)
	}
}

func (s *Server) installWriter(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.writer = w
	if s.writer == nil {
		return
	}
	for _, line := range s.pendingLines {
		if _, err := s.writer.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			fmt.Fprintf(os.Stderr, "mcp: write error: %v\n", err)
			break
		}
	}
	s.pendingLines = nil
}

func (s *Server) sendNotification(method string, params any) {
	s.writeMessage(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// listenEvents reads from the service event channel and forwards each event
// as an MCP notifications/message notification to the connected client.
// It runs in its own goroutine for the lifetime of Run().
// When Run() exits it closes s.done, causing this goroutine to exit cleanly
// instead of leaking on a channel that is never closed.
func (s *Server) listenEvents() {
	for {
		select {
		case <-s.done:
			return
		case event, ok := <-s.service.EventChan():
			if !ok {
				// Channel closed by the service itself -- nothing more to relay.
				return
			}
			s.handleEventNotification(event)
		}
	}
}

func (s *Server) Run() error {
	s.installWriter(os.Stdout)

	// Start notification relay before processing any requests so that events
	// emitted by background jobs started earlier are not missed.
	go s.listenEvents()
	// Signal listenEvents to exit when Run() returns, preventing goroutine leak.
	defer close(s.done)

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

func (s *Server) RegisterTerminalCallback() {
	rv := reflect.ValueOf(s.service)
	method := rv.MethodByName("SetJobTerminalCallback")
	if !method.IsValid() {
		return
	}
	methodType := method.Type()
	if methodType.NumIn() != 1 || methodType.NumOut() != 0 || methodType.In(0).Kind() != reflect.Func {
		return
	}
	callbackType := methodType.In(0)
	callback := reflect.MakeFunc(callbackType, func(args []reflect.Value) []reflect.Value {
		go func(values []reflect.Value) {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[gorchera] terminal callback panic recovered: %v", r)
				}
			}()
			s.handleTerminalCallbackValues(values)
		}(append([]reflect.Value(nil), args...))
		return nil
	})
	method.Call([]reflect.Value{callback})
}

func (s *Server) handleEventNotification(event orchestrator.EventNotification) {
	s.sendNotification("notifications/message", map[string]any{
		"level":  "info",
		"logger": "gorchera",
		"data": map[string]string{
			"job_id":  event.JobID,
			"kind":    event.Kind,
			"message": event.Message,
		},
	})

	if clearsTerminalNotificationState(event.Kind) {
		s.clearTerminalNotificationState(event.JobID)
	}
	if !mightProduceTerminalState(event.Kind) {
		return
	}
	go s.awaitAndSendTerminalNotification(event.JobID)
}

func mightProduceTerminalState(kind string) bool {
	switch kind {
	case "job_blocked", "job_failed", "job_completed", "job_cancelled", "job_interrupted":
		return true
	default:
		return false
	}
}

func clearsTerminalNotificationState(kind string) bool {
	switch kind {
	case "job_created", "job_resumed", "job_retry_requested", "job_approved":
		return true
	default:
		return false
	}
}

func (s *Server) clearTerminalNotificationState(jobID string) {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	delete(s.lastTerminal, jobID)
}

func (s *Server) handleTerminalCallbackValues(args []reflect.Value) {
	switch len(args) {
	case 1:
		s.handleSingleTerminalCallbackArg(args[0])
	case 3:
		jobID := valueAsString(args[0])
		status := terminalStatusFromValue(args[1])
		summary := valueAsString(args[2])
		if strings.TrimSpace(jobID) == "" || !isTerminalJobStatus(status) {
			return
		}
		// No job object available here; worktree info is not included.
		s.sendJobTerminalNotification(jobID, status, summary, nil)
	}
}

func (s *Server) handleSingleTerminalCallbackArg(arg reflect.Value) {
	if !arg.IsValid() {
		return
	}
	switch v := arg.Interface().(type) {
	case domain.Job:
		if isTerminalJobStatus(v.Status) {
			s.sendJobTerminalNotification(v.ID, v.Status, terminalSummary(&v), isolatedWorktreeExtra(&v))
		}
	case *domain.Job:
		if v != nil && isTerminalJobStatus(v.Status) {
			s.sendJobTerminalNotification(v.ID, v.Status, terminalSummary(v), isolatedWorktreeExtra(v))
		}
	}
}

func valueAsString(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if v.Kind() == reflect.String {
		return v.String()
	}
	if status, ok := v.Interface().(domain.JobStatus); ok {
		return string(status)
	}
	return fmt.Sprint(v.Interface())
}

func terminalStatusFromValue(v reflect.Value) domain.JobStatus {
	if !v.IsValid() {
		return ""
	}
	if status, ok := v.Interface().(domain.JobStatus); ok {
		return status
	}
	if v.Kind() == reflect.String {
		return domain.JobStatus(v.String())
	}
	return domain.JobStatus(fmt.Sprint(v.Interface()))
}

func (s *Server) awaitAndSendTerminalNotification(jobID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[gorchera] terminal notification panic recovered for job %s: %v", jobID, r)
		}
	}()

	deadline := time.Now().Add(terminalNotificationTimeout)
	for {
		job, err := s.service.Get(context.Background(), jobID)
		if err == nil && job != nil && isTerminalJobStatus(job.Status) {
			s.sendJobTerminalNotification(job.ID, job.Status, terminalSummary(job), isolatedWorktreeExtra(job))
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(terminalNotificationPollInterval)
	}
}

// sendJobTerminalNotification sends a job_terminal notification to the MCP client.
// extra holds optional fields (e.g. workspace_mode, workspace_dir, diff_stat) that are
// merged into the payload only when present -- callers pass nil for shared-workspace jobs.
func (s *Server) sendJobTerminalNotification(jobID string, status domain.JobStatus, summary string, extra map[string]any) {
	signature := string(status) + "\x00" + summary

	s.terminalMu.Lock()
	if prev := s.lastTerminal[jobID]; prev == signature {
		s.terminalMu.Unlock()
		return
	}
	s.lastTerminal[jobID] = signature
	s.terminalMu.Unlock()

	payload := map[string]any{
		"job_id":  jobID,
		"status":  status,
		"summary": summary,
	}
	for k, v := range extra {
		payload[k] = v
	}
	s.sendNotification("notifications/job_terminal", payload)
}

func terminalSummary(job *domain.Job) string {
	if job == nil {
		return ""
	}
	if strings.TrimSpace(job.Summary) != "" {
		return job.Summary
	}
	if strings.TrimSpace(job.BlockedReason) != "" {
		return job.BlockedReason
	}
	if strings.TrimSpace(job.FailureReason) != "" {
		return job.FailureReason
	}
	return ""
}

// isolatedWorktreeExtra returns extra payload fields for jobs that ran in an
// isolated worktree, so the supervisor can decide whether to merge or discard
// the changes. Returns nil for shared-workspace jobs (no extra fields emitted).
func isolatedWorktreeExtra(job *domain.Job) map[string]any {
	if job == nil || job.WorkspaceMode != string(domain.WorkspaceModeIsolated) {
		return nil
	}

	extra := map[string]any{
		"workspace_mode": job.WorkspaceMode,
		"workspace_dir":  job.WorkspaceDir,
	}
	if job.RequestedWorkspaceDir != "" {
		extra["requested_workspace_dir"] = job.RequestedWorkspaceDir
	}

	// Collect a short diff summary so the supervisor can gauge the scope of
	// changes without having to inspect the worktree manually. Failure is
	// non-fatal -- we still send the notification without diff_stat.
	if job.WorkspaceDir != "" {
		out, err := exec.Command("git", "-C", job.WorkspaceDir, "diff", "--stat").Output()
		if err == nil {
			stat := strings.TrimSpace(string(out))
			if stat != "" {
				extra["diff_stat"] = stat
			}
		}
	}

	return extra
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
	Type        string                `json:"type"`
	Description string                `json:"description,omitempty"`
	Default     any                   `json:"default,omitempty"`
	Enum        []string              `json:"enum,omitempty"`
	Minimum     *int                  `json:"minimum,omitempty"`
	Maximum     *int                  `json:"maximum,omitempty"`
	Properties  map[string]schemaProp `json:"properties,omitempty"`
	Items       *schemaProp           `json:"items,omitempty"`
	Required    []string              `json:"required,omitempty"`
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
			"name":    "gorchera",
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
			Name:        "gorchera_start_job",
			Description: "Start a new Gorchera job. Pipeline: director -> executor -> [engine build/test] -> reviewer(balanced/full) -> evaluator. Use pipeline_mode to control overhead.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"goal":             {Type: "string", Description: "Natural-language goal for the job (required)"},
					"provider":         {Type: "string", Description: "Provider name: mock | codex | claude", Default: "claude"},
					"workspace_dir":    {Type: "string", Description: "Absolute path of the workspace directory"},
					"workspace_mode":   {Type: "string", Description: "Workspace mode: shared | isolated. isolated creates a detached git worktree rooted at HEAD for the job.", Default: "shared"},
					"max_steps":        {Type: "integer", Description: "Maximum leader steps", Default: 8},
					"pipeline_mode":    {Type: "string", Description: "Pipeline mode: light (default, skip reviewer) | balanced (add reviewer) | full (fix loops + parallel workers)", Default: "light", Enum: []string{"light", "balanced", "full"}},
					"strictness_level": {Type: "string", Description: "Evaluator strictness: strict | normal | lenient", Default: "normal"},
					"ambition_level":   {Type: "string", Description: "Worker autonomy scope: low | medium | high", Default: "medium"},
					"context_mode":     {Type: "string", Description: "Leader context mode: full | summary | minimal | auto. full=entire job state, summary=recent steps+compressed history, minimal=last step+counts only, auto=auto selects based on step count", Default: "full"},
					"role_overrides":   roleOverridesSchema(),
				},
				Required: []string{"goal"},
			},
		},
		{
			Name:        "gorchera_start_chain",
			Description: "Start a sequential chain of jobs. The first goal starts immediately and later goals start only after the previous job finishes successfully.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"workspace_dir": {Type: "string", Description: "Absolute path of the workspace directory"},
					"goals": {
						Type:        "array",
						Description: "Sequential goals to execute",
						Items: &schemaProp{
							Type: "object",
							Properties: map[string]schemaProp{
								"goal":             {Type: "string", Description: "Natural-language goal for this chain step"},
								"provider":         {Type: "string", Description: "Provider name: mock | codex | claude"},
								"strictness_level": {Type: "string", Description: "Evaluator strictness: strict | normal | lenient", Default: "normal"},
								"ambition_level":   {Type: "string", Description: "Worker autonomy scope: low | medium | high", Default: "medium"},
								"context_mode":     {Type: "string", Description: "Leader context mode: full | summary | minimal | auto (auto selects based on step count)", Default: "full"},
								"max_steps":        {Type: "integer", Description: "Maximum leader steps for this goal", Default: 8},
								"role_overrides":   roleOverridesSchema(),
							},
							Required: []string{"goal"},
						},
					},
				},
				Required: []string{"goals", "workspace_dir"},
			},
		},
		{
			Name:        "gorchera_list_jobs",
			Description: "List all jobs.",
			InputSchema: toolInputSchema{Type: "object"},
		},
		{
			Name:        "gorchera_status",
			Description: "Get job status. wait=true blocks until terminal state (default 30s, wait_timeout=0 for 5min). For long-running jobs, prefer periodic polling over blocking to stay responsive.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id":       {Type: "string", Description: "Job ID"},
					"wait":         {Type: "boolean", Description: "When true, wait for the job to reach a terminal state before returning", Default: false},
					"wait_timeout": {Type: "integer", Description: "Optional wait timeout in seconds when wait=true. Omit to use 30 seconds. Set to 0 to preserve the original 5-minute timeout.", Default: 30},
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorchera_chain_status",
			Description: "Get chain status. wait=true blocks until terminal state (default 30s, wait_timeout=0 for 5min). For long-running chains, prefer periodic polling over blocking to stay responsive.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"chain_id":     {Type: "string", Description: "Chain ID"},
					"wait":         {Type: "boolean", Description: "When true, wait for the chain to reach a terminal state before returning", Default: false},
					"wait_timeout": {Type: "integer", Description: "Optional wait timeout in seconds when wait=true. Omit to use 30 seconds. Set to 0 to preserve the original 5-minute timeout.", Default: 30},
				},
				Required: []string{"chain_id"},
			},
		},
		{
			Name:        "gorchera_pause_chain",
			Description: "Pause a sequential job chain after the current goal completes.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"chain_id": {Type: "string", Description: "Chain ID"},
				},
				Required: []string{"chain_id"},
			},
		},
		{
			Name:        "gorchera_resume_chain",
			Description: "Resume a paused sequential job chain.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"chain_id": {Type: "string", Description: "Chain ID"},
				},
				Required: []string{"chain_id"},
			},
		},
		{
			Name:        "gorchera_cancel_chain",
			Description: "Cancel a sequential job chain.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"chain_id": {Type: "string", Description: "Chain ID"},
					"reason":   {Type: "string", Description: "Cancellation reason"},
				},
				Required: []string{"chain_id"},
			},
		},
		{
			Name:        "gorchera_skip_chain_goal",
			Description: "Skip the current goal in a sequential job chain and advance to the next goal.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"chain_id": {Type: "string", Description: "Chain ID"},
				},
				Required: []string{"chain_id"},
			},
		},
		{
			Name:        "gorchera_events",
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
			Name:        "gorchera_artifacts",
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
			Name:        "gorchera_approve",
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
			Name:        "gorchera_reject",
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
			Name:        "gorchera_retry",
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
			Name:        "gorchera_cancel",
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
			Name:        "gorchera_resume",
			Description: "Resume a specific recoverable job. Intended for blocked or interrupted non-terminal jobs; failed jobs should use gorchera_retry.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id":      {Type: "string", Description: "Job ID"},
					"extra_steps": boundedIntegerSchema("Optional max_steps extension for blocked max_steps_exceeded resumes.", 1, 20),
				},
				Required: []string{"job_id"},
			},
		},
		{
			Name:        "gorchera_steer",
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
		{
			Name:        "gorchera_diff",
			Description: "Show the current git diff for a job workspace using the persisted workspace_dir.",
			InputSchema: toolInputSchema{
				Type: "object",
				Properties: map[string]schemaProp{
					"job_id":   {Type: "string", Description: "Job ID"},
					"pathspec": {Type: "string", Description: "Optional git pathspec to restrict the diff to a file or path"},
				},
				Required: []string{"job_id"},
			},
		},
	}
}

func boundedIntegerSchema(description string, minimum, maximum int) schemaProp {
	return schemaProp{
		Type:        "integer",
		Description: description,
		Minimum:     intPtr(minimum),
		Maximum:     intPtr(maximum),
	}
}

func roleOverridesSchema() schemaProp {
	return schemaProp{
		Type:        "object",
		Description: "Optional map of role name to {provider, model} overrides. Supports director-era roles and legacy planner/leader/tester compatibility during migration.",
		Properties: map[string]schemaProp{
			"director":  roleOverrideProfileSchema(),
			"planner":   roleOverrideProfileSchema(),
			"leader":    roleOverrideProfileSchema(),
			"executor":  roleOverrideProfileSchema(),
			"reviewer":  roleOverrideProfileSchema(),
			"tester":    roleOverrideProfileSchema(),
			"evaluator": roleOverrideProfileSchema(),
		},
	}
}

func roleOverrideProfileSchema() schemaProp {
	return schemaProp{
		Type: "object",
		Properties: map[string]schemaProp{
			"provider": {Type: "string"},
			"model":    {Type: "string"},
		},
	}
}

func intPtr(v int) *int {
	return &v
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
	case "gorchera_start_job":
		result, err = s.toolStartJob(ctx, args)
	case "gorchera_start_chain":
		result, err = s.toolStartChain(ctx, args)
	case "gorchera_list_jobs":
		result, err = s.toolListJobs(ctx)
	case "gorchera_status":
		result, err = s.toolStatus(ctx, args)
	case "gorchera_chain_status":
		result, err = s.toolChainStatus(ctx, args)
	case "gorchera_pause_chain":
		result, err = s.toolPauseChain(ctx, args)
	case "gorchera_resume_chain":
		result, err = s.toolResumeChain(ctx, args)
	case "gorchera_cancel_chain":
		result, err = s.toolCancelChain(ctx, args)
	case "gorchera_skip_chain_goal":
		result, err = s.toolSkipChainGoal(ctx, args)
	case "gorchera_events":
		result, err = s.toolEvents(ctx, args)
	case "gorchera_artifacts":
		result, err = s.toolArtifacts(ctx, args)
	case "gorchera_approve":
		result, err = s.toolApprove(ctx, args)
	case "gorchera_reject":
		result, err = s.toolReject(ctx, args)
	case "gorchera_retry":
		result, err = s.toolRetry(ctx, args)
	case "gorchera_cancel":
		result, err = s.toolCancel(ctx, args)
	case "gorchera_resume":
		result, err = s.toolResume(ctx, args)
	case "gorchera_steer":
		result, err = s.toolSteer(ctx, args)
	case "gorchera_diff":
		result, err = s.toolDiff(ctx, args)
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
	workspaceMode := stringArgDefault(args, "workspace_mode", string(domain.WorkspaceModeShared))
	maxSteps := intArgDefault(args, "max_steps", 8)
	pipelineMode := stringArgDefault(args, "pipeline_mode", "light")
	strictnessLevel := stringArgDefault(args, "strictness_level", "normal")
	ambitionLevel := stringArgDefault(args, "ambition_level", domain.AmbitionLevelMedium)
	contextMode := stringArgDefault(args, "context_mode", "full")
	if err := orchestrator.ValidateWorkspaceDir(workspaceDir); err != nil {
		return toolResult{}, err
	}
	if err := validatePipelineMode(pipelineMode); err != nil {
		return toolResult{}, err
	}

	var roleOverrides map[string]domain.RoleOverride
	if roRaw, ok := args["role_overrides"]; ok {
		if roMap, ok := roRaw.(map[string]any); ok {
			roleOverrides = parseRoleOverrides(roMap)
		}
	}

	input := orchestrator.CreateJobInput{
		Goal:            goal,
		Provider:        provider,
		WorkspaceDir:    workspaceDir,
		WorkspaceMode:   workspaceMode,
		MaxSteps:        maxSteps,
		StrictnessLevel: strictnessLevel,
		AmbitionLevel:   ambitionLevel,
		ContextMode:     contextMode,
		RoleProfiles:    domain.DefaultRoleProfiles(provider),
		RoleOverrides:   roleOverrides,
	}
	setOptionalStringField(&input, "PipelineMode", pipelineMode)

	// StartAsync creates the job synchronously and runs the main loop in a
	// background goroutine. This is necessary because runLoop is blocking and
	// we need to return the job ID immediately over the MCP channel.
	job, err := s.service.StartAsync(ctx, input)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolStartChain(ctx context.Context, args map[string]any) (toolResult, error) {
	workspaceDir, err := requireStringArg(args, "workspace_dir")
	if err != nil {
		return toolResult{}, err
	}
	if err := orchestrator.ValidateWorkspaceDir(workspaceDir); err != nil {
		return toolResult{}, err
	}

	rawGoals, ok := args["goals"].([]any)
	if !ok || len(rawGoals) == 0 {
		return toolResult{}, fmt.Errorf("goals is required")
	}

	goals := make([]domain.ChainGoal, 0, len(rawGoals))
	for i, rawGoal := range rawGoals {
		goalMap, ok := rawGoal.(map[string]any)
		if !ok {
			return toolResult{}, fmt.Errorf("goals[%d] must be an object", i)
		}
		goal := domain.ChainGoal{
			Goal:            stringArg(goalMap, "goal"),
			Provider:        domain.ProviderName(stringArg(goalMap, "provider")),
			StrictnessLevel: stringArgDefault(goalMap, "strictness_level", "normal"),
			AmbitionLevel:   stringArgDefault(goalMap, "ambition_level", domain.AmbitionLevelMedium),
			ContextMode:     stringArgDefault(goalMap, "context_mode", "full"),
			MaxSteps:        intArgDefault(goalMap, "max_steps", 8),
		}
		if roRaw, ok := goalMap["role_overrides"]; ok {
			if roMap, ok := roRaw.(map[string]any); ok {
				goal.RoleOverrides = parseRoleOverrides(roMap)
			}
		}
		if strings.TrimSpace(goal.Goal) == "" {
			return toolResult{}, fmt.Errorf("goals[%d].goal is required", i)
		}
		goals = append(goals, goal)
	}

	chain, err := s.service.StartChain(ctx, goals, workspaceDir)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(map[string]any{"chain_id": chain.ID})
}

// parseRoleOverrides converts a raw map[string]any (from JSON) into a typed
// map[string]domain.RoleOverride. Keys whose values cannot be type-asserted
// are silently skipped.
func parseRoleOverrides(raw map[string]any) map[string]domain.RoleOverride {
	result := make(map[string]domain.RoleOverride, len(raw))
	for role, val := range raw {
		m, ok := val.(map[string]any)
		if !ok {
			continue
		}
		var rp domain.RoleOverride
		if p, ok := m["provider"].(string); ok {
			rp.Provider = domain.ProviderName(p)
		}
		if model, ok := m["model"].(string); ok {
			rp.Model = model
		}
		result[role] = rp
	}
	return result
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
	wait := boolArgDefault(args, "wait", false)
	job, err := s.getJobStatus(ctx, jobID, wait, statusWaitDuration(args, wait))
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(job)
}

func (s *Server) toolChainStatus(ctx context.Context, args map[string]any) (toolResult, error) {
	chainID, err := requireStringArg(args, "chain_id")
	if err != nil {
		return toolResult{}, err
	}
	wait := boolArgDefault(args, "wait", false)
	chain, err := s.getChainStatus(ctx, chainID, wait, statusWaitDuration(args, wait))
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(chain)
}

func (s *Server) getJobStatus(ctx context.Context, jobID string, wait bool, waitTimeout time.Duration) (*domain.Job, error) {
	if !wait {
		return s.service.Get(ctx, jobID)
	}

	var (
		job     *domain.Job
		lastErr error
	)
	timer := time.NewTimer(waitTimeout)
	ticker := time.NewTicker(statusWaitPollInterval)
	defer timer.Stop()
	defer ticker.Stop()

	for {
		current, err := s.service.Get(ctx, jobID)
		if err == nil {
			job = current
			lastErr = nil
			if isTerminalJobStatus(job.Status) {
				return job, nil
			}
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if job != nil {
				return job, nil
			}
			return nil, lastErr
		case <-ticker.C:
		}
	}
}

func (s *Server) getChainStatus(ctx context.Context, chainID string, wait bool, waitTimeout time.Duration) (*domain.JobChain, error) {
	if !wait {
		return s.service.GetChain(ctx, chainID)
	}

	var (
		chain   *domain.JobChain
		lastErr error
	)
	timer := time.NewTimer(waitTimeout)
	ticker := time.NewTicker(statusWaitPollInterval)
	defer timer.Stop()
	defer ticker.Stop()

	for {
		current, err := s.service.GetChain(ctx, chainID)
		if err == nil {
			chain = current
			lastErr = nil
			if isTerminalChainStatus(chain.Status) {
				return chain, nil
			}
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			if chain != nil {
				return chain, nil
			}
			return nil, lastErr
		case <-ticker.C:
		}
	}
}

func isTerminalJobStatus(status domain.JobStatus) bool {
	switch string(status) {
	case string(domain.JobStatusDone), string(domain.JobStatusFailed), string(domain.JobStatusBlocked), "cancelled":
		return true
	default:
		return false
	}
}

func isTerminalChainStatus(status string) bool {
	switch status {
	case domain.ChainStatusDone, domain.ChainStatusFailed, domain.ChainStatusCancelled:
		return true
	default:
		return false
	}
}

func (s *Server) toolPauseChain(ctx context.Context, args map[string]any) (toolResult, error) {
	chainID, err := requireStringArg(args, "chain_id")
	if err != nil {
		return toolResult{}, err
	}
	chain, err := s.service.PauseChain(ctx, chainID)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(chain)
}

func (s *Server) toolResumeChain(ctx context.Context, args map[string]any) (toolResult, error) {
	chainID, err := requireStringArg(args, "chain_id")
	if err != nil {
		return toolResult{}, err
	}
	chain, err := s.service.ResumeChain(ctx, chainID)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(chain)
}

func (s *Server) toolCancelChain(ctx context.Context, args map[string]any) (toolResult, error) {
	chainID, err := requireStringArg(args, "chain_id")
	if err != nil {
		return toolResult{}, err
	}
	chain, err := s.service.CancelChain(ctx, chainID, stringArg(args, "reason"))
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(chain)
}

func (s *Server) toolSkipChainGoal(ctx context.Context, args map[string]any) (toolResult, error) {
	chainID, err := requireStringArg(args, "chain_id")
	if err != nil {
		return toolResult{}, err
	}
	chain, err := s.service.SkipChainGoal(ctx, chainID)
	if err != nil {
		return toolResult{}, err
	}
	return jsonResult(chain)
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

func (s *Server) toolDiff(ctx context.Context, args map[string]any) (toolResult, error) {
	jobID, err := requireStringArg(args, "job_id")
	if err != nil {
		return toolResult{}, err
	}
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		if os.IsNotExist(err) {
			return toolResult{}, fmt.Errorf("job not found: %s", jobID)
		}
		return toolResult{}, err
	}

	workspaceDir := strings.TrimSpace(job.WorkspaceDir)
	if workspaceDir == "" {
		return toolResult{}, fmt.Errorf("workspace path is missing for job: %s", jobID)
	}
	info, err := os.Stat(workspaceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return toolResult{}, fmt.Errorf("workspace path not found: %s", workspaceDir)
		}
		return toolResult{}, fmt.Errorf("stat workspace path %q: %w", workspaceDir, err)
	}
	if !info.IsDir() {
		return toolResult{}, fmt.Errorf("workspace path not found: %s", workspaceDir)
	}

	if _, err := gitCommandOutput(ctx, workspaceDir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return toolResult{}, fmt.Errorf("workspace is not a git worktree: %s", workspaceDir)
	}

	argsv := []string{"diff", "HEAD"}
	if pathspec := strings.TrimSpace(stringArg(args, "pathspec")); pathspec != "" {
		// Reject path traversal and git pathspec magic prefixes.
		// Only simple relative paths are accepted (no ".." segments, no ":" magic).
		if strings.Contains(pathspec, "..") {
			return toolResult{}, fmt.Errorf("pathspec must not contain '..': %q", pathspec)
		}
		if strings.HasPrefix(pathspec, ":") {
			return toolResult{}, fmt.Errorf("pathspec must not start with ':' (git magic prefix): %q", pathspec)
		}
		argsv = append(argsv, "--", pathspec)
	}
	output, err := gitCommandOutput(ctx, workspaceDir, argsv...)
	if err != nil {
		return toolResult{}, err
	}
	if strings.TrimSpace(output) == "" {
		return textResult("no changes"), nil
	}
	return textResult(output), nil
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
		if _, err := s.service.Approve(context.Background(), jobID); err != nil {
			log.Printf("[gorchera] Approve failed for job %s: %v", jobID, err)
		}
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
		if _, err := s.service.Retry(context.Background(), jobID); err != nil {
			log.Printf("[gorchera] Retry failed for job %s: %v", jobID, err)
		}
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
	extraSteps, err := optionalExtraStepsArg(args)
	if err != nil {
		return toolResult{}, err
	}
	// Resume calls runLoop; run in background.
	job, err := s.service.Get(ctx, jobID)
	if err != nil {
		return toolResult{}, err
	}
	go func() {
		if _, err := s.resumeJob(context.Background(), jobID, extraSteps); err != nil {
			log.Printf("[gorchera] Resume failed for job %s: %v", jobID, err)
		}
	}()
	snapshot := map[string]any{
		"job_id":  job.ID,
		"status":  job.Status,
		"message": "resume submitted; job is resuming in background",
	}
	if extraSteps > 0 {
		snapshot["extra_steps"] = extraSteps
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

func gitCommandOutput(ctx context.Context, dir string, args ...string) (string, error) {
	// Apply a hard 10-second timeout so that large worktrees cannot cause
	// git diff to hang indefinitely inside the MCP request handler.
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"HOME="+dir,
		"XDG_CONFIG_HOME="+dir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("git %s timed out after 10s", strings.Join(args, " "))
		}
		return "", fmt.Errorf("git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return string(output), nil
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

func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case int:
		return v, true
	case int32:
		return int(v), true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

func optionalExtraStepsArg(args map[string]any) (int, error) {
	raw, ok := args["extra_steps"]
	if !ok || raw == nil {
		return 0, nil
	}
	value, ok := intArg(map[string]any{"extra_steps": raw}, "extra_steps")
	if !ok {
		return 0, fmt.Errorf("extra_steps must be an integer")
	}
	if value < 1 || value > 20 {
		return 0, fmt.Errorf("extra_steps must be between 1 and 20")
	}
	return value, nil
}

func validatePipelineMode(mode string) error {
	switch mode {
	case "light", "balanced", "full":
		return nil
	default:
		return fmt.Errorf("pipeline_mode must be one of light, balanced, or full")
	}
}

func setOptionalStringField(target any, fieldName, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return
	}
	field := rv.Elem().FieldByName(fieldName)
	if !field.IsValid() || !field.CanSet() || field.Kind() != reflect.String {
		return
	}
	field.SetString(value)
}

func (s *Server) resumeJob(ctx context.Context, jobID string, extraSteps int) (*domain.Job, error) {
	// ResumeWithOptions is the canonical resume method. It accepts ExtraSteps
	// directly, so we don't need the old reflect-based ResumeWithExtraSteps
	// fallback that never existed on the real Service.
	return s.service.ResumeWithOptions(ctx, jobID, orchestrator.ResumeOptions{
		ExtraSteps: extraSteps,
	})
}

func statusWaitDuration(args map[string]any, wait bool) time.Duration {
	if !wait {
		return 0
	}

	seconds, ok := intArg(args, "wait_timeout")
	if !ok {
		return defaultStatusWaitTimeout()
	}
	if seconds == 0 {
		return statusWaitTimeout
	}
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return statusWaitDefault
}

func defaultStatusWaitTimeout() time.Duration {
	if statusWaitTimeout < statusWaitDefault {
		return statusWaitTimeout
	}
	return statusWaitDefault
}

func boolArgDefault(args map[string]any, key string, def bool) bool {
	v, ok := args[key].(bool)
	if !ok {
		return def
	}
	return v
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
