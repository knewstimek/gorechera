{
  "next_recommended_action": "none",
  "status": "success",
  "summary": "Implemented structured worker failure feedback. Added `StructuredReason` and optional `Step.StructuredReason` in `internal/domain/types.go`. Updated `internal/orchestrator/service.go` to classify worker failure/block paths, attach structured reasons to steps, emit deterministic JSON payloads in `worker_failed` and `worker_blocked` events, and preserve blocked-reason strike counting by parsing event JSON. No docs were changed. Verification passed: `go build ./...` and `go test ./...` both succeeded."
}