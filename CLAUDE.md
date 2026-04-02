# CLAUDE.md

Gorchera -- Go 상태 기반 멀티 에이전트 오케스트레이션 엔진 (대화형 비서 아님, 워크플로 엔진).

## 빌드 & 검증

```bash
go build ./...
go test ./...
```

## 핵심 금기사항 (상세: `docs/PRINCIPLES.md`)

- evaluator gate 우회 금지 -- complete은 반드시 evaluateCompletion() 통과 후 done
- agent 간 전체 대화 로그 전달 금지 -- artifact + summary만
- executor가 자체 worker spawn 금지 -- 병렬은 orchestrator 주도
- approval 필요 작업 자동 통과 금지
- global harness와 job-scoped harness semantics 혼합 금지
- Windows 전용 코드를 코어에 넣지 않음 -- cross-platform 중립
- 비ASCII 특수문자(em dash 등) 코드/출력에 금지 -- cp949 깨짐
- 의도가 불명확한 코드에는 반드시 주석 -- "왜"를 설명, "무엇"은 코드로

## 현재 상태 (상세: `docs/IMPLEMENTATION_STATUS.md`)

- Known Bugs 전부 수정 (TOCTOU 포함)
- Claude + Codex(GPT) 어댑터 실연동 완료
- normal + strict 모드 done 수렴 달성 (GPT-only 파이프라인)
- MCP server 구현 완료 (17+ 도구, notification 지원)
- Leader 프롬프트에 sprint contract + strictness 규칙 삽입
- Evaluator retry loop (blocked -> leader 재시도, 무한루프 방어)
- In-memory job cache -- status API 실시간 반영
- JobStatusPlanning -- planner 단계 가시성
- Audit V2 CRITICAL/HIGH 수정 (XSS-1, XSS-2, H1, H2, H3)
- Reviewer/Evaluator/Tester 프롬프트 하드닝 (역할별 분리)
- GitHub: https://github.com/knewstimek/gorchera

## 문서 읽기 순서

1. `docs/ARCHITECTURE.md` -- 패키지 구조, 상태 머신, 핵심 루프, API 라우팅
2. `docs/IMPLEMENTATION_STATUS.md` -- 현재 상태, 수정된 버그, 미구현 목록
3. `docs/PRINCIPLES.md` -- 대원칙 상세
4. `docs/CODING_CONVENTIONS.md` -- 코딩 규칙, 패턴, 확장 가이드
5. `docs/ORCHESTRATOR_SPEC_UPDATED.md` -- 상세 설계 스펙

## 코드 진입점

- `cmd/gorchera/main.go` -- CLI
- `internal/orchestrator/service.go` -- 핵심 루프
- `internal/provider/provider.go` -- 어댑터 인터페이스
- `internal/provider/protocol.go` -- 프롬프트/스키마

## 기본 Role Profile (감독관 권장)

provider=codex 기준, executor/reviewer/tester만 claude sonnet으로 override:

```json
{
  "provider": "codex",
  "role_overrides": {
    "executor":  {"provider": "claude", "model": "sonnet"},
    "reviewer":  {"provider": "claude", "model": "sonnet"},
    "tester":    {"provider": "claude", "model": "sonnet"}
  }
}
```

결과: planner/leader/evaluator = GPT 5.4, executor/reviewer/tester = Claude Sonnet.
GPT가 계획/판단, Claude가 실행/검토. 토큰 효율 + 크로스체크 효과.

## 감독관 Goal 작성 가이드

Goal의 품질이 job 결과를 결정한다. "XSS 고쳐" 같은 짧은 goal은 기계적 실행만 유도한다.

### Goal 템플릿

```
Objective: [무엇을 달성하려는가]
Why: [왜 필요한가 -- 비즈니스/UX 영향, 실제 겪은 문제]
In-scope: [변경할 파일/모듈/기능]
Out-of-scope: [건드리지 말 것]
Invariants: [절대 깨뜨리면 안 되는 것 -- recovery 로직, 상태 머신, 기존 테스트 등]
Constraints: [기술적 제약 -- ASCII only, 새 파일 금지, 특정 패턴 사용 등]
Done when: [완료 기준 -- 빌드 통과, 특정 동작 확인 등]
```

### Goal 야망 수준

- **최소 달성 (fix)**: "H2 토큰 비교를 constant-time으로 변경"
- **목표 달성 (implement)**: "audit V2의 CRITICAL/HIGH 5건 수정 + 빌드/테스트 통과"
- **초과 달성 (improve)**: "status API가 감독관에게 실시간 가시성을 제공하지 못해서 멀쩡한 job을 두 번 죽였다. 근본 원인을 해결하되, 향후 비슷한 가시성 문제가 재발하지 않는 구조를 제안하라"

야망 수준이 높을수록 worker에게 더 많은 맥락과 자율성이 필요하다.

### 불변식 (Invariants) 작성법

감독관만 아는 시스템 지식을 goal에 명시해야 한다. planner는 코드를 읽지만 운영 경험은 없다.

예시:
- "recovery 로직(RecoverJobs, InterruptRecoverableJobs)은 MCP 재시작 시 실행됨 -- 무한 resume 루프 주의"
- "addEvent()는 cache 즉시 업데이트해야 함 -- status API stale 버그 재발 방지"
- "Cancel과 runLoop이 동시 실행될 수 있음 -- race condition 고려"

## 작업 규칙

- 코드 변경 후 `go build ./...` && `go test ./...` 필수
- 변경 사항에 맞춰 `docs/` 문서 업데이트
- 새 기능은 `docs/CODING_CONVENTIONS.md`의 확장 가이드 참조
- 의도 전달이 필요한 로직에는 주석 필수 (특히 "왜 이렇게 했는지")
