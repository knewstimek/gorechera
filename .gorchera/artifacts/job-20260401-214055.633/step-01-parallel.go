{
  "next_recommended_action": "",
  "status": "success",
  "summary": "Added rough token/cost tracking to orchestrator state. Introduced TokenUsage on Job and Step, added estimateTokens(input, output string) plus shared accumulation helpers, and applied tracking after RunLeader, RunWorker, RunPlanner, and RunEvaluator, including parallel worker execution. Updated docs to describe the heuristic tracking behavior. Verification passed: `go build ./...` succeeded and `go test ./...` succeeded."
}