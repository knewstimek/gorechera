# CLAUDE.md

Gorechera -- Go 상태 기반 멀티 에이전트 오케스트레이션 엔진 (대화형 비서 아님, 워크플로 엔진).

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
- MCP server 구현 완료 (10개 도구, notification 지원)
- Leader 프롬프트에 sprint contract + strictness 규칙 삽입
- Evaluator retry loop (blocked -> leader 재시도, 무한루프 방어)
- GitHub: https://github.com/knewstimek/gorechera

## 문서 읽기 순서

1. `docs/ARCHITECTURE.md` -- 패키지 구조, 상태 머신, 핵심 루프, API 라우팅
2. `docs/IMPLEMENTATION_STATUS.md` -- 현재 상태, 수정된 버그, 미구현 목록
3. `docs/PRINCIPLES.md` -- 대원칙 상세
4. `docs/CODING_CONVENTIONS.md` -- 코딩 규칙, 패턴, 확장 가이드
5. `docs/ORCHESTRATOR_SPEC_UPDATED.md` -- 상세 설계 스펙

## 코드 진입점

- `cmd/gorechera/main.go` -- CLI
- `internal/orchestrator/service.go` -- 핵심 루프
- `internal/provider/provider.go` -- 어댑터 인터페이스
- `internal/provider/protocol.go` -- 프롬프트/스키마

## 작업 규칙

- 코드 변경 후 `go build ./...` && `go test ./...` 필수
- 변경 사항에 맞춰 `docs/` 문서 업데이트
- 새 기능은 `docs/CODING_CONVENTIONS.md`의 확장 가이드 참조
- 의도 전달이 필요한 로직에는 주석 필수 (특히 "왜 이렇게 했는지")
