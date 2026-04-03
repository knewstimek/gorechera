# Anthropic Blog Comparison

This document compares the ideas in [Anthropic's harness engineering post](https://www.reddit.com/r/ClaudeAI/comments/1s6jouf/anthropic_shares_how_to_make_claude_code_better) ("Harness design for long-running application development") with the current Gorchera repository. The goal is to separate what Gorchera already implements from where it intentionally takes a different path, and to identify ideas from the blog that still look adoptable without claiming features that do not exist today.

## Source Framing

The blog describes two related harness patterns:

- A generator/evaluator loop for frontend design, where grading criteria make subjective quality more concrete.
- A long-running coding harness that evolved into a planner + generator + evaluator architecture, with structured handoffs, model-aware decomposition, QA feedback loops, and later a simplified planner + builder + QA flow when stronger models reduced the need for sprint-by-sprint decomposition.

Gorchera is not trying to be that exact application-building harness. Per `docs/PRINCIPLES.md`, it is a workflow engine with persisted state, policy enforcement, and evidence capture. That difference matters when mapping concepts.

## Concepts From The Blog That Gorchera Already Implements

### 1. Separate planning from execution

The blog's planner expands a short prompt into a fuller product specification before implementation begins. Gorchera already has a dedicated planner phase that generates persisted planning artifacts before the main execution loop proceeds. This is visible in:

- `docs/ARCHITECTURE.md`: `ensurePlanning()` generates `product_spec.md`, `execution_plan.json`, `sprint_contract.json`, and `verification_contract.json`
- `docs/IMPLEMENTATION_STATUS.md`: "Provider-backed planner and evaluator phases, plus persisted planning artifacts and verification contracts"
- `internal/provider/protocol.go`: `buildPlannerPrompt()`

This is not merely "leader thinks before acting"; it is an explicit planner role with stored outputs.

### 2. Separate evaluation from generation/execution

The blog's central pattern is that the producer should not be trusted as its own final judge. Gorchera implements the same structural idea through a mandatory evaluator gate. A leader cannot directly mark a job done; `complete` must pass through `evaluateCompletion()`. Evidence:

- `docs/PRINCIPLES.md`: "Evaluator Gate Is Mandatory"
- `docs/ARCHITECTURE.md`: `complete` never transitions directly to `done`
- `internal/orchestrator/service.go`: `runLoop()` routes `complete` through `evaluateCompletion()`
- `internal/provider/protocol.go`: `buildEvaluatorPrompt()`

This is one of the strongest overlaps between the blog and Gorchera.

### 3. Structured handoff instead of raw conversational carryover

The blog emphasizes structured artifacts for cross-session handoff. Gorchera does the same, and in a stricter form. Workers hand off summaries, status, and artifacts rather than full transcripts. Evidence:

- `docs/PRINCIPLES.md`: "Artifact-Based Handoff Only"
- `docs/ARCHITECTURE.md`: planning, worker, and system artifacts are materialized under `.gorchera/artifacts/<jobID>/`
- `internal/provider/protocol.go`: worker contract is structured JSON only

This is a strong conceptual match, and Gorchera arguably enforces the boundary more explicitly than the blog's harness description.

### 4. Task decomposition into tractable units

The blog repeatedly returns to decomposition: first into tasks, then into sprints, then into planner/build/QA phases. Gorchera already decomposes work into typed steps (`implement`, `review`, `test`, `search`, `build`, `lint`, `command`) and persists step state and artifacts for each unit of work. Evidence:

- `docs/ARCHITECTURE.md`: job/step state model and leader actions
- `docs/IMPLEMENTATION_STATUS.md`: bounded orchestrator loop with persisted job state, step state, and ordered events
- `internal/provider/protocol.go`: leader schema and worker task typing

This is a workflow-engine version of the same idea rather than a UI-app-builder-specific one.

### 5. Role-specialized agents instead of a single undifferentiated worker

The blog's planner/generator/evaluator split maps conceptually to Gorchera's planner/leader/executor/tester/evaluator roles. Gorchera also supports role-specific provider/model routing. Evidence:

- `docs/ARCHITECTURE.md`: role profiles and overrides, role-based provider selection
- `docs/IMPLEMENTATION_STATUS.md`: role-based provider resolution
- `internal/provider/provider.go`: `resolveProfile()` and `adapterForProfile()`

The exact role taxonomy differs, but the underlying concept of specialized agent responsibilities is implemented.

### 6. Context management as an orchestration concern

The blog treats context growth as a harness-level problem. Gorchera also treats context shaping as an orchestrator responsibility, although with different mechanisms. It supports `full`, `summary`, and `minimal` leader payloads rather than relying on only one long conversation strategy. Evidence:

- `docs/ARCHITECTURE.md`: context modes
- `docs/IMPLEMENTATION_STATUS.md`: context shaping and steering
- `internal/provider/protocol.go`: `buildLeaderJobPayload()`, `buildSummaryPayload()`, `buildMinimalPayload()`

This is a narrower implementation than the blog's discussion of resets vs compaction, but it is clearly the same class of concern.

### 7. Controlled parallelism

The blog discusses multi-agent structure and decomposition; Gorchera implements parallel fan-out with explicit guardrails. Evidence:

- `docs/PRINCIPLES.md`: orchestrator-owned parallelism only
- `docs/ARCHITECTURE.md`: `parallel.go` enforces max 2 workers and disjoint targets/write scopes
- `docs/IMPLEMENTATION_STATUS.md`: parallel worker fan-out is implemented

Gorchera's implementation is stricter and more policy-driven, but it belongs in the same family of orchestration ideas.

## Where Gorchera Intentionally Differs

### 1. Gorchera is a stateful workflow engine, not a continuous single-session harness

The blog's later harness moved toward one continuous build session with model compaction. Gorchera instead centers persisted state, resumability, contracts, approvals, and typed control surfaces. It is designed to survive across turns and processes, not to keep one model session running as the primary unit of coherence. Evidence:

- `docs/PRINCIPLES.md`: Gorchera is not a conversational assistant
- `docs/ARCHITECTURE.md`: persisted job/chain state, control surfaces, approval flow, resume/retry/cancel
- `cmd/gorchera/main.go`: CLI and server/MCP entrypoints for lifecycle control

This is not a missing feature. It is a product-level architectural choice.

### 2. Gorchera forbids full transcript passing between agents

The blog focuses on structured handoffs, but it does not present the strict prohibition Gorchera enforces. Gorchera explicitly disallows whole inter-agent conversation log transfer. Evidence:

- `docs/PRINCIPLES.md`: "Do not pass full inter-agent conversation logs between agents"

That makes Gorchera more opinionated about isolation and artifact contracts than the blog harness.

### 3. Gorchera does not let executors create their own sub-workers

The blog is about composing specialized agents to improve performance. Gorchera accepts that idea only when the orchestrator owns the fan-out. Executors are not allowed to spawn their own worker trees. Evidence:

- `docs/PRINCIPLES.md`: "Orchestrator-Owned Parallelism"
- `docs/ARCHITECTURE.md`: leader actions `run_worker` / `run_workers`, disjoint-scope checks

This is a deliberate control and observability decision, not an omission.

### 4. Gorchera's evaluator is a completion gate, not a rich rubric-driven QA system

The blog's evaluator uses explicit criteria, hard thresholds, and real product interaction such as browser testing. Gorchera's current evaluator is much lighter-weight and heuristic. The repository explicitly says so:

- `docs/IMPLEMENTATION_STATUS.md`: evaluator scoring is still heuristic and step-coverage-based; full multi-axis scoring is not implemented
- `internal/provider/protocol.go`: `buildEvaluatorPrompt()` focuses on step evidence and verification contract satisfaction, not criterion-by-criterion product review

This is one of the clearest current gaps relative to the blog.

### 5. Gorchera is provider-neutral and cross-platform by principle

The blog's harness is tightly tied to Claude-specific capabilities, the Claude Agent SDK, and Playwright MCP. Gorchera intentionally keeps the core neutral:

- `docs/PRINCIPLES.md`: cross-platform neutral core
- `docs/ARCHITECTURE.md`: provider registry and role-aware adapter selection
- `internal/provider/provider.go`: adapter registry for Codex, Claude, and mock

This means Gorchera gives up some blog-specific depth in exchange for a more general orchestration core.

### 6. Gorchera preserves approval and policy gates as first-class runtime semantics

The blog is mainly about model performance and harness quality. Gorchera places stronger emphasis on operational policy: approval-required actions must block, risky scopes are checked, and supervisor directives cannot bypass these rules. Evidence:

- `docs/PRINCIPLES.md`: approval rules are not optional; supervisor directives cannot bypass policy or evaluator gates
- `docs/ARCHITECTURE.md`: policy-based `run_system` approval flow, workspace scope classification
- `internal/orchestrator/service.go`: `buildSystemRequest()`, policy evaluation, blocking behavior

This is a major architectural difference in priorities.

### 7. Gorchera currently uses context shaping, not model-adaptive reset/compaction orchestration

The blog explicitly compares context resets with compaction and changes strategy based on model behavior. Gorchera currently offers prompt payload shaping only. The docs call that out:

- `docs/PRINCIPLES.md`: current implementation gap is "Context compaction strategy: prompt payload shaping only"
- `docs/IMPLEMENTATION_STATUS.md`: no milestone-based leader session reset or provider-specific context strategy orchestration beyond payload modes

So Gorchera acknowledges the problem space, but does not yet implement the blog's more adaptive approach.

## Ideas From The Blog That Gorchera Could Still Adopt

### 1. Richer evaluator rubrics with explicit thresholds

The most valuable adoptable idea is the blog's criterion-based evaluator design. Gorchera already has an evaluator phase and verification contracts, so the natural extension is to make evaluator output more structured:

- add per-axis scoring such as functionality, completeness, UX quality, code quality, and policy compliance
- allow contracts to define minimum thresholds per axis
- fail completion when one axis misses its threshold, not just when step coverage looks insufficient

This would fit Gorchera's existing evaluator-gated completion model rather than fight it.

**Update (v2026.04.03.1):** Gorchera now has prompt overrides (`.gorchera/prompts/evaluator.md` or `prompt_overrides` job parameter) that allow per-project evaluator criteria customization without code changes. Combined with the existing `RubricAxes` in verification contracts, this partially addresses the rubric gap.

### 2. Browser-driven QA and richer runtime verification

The blog's evaluator interacts with the running product and checks UI behavior, APIs, and state. Gorchera explicitly does not have this yet:

- `docs/IMPLEMENTATION_STATUS.md`: no browser evaluator lifecycle, dev-server readiness orchestration, or restart policy

Adopting browser QA would materially improve evidence quality for web tasks, especially when paired with the existing harness/process control surfaces.

### 3. Task-adaptive decomposition instead of fixed current behavior

The blog evolves from sprint-heavy orchestration to a lighter planner + evaluator loop when stronger models make some scaffolding unnecessary. Gorchera could adopt a similar adaptive strategy:

- choose between single-step execution, staged review/test, or explicit milestone decomposition based on strictness, task type, or provider profile
- decide whether evaluator review should happen only at completion or also at milestones

This would be a useful extension of today's static strictness and context-mode settings.

### 4. Model-aware context strategy

The blog changes between resets and compaction depending on model behavior. Gorchera could add provider/model-aware context strategy selection on top of `full` / `summary` / `minimal`:

- session reset at milestone boundaries
- carry-forward handoff artifacts for a fresh leader session
- provider-specific defaults for when compaction vs reset is preferred

This aligns directly with an already documented implementation gap.

### 5. Stronger planner guidance for product depth

The blog's planner is intentionally ambitious about scope while avoiding overcommitting to low-level technical details too early. Gorchera's planner already exists, but it could borrow more of that stance:

- push for fuller product specs from short prompts
- emphasize deliverables and acceptance criteria more than premature implementation detail
- optionally suggest AI-feature opportunities when the goal is product-oriented

This would improve planning quality without changing the orchestrator's core principles.

## Overall Assessment

Gorchera already implements the blog's most important structural ideas: dedicated planning, separate evaluation, typed worker specialization, structured artifacts, task decomposition, and context-aware orchestration. The biggest difference is that Gorchera packages those ideas inside a policy-enforcing workflow engine rather than a continuous application-building harness.

The main opportunity is not to copy the blog literally. It is to deepen the evaluator and verification side of Gorchera with richer rubrics, runtime/browser QA, and model-adaptive orchestration choices while preserving Gorchera's existing principles: evaluator-gated completion, artifact-only handoffs, orchestrator-owned parallelism, approval enforcement, and cross-platform neutrality.
