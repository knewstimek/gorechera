package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"gorchera/internal/domain"
	"gorchera/internal/orchestrator"
	webstatic "gorchera/web"
)

type Server struct {
	orchestrator *orchestrator.Service
}

func NewServer(orchestrator *orchestrator.Service) *Server {
	return &Server{orchestrator: orchestrator}
}

func (s *Server) Handler() http.Handler {
	// Windows registry maps .js to text/plain -- override to correct MIME types.
	mime.AddExtensionType(".js", "application/javascript")
	mime.AddExtensionType(".css", "text/css")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/jobs", s.handleJobs)
	mux.HandleFunc("/jobs/", s.handleJob)
	mux.HandleFunc("/harness", s.handleHarness)
	mux.HandleFunc("/harness/", s.handleHarness)
	mux.HandleFunc("/chains", s.handleChains)
	mux.HandleFunc("/chains/", s.handleChain)
	// Serve static dashboard files from the embedded web/ assets.
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard/", http.FileServer(http.FS(webstatic.FS))))
	// Wrap all routes with bearer token auth when GORCHERA_AUTH_TOKEN is set.
	// If the env var is empty/unset, auth is skipped (development mode).
	token := os.Getenv("GORCHERA_AUTH_TOKEN")
	return authMiddleware(token, mux)
}

// authMiddleware enforces Bearer token authentication when token is non-empty.
// If token is empty, requests pass through without any check (development mode).
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		// Use constant-time comparison to prevent timing attacks on the token.
		if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs, err := s.orchestrator.List(r.Context())
		if err != nil {
			log.Printf("list jobs: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		var req StartJobRequest
		if err := decodeJSONBody(r, &req); err != nil {
			log.Printf("decode start job request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		job, err := s.orchestrator.Start(r.Context(), orchestrator.CreateJobInput{
			Goal:          req.Goal,
			TechStack:     req.TechStack,
			WorkspaceDir:  req.WorkspaceDir,
			WorkspaceMode: req.WorkspaceMode,
			Constraints:   req.Constraints,
			DoneCriteria:  req.DoneCriteria,
			Provider:      req.Provider,
			RoleProfiles:  req.RoleProfiles,
			MaxSteps:      req.MaxSteps,
		})
		if err != nil {
			log.Printf("start job: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/jobs/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	jobID, rest, hasRest := strings.Cut(path, "/")
	if !hasRest {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}

	switch {
	case rest == "resume":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Resume(r.Context(), jobID)
		if err != nil {
			log.Printf("resume job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case strings.HasPrefix(rest, "harness"):
		s.handleJobHarness(w, r, jobID, strings.TrimPrefix(rest, "harness"))
		return
	case rest == "approve":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Approve(r.Context(), jobID)
		if err != nil {
			log.Printf("approve job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case rest == "retry":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Retry(r.Context(), jobID)
		if err != nil {
			log.Printf("retry job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case rest == "reject":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RejectJobRequest
		if r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				log.Printf("read reject body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(strings.TrimSpace(string(body))) > 0 {
				if err := json.Unmarshal(body, &req); err != nil {
					log.Printf("decode reject request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
			}
		}
		job, err := s.orchestrator.Reject(r.Context(), jobID, req.Reason)
		if err != nil {
			log.Printf("reject job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case rest == "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req CancelJobRequest
		if r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				log.Printf("read cancel body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(strings.TrimSpace(string(body))) > 0 {
				if err := json.Unmarshal(body, &req); err != nil {
					log.Printf("decode cancel request: %v", err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
			}
		}
		job, err := s.orchestrator.Cancel(r.Context(), jobID, req.Reason)
		if err != nil {
			log.Printf("cancel job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case rest == "steer":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req SteerJobRequest
		if err := decodeJSONBody(r, &req); err != nil {
			log.Printf("decode steer request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		job, err := s.orchestrator.Steer(r.Context(), jobID, req.Message)
		if err != nil {
			log.Printf("steer job %s: %v", jobID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	case strings.HasPrefix(rest, "events/stream"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.streamEvents(w, r, jobID)
		return
	case strings.HasPrefix(rest, "events"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s events: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, job.Events)
		return
	case strings.HasPrefix(rest, "artifacts"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s artifacts: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, flattenArtifacts(job))
		return
	case strings.HasPrefix(rest, "verification"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s verification: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, BuildVerificationView(job))
		return
	case strings.HasPrefix(rest, "planning"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s planning: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, BuildPlanningView(job))
		return
	case strings.HasPrefix(rest, "evaluator"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s evaluator: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, BuildEvaluatorView(job))
		return
	case strings.HasPrefix(rest, "profile"):
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("get job %s profile: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, BuildProfileView(job))
		return
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, jobID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var lastCount int
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	for {
		job, err := s.orchestrator.Get(r.Context(), jobID)
		if err != nil {
			log.Printf("stream events get job %s: %v", jobID, err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		for ; lastCount < len(job.Events); lastCount++ {
			if err := writeSSEEvent(w, "job_event", job.Events[lastCount], lastCount); err != nil {
				return
			}
			flusher.Flush()
		}

		// Blocked is also terminal for streaming: the job will not make further
		// progress without operator intervention, so holding the connection open
		// would result in indefinite polling.
		if job.Status == domain.JobStatusDone ||
			job.Status == domain.JobStatusFailed ||
			job.Status == domain.JobStatusBlocked {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func flattenArtifacts(job *domain.Job) []string {
	var out []string
	for _, step := range job.Steps {
		out = append(out, step.Artifacts...)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSEEvent(w http.ResponseWriter, event string, payload any, id int) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\n", id); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}

func decodeJSONBody(r *http.Request, target any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return errors.New("request body is required")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return err
	}
	return nil
}

func (s *Server) handleChains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	chains, err := s.orchestrator.ListChains(r.Context())
	if err != nil {
		log.Printf("list chains: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, chains)
}

func (s *Server) handleChain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	chainID := strings.TrimPrefix(r.URL.Path, "/chains/")
	if chainID == "" {
		http.NotFound(w, r)
		return
	}
	chain, err := s.orchestrator.GetChain(r.Context(), chainID)
	if err != nil {
		log.Printf("get chain %s: %v", chainID, err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, chain)
}
