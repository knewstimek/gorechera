# ORCHESTRATOR_SPEC.md

## Project Identity

Gorchera는 Codex와 Claude를 포함한 provider-adapter 기반의 다중 에이전트 오케스트레이션 엔진이다.
초기 구현은 Go MVP로 시작하며, CLI와 HTTP API를 함께 제공하는 운영 가능한 백엔드 코어를 목표로 한다.
Windows Terminal은 필수 런타임이 아니라 선택적 관찰/디버그 뷰로만 취급한다.

## Platform Goal

Gorchera의 목표 플랫폼은 Windows, macOS, Linux다.
특정 운영체제의 터미널 UI, 프로세스 모델, 셸 문법에 코어 아키텍처가 종속되면 안 된다.

원칙:
- 코어 오케스트레이터는 OS 중립적인 Go 코드로 유지
- provider adapter는 가능하면 OS 공통 인터페이스를 사용
- runtime harness는 카테고리와 정책은 공통으로 유지하고, 실제 명령 조합은 OS별로 다를 수 있음
- 웹 UI와 HTTP API는 운영체제와 무관하게 동일하게 동작해야 함
- Windows 전용 관찰 도구는 optional debug layer로만 취급

## Goal
이 프로젝트의 목표는 사용자 입력 1회 이후에도 내부적으로 계속 작업을 진행할 수 있는 오케스트레이터를 구현하는 것이다.

오케스트레이터는 AI가 아니다.
오케스트레이터는 일반 프로그램이며 다음 역할만 수행한다.

- 작업 상태 관리
- 에이전트 세션 실행
- 에이전트 간 메시지 전달
- 승인 정책 적용
- artifact 저장 및 참조 전달
- 종료 조건 또는 중단 조건까지 내부 루프 실행

이 시스템은 대화형 비서가 아니라 워크플로 엔진이다.

## Implementation Baseline

초기 구현 방향은 다음과 같다.

- Go로 작성된 단일 리포지토리 MVP
- provider adapter를 통한 Codex / Claude 확장 가능 구조
- role-based execution profiles를 통한 planner/executor/tester/evaluator 분리
- 파일 기반 StateStore와 ArtifactStore
- CLI 우선 운영, HTTP API와 이벤트 스트림은 같은 엔진의 제어면으로 제공
- mock provider로 end-to-end 루프를 먼저 검증
- CLI와 HTTP API에서 job state, events, artifacts를 분리해 볼 수 있어야 함

현재 Go MVP는 job에 role profile을 저장하고 planner / leader / executor / tester / evaluator 라우팅에 실제로 사용한다.
planner / evaluator phase도 provider-backed session으로 동작하며, 병렬 worker fan-out은 orchestrator가 `max_parallel_workers = 2`와 disjoint write scope 정책으로 직접 집행한다.

## Core Principles

1. 오케스트레이터는 AI가 아니며 메시지를 이해하려 하지 않는다.
2. 리더 에이전트 A는 작업 분해와 결과 취합만 담당한다.
3. 워커 에이전트 B, C, D는 단일 역할만 수행한다.
4. 에이전트 간에는 전체 대화 로그를 전달하지 않는다.
5. 전달 단위는 구조화된 JSON과 artifact reference만 사용한다.
6. 워커 에이전트는 task-scoped session으로 실행 후 폐기한다.
7. 리더 에이전트 A는 상대적으로 긴 문맥을 가질 수 있으나 milestone 단위로 요약한다.
8. 에이전트는 사용자에게 질문하지 않는다.
9. 정보가 부족하면 질문 대신 blocked 상태를 반환한다.
10. 사용자 입력 없이도 내부 루프는 계속 진행되어야 한다.
11. 위험 작업만 중단 가능하며 그 외에는 자동 진행한다.

## High-Level Architecture

### Orchestrator
역할:
- job 생성
- 현재 상태 저장
- A/B/C/D 세션 실행
- A의 action을 파싱하고 대상 agent로 전달
- worker 결과를 저장하고 다시 A에게 전달
- 승인 정책 적용
- 무한 대기 대신 bounded loop 실행
- done / failed / blocked / max_steps 도달 시 종료

오케스트레이터는 판단자가 아니라 라우터이자 상태 관리자다.

### SessionManager
역할:
- provider 종류별 세션 생성과 종료 관리
- leader / worker 역할에 맞는 실행 설정 선택
- provider transport 추상화
- 세션 상태 추적
- 타임아웃 / 중단 / 강제 종료 처리
- provider별 오류를 공통 오류 클래스로 정규화

SessionManager는 특정 도구 이름에 종속되면 안 된다.
Codex CLI, Claude CLI 또는 이후 추가될 provider를 같은 인터페이스로 다뤄야 한다.

권장 인터페이스 예시:
- StartSession
- SendMessage
- WaitResult
- Interrupt
- CloseSession

### Provider Adapter
세션 실행기는 provider adapter 구조로 분리한다.

초기 목표:
- Codex provider 지원
- Claude provider 지원

각 provider는 내부적으로 서로 다른 실행 방식을 가질 수 있으나 오케스트레이터는 공통 인터페이스만 사용해야 한다.

### Role-Based Execution Profiles
Gorchera는 하나의 global provider/model 설정에 묶이지 않고, 역할별 execution profile을 선택하는 구조를 사용한다.

역할 예시:
- planner
- executor
- tester
- evaluator

각 역할 프로파일은 최소한 다음 필드를 가진다.

| Field | Meaning |
| --- | --- |
| `provider` | 역할에 사용할 provider 이름. 예: `codex`, `claude` |
| `model` | provider 내부 모델 또는 모델 계열 |
| `effort` | reasoning / speed / cost 우선순위 |
| `tool_policy` | 허용 도구, 파일 접근 범위, 네트워크 허용 여부, 위험 작업 허용 여부 |
| `fallback` | 주 provider 실패 시 사용할 대체 provider / model / mode |
| `budget` | 시간, 호출 수, 비용, 단계 수, retry 상한 |

추가로 역할별로 `timeout`, `permission_mode`, `context_strategy`, `output_schema`를 둘 수 있다.

오케스트레이터는 job 전체에 하나의 provider를 고정하지 않는다.
대신 role profile registry를 조회해서 planner/executor/tester/evaluator마다 다른 execution setting을 선택한다.

스프린트 계약은 기본 profile을 override할 수 있다.
예를 들어 evaluator는 adversarial 검증을 수행하며 수정 권한 없이 판정만 하고, tester는 더 낮은 effort와 제한된 tool set을 쓴다.

### Verification Contract
verification contract는 planner가 만든 목표와 sprint contract를 tester/evaluator가 실제로 검증할 수 있게 정리한 읽기 전용 계약이다.

검증 contract는 다음 요소를 한 화면에 모은다.
- sprint contract
- evaluator report
- tester와 evaluator role profile
- required checks
- required artifacts

운영 표면에서는 `verification` 뷰로 읽는다.
이 뷰는 저장 모델을 바꾸지 않고, 현재 job에 이미 저장된 planning artifact와 evaluator evidence를 조합해서 보여준다.

### Parallel Execution Policy
병렬 실행은 기본값이 아니라 명시적 허가가 필요한 최적화다.

원칙:
- planner는 병렬 가능 후보를 제안할 수 있다
- leader는 병렬 실행 여부와 분할 단위를 승인한다
- orchestrator는 정책과 자원 한도에 따라 실제 worker spawn을 집행한다
- executor는 임의로 다른 worker를 spawn하지 않는다

초기 정책:
- `max_parallel_workers = 2`
- 병렬 작업은 서로 겹치지 않는 `disjoint write scope`를 가져야 한다
- 병렬 작업은 artifact merge rule이 사전에 정의되어 있어야 한다
- 병렬 실행 시 각 worker는 필요한 최소 context만 전달받아야 한다
- shared context는 짧은 job summary, contract excerpt, role-specific artifact reference로 제한한다

토큰/비용 원칙:
- 전체 대화 로그는 전달하지 않는다
- worker마다 필요한 artifact만 선택적으로 넘긴다
- planner, leader, evaluator는 요약 기반 context를 선호한다
- 불확실한 병렬화보다 순차 실행을 우선한다

### Transport Layer
초기 transport는 CLI subprocess 기반으로 한다.

권장 방식:
- 표준 입력/출력 또는 동등한 프로세스 I/O 기반 제어
- 백그라운드 프로세스 실행 가능
- 오케스트레이터가 세션 생명주기를 직접 관리

비권장 방식:
- Windows Terminal 탭 또는 창 자체를 세션 제어의 기준으로 사용하는 방식

Windows Terminal은 사람이 관찰하는 디버그 뷰로는 사용할 수 있으나, 핵심 세션 제어 경계로 삼으면 안 된다.
핵심 자동화는 터미널 UI가 아니라 프로세스와 I/O를 기준으로 구현해야 한다.

### Leader Agent A
역할:
- 전체 목표 이해
- 현재 상태를 바탕으로 다음 action 결정
- worker에게 보낼 task 생성
- worker 결과 취합
- 완료 여부 판단
- 다음 단계 추천

A는 B/C/D의 긴 실행 로그를 직접 들고 있지 않는다.
A는 worker의 구조화된 결과와 artifact reference만 읽는다.

### Planner Agent
역할:
- 짧은 사용자 goal을 실행 가능한 product spec 또는 execution plan으로 확장
- 범위를 sprint 단위 작업으로 분해
- 고수준 deliverable과 done criteria 정리
- 구현 세부사항을 과도하게 고정하지 않음

Planner는 구현 자체를 담당하지 않는다.
Planner의 출력은 이후 sprint contract와 worker 작업의 기준 artifact가 된다.

### Worker Agent B
역할:
- 구현 전용
- 입력된 task만 수행
- 코드 수정 / 파일 생성 / patch 생성
- 요약과 artifact만 반환
- 세션 종료

### Worker Agent C
역할:
- 리뷰 전용
- diff / patch / notes 검토
- 이슈 목록과 판단만 반환
- 세션 종료

### Worker Agent D
역할:
- 테스트 전용
- 지정된 테스트 실행
- 실패/성공 결과와 로그 요약만 반환
- 세션 종료

## Non-Goals

다음은 하지 않는다.

- 에이전트 간 전체 대화 로그 전달
- 대화형 assistant 스타일 UX
- "다음 뭐할까요?" 같은 질의
- 사용자 승인 없이 위험 작업 수행
- 워커 세션 장기 유지
- 자연어 메시지 파싱에 의존하는 라우팅

## Main Execution Model

사용자는 오케스트레이터에게 프로젝트 목표를 입력한다.
오케스트레이터는 job을 생성하고 planner 또는 leader를 실행한다.
planner는 필요 시 plan artifact를 생성한다.
A는 현재 상태와 plan artifact를 바탕으로 다음 action을 JSON으로 반환한다.
오케스트레이터는 action을 실행한다.
worker 결과를 저장한다.
저장된 결과를 다시 A에게 전달한다.
A는 다음 action을 결정한다.
이 과정을 종료 조건까지 반복한다.

## Planning Phase

짧은 goal만으로 시작하는 경우 planner 단계를 먼저 수행할 수 있어야 한다.

planner의 출력 예시:
- product_spec.md
- execution_plan.json
- sprint_backlog.json
- design_principles.md

planner는 구현 세부 코드를 미리 못박기보다 다음을 정의하는 데 집중한다.
- 무엇을 만들어야 하는지
- 어떤 순서로 검증 가능한 단위로 쪼갤지
- 각 단위의 완료 기준이 무엇인지

planner 출력은 artifact로 저장하고 leader가 이후 루프에서 참조한다.

## Project Bootstrap

프로젝트가 아직 존재하지 않는 경우에도 오케스트레이터는 동작해야 한다.

초기 입력:
- project goal
- tech stack
- constraints
- done criteria

초기 흐름:
1. 오케스트레이터가 job 생성
2. planner 또는 leader A가 초기 계획 수립
3. sprint contract 초안 생성
4. implementation worker가 프로젝트 스캐폴딩 생성
5. evaluator 또는 review/test worker가 최소 검증 수행
6. 이후 일반 루프로 전환
## Main Loop

다음 루프를 구현한다.

1. job 시작
2. 필요 시 planner 결과 또는 기존 sprint backlog 로드
3. A에게 목표, 현재 상태, 관련 artifact 전달
4. 필요 시 sprint contract 초안 생성 또는 갱신
5. A가 action 반환
6. 오케스트레이터가 action 검증
7. worker 또는 system action 실행
8. 결과 저장
9. evaluator gate 통과 여부 검사
10. 결과를 A에게 전달
11. 종료 조건 검사
12. 종료가 아니면 반복

## Sprint Contract Protocol

구현 또는 테스트를 시작하기 전에 이번 sprint의 범위와 완료 기준을 명시적으로 고정할 수 있어야 한다.

contract artifact 예시:
- sprint_contract.json
- sprint_acceptance.md

최소 포함 항목:
- sprint 목표
- 대상 기능 목록
- 제외 범위
- 완료 기준
- 검증 방법
- 관련 artifact

leader와 evaluator는 contract를 기준으로 generator 또는 worker 결과를 해석해야 한다.
contract가 확정되지 않으면 구현 작업을 시작하지 않는 모드를 지원하는 것이 바람직하다.

## Required Behavior

### No User Turn Dependency
내부 루프는 사용자 입력 없이 계속 진행되어야 한다.
사용자 턴이 와야 상태를 확인할 수 있는 구조는 금지한다.

### No Interactive Questions
에이전트는 다음과 같은 표현을 사용하면 안 된다.

- 다음 뭐할까요
- 이걸 진행해도 될까요
- 승인해 주세요
- 어느 방향으로 갈까요

대신 구조화된 blocked 상태를 반환해야 한다.

### Blocked Instead of Asking
정보 부족, 충돌, 정책 위반, 위험 작업 필요 시 질문하지 말고 blocked 상태를 반환한다.

### Artifact-Based Handoff
A와 B/C/D 사이 전달은 다음만 허용한다.

- 짧은 task 설명
- structured JSON
- artifact path 또는 artifact id
- 요약
- 상태값

전체 reasoning 로그 전달은 금지한다.

## State Machine

Job 상태:

- queued
- starting
- running
- waiting_leader
- waiting_worker
- blocked
- failed
- done

Step 상태:

- pending
- active
- succeeded
- blocked
- failed
- skipped

## Stop Conditions

다음 중 하나면 루프를 종료한다.

- job 상태가 done
- job 상태가 failed
- job 상태가 blocked 이고 자동 복구 불가
- max_steps 초과
- max_retries 초과
- 동일 blocked_reason 반복 횟수 초과
- timeout 도달

## Evaluation Criteria And Gating

review와 test는 단순 참고 의견이 아니라 다음 단계 진행을 막을 수 있는 gate가 되어야 한다.

최소 평가 축 예시:
- functionality
- code_quality
- protocol_compliance
- ux_or_product_quality
- test_health

규칙:
- critical 기준 실패 시 다음 sprint로 진행 금지
- evaluator는 실패 이유와 재시도 방향을 구조화된 artifact로 남긴다
- gate를 통과하지 못하면 leader는 수정 작업 또는 blocked/fail 중 하나를 선택해야 한다
- 오케스트레이터는 gate 결과를 저장하고 우회 진행을 허용하지 않는다

## Retry Policy

다음 규칙을 둔다.

- 일시적 실패는 제한 횟수 내 재시도 가능
- 동일 blocked_reason이 3회 반복되면 중단
- 동일 action이 진전 없이 반복되면 중단
- worker 실패 시 A에게 재판단 기회 제공
- A가 새 계획을 내지 못하면 failed 또는 blocked 처리

## Service Availability And Access Edge Cases

오케스트레이터는 모델 응답이 항상 정상적으로 온다고 가정하면 안 된다.
세션 사용량 초과, 로그인 만료, 결제/플랜 문제, 권한 부족, 모델 비가용, 레이트리밋, 네트워크 오류를 모두 고려해야 한다.

### Failure Classes
다음 상태를 구분해야 한다.

- authentication_error
- authorization_error
- quota_exceeded
- billing_required
- rate_limited
- model_unavailable
- session_expired
- network_error
- transport_error
- invalid_provider_response

### Required Handling Rules

1. 인증 실패, 로그인 만료, 결제 필요, 권한 부족은 자동 재시도하지 않는다.
2. quota_exceeded, billing_required, authentication_error, authorization_error, session_expired는 recoverable blocked 상태로 전환한다.
3. rate_limited, network_error, transport_error, model_unavailable는 제한 횟수 내 재시도할 수 있다.
4. invalid_provider_response는 스키마 오류와 분리해서 기록한다.
5. provider 응답이 없으면 worker 성공으로 간주하면 안 된다.
6. 리더 또는 워커가 응답하지 못한 경우 다음 단계로 진행하면 안 된다.
7. 오케스트레이터는 "모델이 응답했는지", "응답이 유효한지", "정책상 실행 가능한지"를 분리해서 검사해야 한다.

### Availability State Mapping

예시 매핑:
- login required -> blocked
- payment required -> blocked
- usage limit exceeded -> blocked
- insufficient permissions -> blocked
- rate limit -> retry then blocked
- transient network failure -> retry then blocked
- provider internal error -> retry then failed 또는 blocked
- malformed response -> retry then failed

### User-Visible Block Reasons

다음과 같은 이유 코드를 보존해야 한다.

- blocked_reason = "authentication_required"
- blocked_reason = "billing_required"
- blocked_reason = "quota_exceeded"
- blocked_reason = "rate_limited"
- blocked_reason = "model_unavailable"
- blocked_reason = "session_expired"
- blocked_reason = "network_failure"

오케스트레이터는 단순히 "failed"로 뭉개지 말고 원인 코드를 저장해야 한다.

### Retry Guidance

- authentication_required: 재시도 금지
- billing_required: 재시도 금지
- quota_exceeded: 짧은 재시도 금지, 정책에 따라 장기 보류 가능
- rate_limited: exponential backoff 후 제한 횟수 내 재시도
- network_failure: 재시도 가능
- model_unavailable: 재시도 가능
- session_expired: 재인증 전까지 재시도 금지

### No False Progress
리더 또는 워커 호출이 실패했는데도 오케스트레이터가 내부 루프를 계속 전진시키면 안 된다.
응답 부재 또는 비정상 응답은 progress가 아니라 blocked 또는 failure다.

### Suspension And Resume
외부 서비스 상태 때문에 blocked된 job은 나중에 재개 가능해야 한다.

필수 요구:
- blocked 시점의 job state 저장
- 마지막 정상 artifact 참조 저장
- 마지막 leader context summary 저장
- resume 가능한 blocked reason 저장

### Acceptance Criteria For Edge Cases

다음 조건을 만족해야 한다.

- 로그인 만료 시 다음 단계로 잘못 진행하지 않는다.
- 결제/플랜 문제 발생 시 blocked_reason을 남긴다.
- 사용량 초과 시 성공으로 오판하지 않는다.
- 일시적 네트워크 오류는 재시도 후 처리한다.
- provider 응답 포맷이 깨지면 invalid_provider_response로 기록한다.
- recoverable blocked 상태에서 resume가 가능하다.
## Approval Policy

기본 원칙:
안전한 작업은 자동 진행한다.
위험한 작업만 중단한다.

자동 허용 예시:
- workspace 내부 파일 읽기
- workspace 내부 파일 수정
- 검색
- 빌드
- 테스트
- lint
- diff 생성

자동 중단 예시:
- workspace 외부 파일 수정
- 네트워크 접근
- 대량 삭제
- 배포
- git push
- credential 접근

위험 작업 필요 시 에이전트는 질문하지 말고 blocked 상태와 reason을 반환해야 한다.

## Operator Interfaces

오케스트레이터는 하나의 내부 엔진 위에 여러 운영 인터페이스를 가져야 한다.
CLI와 웹은 서로 다른 제품이 아니라 같은 엔진의 control plane이어야 한다.

### CLI Control Plane
초기 버전부터 CLI를 제공한다.

최소 명령 예시:
- orchestrator run
- orchestrator resume
- orchestrator status
- orchestrator events
- orchestrator approve
- orchestrator cancel
- orchestrator serve

CLI는 로컬 운영, 자동화 스크립트, 디버깅, 비대화형 배치 실행에 사용된다.

### Web Control Plane
나중 단계에서 웹 UI와 HTTP API를 제공할 수 있도록 구조를 설계한다.

웹 인터페이스 목표:
- job 목록 조회
- job 상세 상태 조회
- 현재 step 및 최근 이벤트 조회
- artifact 링크 조회
- blocked reason 조회
- pause / resume / retry / cancel / approve 같은 운영 명령 실행

### HTTP API
웹 UI는 직접 엔진을 호출하지 않고 명시적 API를 통해 제어한다.

최소 API 범주:
- job 생성
- job 조회
- job 이벤트 조회
- job artifact 조회
- job 이벤트 스트림 조회
- approve / resume / retry / cancel
- artifact 메타데이터 조회

현재 MVP 기준 실제 운영 표면 예시:
- `GET /healthz`
- `GET /jobs`
- `POST /jobs`
- `GET /jobs/{id}`
- `GET /jobs/{id}/events`
- `GET /jobs/{id}/events/stream`
- `GET /jobs/{id}/artifacts`
- `GET /jobs/{id}/verification`
- `GET /jobs/{id}/planning`
- `GET /jobs/{id}/evaluator`
- `GET /jobs/{id}/profile`
- `POST /jobs/{id}/resume`
- `POST /jobs/{id}/approve`
- `POST /jobs/{id}/reject`
- `POST /jobs/{id}/retry`
- `POST /jobs/{id}/cancel`
- `GET /jobs/{id}/harness`
- `GET /jobs/{id}/harness/processes`
- `POST /jobs/{id}/harness/processes`
- `GET /jobs/{id}/harness/processes/{pid}`
- `POST /jobs/{id}/harness/processes/{pid}/stop`
- `GET /harness/processes`
- `POST /harness/processes`
- `GET /harness/processes/{pid}`
- `POST /harness/processes/{pid}/stop`

### Realtime Events
웹 UI와 CLI는 polling만으로 상태를 추적하지 않도록 event stream을 제공하는 것이 바람직하다.

허용 예시:
- SSE
- WebSocket
- append-only event log 기반 tail

현재 MVP는 persisted event log를 polling해서 SSE로 흘려보내는 방식이다.
즉 장기적으로는 더 강한 realtime transport로 교체될 수 있지만, 현재 단계에서는 event source of truth가 job에 저장된 ordered event log다.

### Control Plane Semantics

운영 명령은 단순 버튼이 아니라 상태 전이 규칙을 가져야 한다.

현재 MVP 기준 semantics:
- `approve`: pending approval로 막혀 있던 system step을 operator consent로 재실행하고, 성공 시 내부 루프를 이어간다
- `reject`: pending approval을 제거하고 job을 blocked 상태로 남긴다
- `retry`: blocked 또는 failed 상태의 job을 running으로 되돌리고 내부 루프를 다시 시작한다
- `cancel`: running 또는 blocked job을 operator reason과 함께 blocked 상태로 전환한다
- `resume`: blocked 상태의 job을 기존 artifact와 context summary를 유지한 채 다시 루프에 넣는다

이 semantics는 CLI와 HTTP API 양쪽에서 동일해야 한다.

### Headless Service Mode
오케스트레이터는 UI 없이 백그라운드 서비스로 실행 가능해야 한다.
CLI와 웹은 이 서비스에 붙는 운영 인터페이스일 수 있다.

### Auditability
웹 또는 CLI에서 발생한 운영 명령은 감사 가능해야 한다.

최소 기록 대상:
- 누가 어떤 job에 어떤 명령을 내렸는지
- 언제 승인 / 재시도 / 취소가 발생했는지
- 어떤 blocked reason에서 재개되었는지

## Runtime Harness Lifecycle

오케스트레이터는 코드 편집만 관리하는 것이 아니라 필요 시 실행 환경의 수명주기도 관리해야 한다.

예시 책임:
- 개발 서버 시작
- 테스트 프로세스 실행
- 브라우저 또는 헤드리스 검증 세션 시작
- 포트 충돌 또는 hung process 정리
- 실행 로그 요약 저장

이 책임은 오케스트레이터가 직접 판단한다는 뜻이 아니라, runtime action을 안전하게 시작/중지/검증하는 절차를 가진다는 뜻이다.

runtime harness 구현 규칙:
- 특정 셸 문법에 코어 로직이 묶이지 않도록 한다
- 명령 정책은 category 중심으로 정의하고, OS별 실제 실행 명령은 adapter 또는 runtime layer에서 선택한다
- `bash` 전용 또는 `PowerShell` 전용 가정을 프로토콜 수준에 넣지 않는다
- 동일한 job/state/artifact 모델이 Windows, macOS, Linux에서 공통으로 유지되어야 한다

현재 MVP에서 runtime harness가 실제로 보장하는 범위:
- bounded local process start
- process status 조회
- process stop
- stdout/stderr log capture
- global runtime inventory 조회
- job-scoped harness ownership 조회 및 제어

현재 MVP에서 아직 없는 것:
- browser evaluator lifecycle
- dev server readiness probe와 port orchestration
- process restart policy
- job state에 대한 persisted harness recovery

### Harness Ownership Model

runtime harness는 전역 inventory와 job-scoped ownership surface를 함께 가진다.

구분:
- `/harness/*`: 전역 runtime inventory
- `/jobs/{id}/harness/*`: 특정 job이 소유한 process만 노출하는 표면

필수 규칙:
- job-scoped surface는 해당 job이 시작한 pid만 보여준다
- 다른 job이 소유한 pid를 job-scoped surface로 조회하거나 중지하려 하면 거부한다
- ownership check는 orchestrator service 레벨에서 강제한다
- 전역 inventory는 운영자용 진단 표면으로 남길 수 있다

## Provider Context Strategy

provider마다 긴 세션 유지와 context reset의 효율이 다를 수 있다.
오케스트레이터는 provider별 전략을 선택할 수 있어야 한다.

지원해야 할 전략:
- long_session_with_compaction
- task_scoped_reset_with_handoff
- hybrid

선택 기준 예시:
- provider 특성
- 현재 작업 길이
- context window 사용량
- 오류 또는 품질 저하 징후

reset을 수행할 경우 handoff artifact가 다음 세션에 충분한 상태를 전달해야 한다.

## Context Policy

### Leader Agent A
A는 다음 정보를 가질 수 있다.

- 프로젝트 목표
- 현재 단계
- 완료된 작업 요약
- 열린 이슈
- 결정 로그 요약
- 남은 작업 목록
- 최근 worker 결과 요약

A는 milestone 단위로 요약된다.
A의 세션이 과도하게 커지면 새 세션을 만들고 아래 정보를 재주입한다.

- project summary
- active tasks
- decision log summary
- pending risks
- protocol rules

### Worker Agents B/C/D
B/C/D는 task-scoped session이어야 한다.
각 worker는 작업 종료 후 세션을 폐기한다.
다음 작업에서는 이전 대화 로그를 넘기지 않는다.
필요한 정보만 새로 주입한다.

## Protocol Rules

모든 에이전트 응답은 구조화된 JSON이어야 한다.
자유 텍스트 응답에 의존하지 않는다.

리더 A는 반드시 아래 action 중 하나만 반환한다.

- run_worker
- run_workers
- run_system
- summarize
- complete
- fail
- blocked

워커 B/C/D는 반드시 아래 status 중 하나만 반환한다.

- success
- failed
- blocked

## Leader Output Schema

```json
{
  "action": "run_worker | run_workers | run_system | summarize | complete | fail | blocked",
  "target": "B | C | D | none",
  "task_type": "implement | review | test | summarize | none",
  "task_text": "string",
  "artifacts": ["string"],
  "reason": "string",
  "next_hint": "string"
}
```

필드 규칙:
- action은 필수
- run_worker일 경우 target, task_type, task_text 필수
- run_system일 경우 target=SYS, task_type, system_action 필수
- complete / fail / blocked일 경우 reason 필수
- artifacts는 선택
- next_hint는 선택

## Worker Output Schema

```json
{
  "status": "success | failed | blocked",
  "summary": "string",
  "artifacts": ["string"],
  "blocked_reason": "string | null",
  "error_reason": "string | null",
  "next_recommended_action": "string | null"
}
```

필드 규칙:
- status는 필수
- summary는 필수
- artifacts는 선택
- blocked면 blocked_reason 필수
- failed면 error_reason 필수

## Orchestrator Validation Rules

오케스트레이터는 에이전트 출력을 신뢰하지 말고 검증한다.

검증 규칙:
- JSON 파싱 실패 시 재요청
- 필수 필드 누락 시 재요청
- 허용되지 않은 action 거부
- 허용되지 않은 target 거부
- 승인 정책 위반 action은 실행 금지
- invalid schema가 반복되면 실패 처리

## Internal Message Flow

### A to Worker
A는 worker에게 다음만 전달한다.

- task_type
- task_text
- 필요한 artifact reference
- 현재 작업 범위
- 완료 기준

### Worker to A
worker는 A에게 다음만 전달한다.

- status
- summary
- artifacts
- blocked_reason 또는 error_reason

전체 작업 로그는 전달하지 않는다.

## Artifact Store

artifact는 다음 예시를 포함한다.

- patch.diff
- implementation_notes.md
- review_report.json
- test_report.json
- project_summary.md
- decision_log.md
- product_spec.md
- execution_plan.json
- sprint_contract.json
- evaluator_report.json
- runtime_result.json

## Operator Visibility

운영자는 job 전체 JSON만 보는 것에 의존하지 않아야 한다.

최소 분리 노출:
- current job state
- ordered event log
- flattened artifact references
- step-level summaries

이 분리는 CLI와 HTTP API 양쪽 모두에서 유지되어야 한다.

오케스트레이터는 artifact path 또는 artifact id만 전달한다.

## Default Worker Prompts

### Prompt for A
너는 leader agent다.
사용자와 직접 대화하지 않는다.
항상 구조화된 JSON만 반환한다.
가능한 한 다음 작업을 스스로 결정한다.
질문하지 않는다.
불확실하면 blocked를 반환한다.
worker의 전체 로그를 요구하지 말고 summary와 artifact만 사용한다.

### Prompt for B
너는 implementation worker다.
입력된 task만 수행한다.
질문하지 않는다.
작업 완료 후 지정된 JSON만 반환한다.
불확실하면 blocked를 반환한다.
관련 없는 리팩터링은 하지 않는다.

### Prompt for C
너는 review worker다.
diff와 artifact만 검토한다.
질문하지 않는다.
지정된 JSON만 반환한다.
코드 변경은 하지 않는다.

### Prompt for D
너는 test worker다.
지정된 테스트만 실행한다.
질문하지 않는다.
지정된 JSON만 반환한다.
결과와 실패 요약만 제공한다.

## Implementation Requirements

오케스트레이터 구현 시 반드시 포함할 것:

- JobManager
- SessionManager
- ProviderRegistry
- ProviderAdapter interface
- Transport abstraction
- StateStore
- ArtifactStore
- MessageValidator
- ApprovalPolicy
- LoopController
- RetryPolicy
- CLI control commands
- HTTP API or API-ready service boundary
- Event log / event stream support
- Cross-platform runtime strategy

## Minimum Viable Implementation

최소 구현은 다음을 만족해야 한다.

1. 하나의 job 생성 가능
2. A 실행 가능
3. A의 run_worker action 파싱 가능
4. B/C/D 중 하나 실행 가능
5. worker 결과를 저장하고 A에게 다시 전달 가능
6. done / failed / blocked 종료 가능
7. max_steps 제한 가능
8. invalid JSON 방어 가능
9. 최소 1개 provider adapter 연결 가능
10. CLI로 run / status / resume 실행 가능
11. CLI 또는 API로 events/artifacts를 분리 조회 가능

MVP 단계에서는 다음을 아직 생략할 수 있다.
- 실제 Codex / Claude adapter
- Playwright 기반 evaluator
- 자동 sprint contract 협상
- 웹 UI

하지만 아래 경계는 미리 유지해야 한다.
- provider adapter 경계
- runtime harness action 경계
- evaluator gate 확장 가능성
- self-hosting으로 넘어갈 수 있는 artifact 구조

## Acceptance Criteria

다음 조건을 만족하면 구현 완료로 본다.

- 사용자 입력 없이 내부 루프가 진행된다
- A가 B/C/D를 호출할 수 있다
- B/C/D 결과가 다시 A에게 전달된다
- 되묻기 대신 blocked가 반환된다
- 안전 작업은 자동 진행된다
- 위험 작업은 blocked 처리된다
- 전체 대화 로그 전달 없이 동작한다
- worker 세션은 작업 종료 후 폐기된다
- A는 milestone 단위 요약이 가능하다
- SessionManager가 특정 provider 이름에 종속되지 않는다
- Codex 또는 Claude 중 하나 이상을 adapter로 연결할 수 있다
- Windows Terminal 없이도 전체 루프가 동작한다
- CLI에서 job 상태 확인과 제어가 가능하다
- 웹 UI를 붙일 수 있는 API 경계가 분리되어 있다
- planner artifact와 sprint contract artifact를 나중에 추가해도 코어 구조를 뒤엎지 않는다

## Explicit Constraints

- 기존 변수명은 불필요하게 변경하지 않는다
- 불필요한 리팩터링 금지
- 프로토콜 스키마는 코드로 검증한다
- 자연어 라우팅에 의존하지 않는다
- 질문형 UX를 만들지 않는다
- 사용자 턴 의존 상태 확인 구조를 만들지 않는다
- Windows Terminal UI에 세션 제어를 의존하지 않는다
- provider별 특수 로직이 오케스트레이터 코어에 새지 않도록 분리한다
- Windows, macOS, Linux 중 하나에만 성립하는 코어 설계를 만들지 않는다

## Self-Hosting Roadmap

Gorchera는 장기적으로 이 프로젝트 자체를 Gorchera 하네스로 개발할 수 있어야 한다.

단계 구분:
- Phase 0: 수동 개발용 MVP
- Phase 1: mock provider 기반 내부 루프 검증
- Phase 2: 실제 provider adapter 연결
- Phase 3: evaluator gate와 runtime harness 추가
- Phase 4: 이 저장소 자체를 Gorchera가 보조적으로 수정하고 검증
- Phase 5: 사람 감독 하의 self-hosting loop

MVP 단계에서는 self-hosting이 완전하지 않아도 된다.
중요한 것은 later phase로 갈 수 있는 구조적 경계를 지금부터 깨지 않는 것이다.

## Instruction to Codex

이 문서를 기준으로 오케스트레이터를 구현해라.

구현 순서는 다음과 같다.

1. 상태 모델과 메시지 스키마 정의
2. 오케스트레이터 메인 루프 구현
3. A/B/C/D 실행 인터페이스 구현
4. JSON 검증 및 정책 검사 구현
5. artifact 전달 구조 구현
6. retry / blocked / done 처리 구현

먼저 최소 동작 버전을 만든 뒤 점진적으로 확장해라.
대화형 assistant처럼 구현하지 말고 무인 워크플로 엔진처럼 구현해라.
