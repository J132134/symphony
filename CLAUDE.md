# Symphony

## 프로젝트 개요

Linear 이슈를 자동 폴링해 이슈별 격리 워크스페이스를 생성하고, 코딩 에이전트(Codex/Claude Code)를 실행하는 오케스트레이터 데몬.

## 핵심 원칙: 메커니즘은 Go 코드, 정책/설정은 WORKFLOW.md

Symphony 설계의 가장 중요한 원칙이다.

**Go 코드가 담당하는 것 (메커니즘)**
- 언제 dispatch할지, 언제 retry할지, 언제 cancel할지
- concurrency 제한, stall 감지, drain 타임아웃
- Linear API 호출, 워크스페이스 생성/삭제, 에이전트 프로세스 실행
- 상태 정규화, 폴링 루프

**WORKFLOW.md가 담당하는 것 (정책/설정)**
- 어떤 상태에서 실행할지 (`active_states`)
- 어떤 상태에서 멈출지 (`pause_states`)
- 에이전트에게 무엇을 지시할지 (프롬프트 템플릿)
- 성공/실패 후 어떤 상태로 전환할지 (`on_success_state`, `on_failure_state`)
- 동시 실행 수, 재시도 횟수, 타임아웃 값

### 판단 기준

새 동작을 구현할 때:

```
이 값이 프로젝트마다 달라질 수 있는가?
  YES → WORKFLOW.md 설정 키로 노출
  NO  → Go 코드에 상수로 둬도 무방

이 로직이 "어떻게" 실행되는가 vs "무엇을" 실행하는가?
  "어떻게" (타이밍, 순서, 에러 처리) → Go 코드
  "무엇을" (값, 문자열, 조건 목록)   → WORKFLOW.md
```

### 위반 패턴 (하지 말 것)

```go
// BAD: 상태 이름을 Go 코드에 하드코딩
const humanReviewState = "human review"
if normalize(issue.State) == humanReviewState { ... }

// GOOD: 설정에서 읽은 pause_states로 판단
if isPauseState(cfg, issue.State) { ... }
```

```go
// BAD: 프롬프트를 Go 포맷 문자열로 하드코딩
return fmt.Sprintf("Continue working on %s: %s. This is turn %d of %d.", ...)

// GOOD: WORKFLOW.md continuation_prompt 템플릿 렌더링
workflow.RenderContinuation(wf, issueCtx, turnNum, maxTurns)
```

## 새 설정 키 추가 시

1. `config.go`에 typed getter 메서드 추가
2. `config_test.go`에 기본값 + override 테스트 추가
3. `README.md` 설정 기본값 표에 추가
4. 새 동작의 파라미터를 Go 상수로만 두지 말고 반드시 WORKFLOW.md에 노출

## 테스트 작성 가이드

- 유닛 테스트에서 Linear API는 인터페이스로 mock (`linearClient` interface)
- `config.New(map[string]any{...})`로 인메모리 설정 생성
- WORKFLOW.md 파일이 필요한 테스트: `t.TempDir()`에 임시 파일 생성
- 상태명 테스트: 정규화된 소문자("plan review")와 원본 케이스("Plan Review") 모두 커버
- 테이블 드리븐 테스트 선호 (`for _, tc := range tests`)

## 자주 실수하는 것들

1. **상태 하드코딩**: `"Human Review"` 문자열을 Go에 직접 쓰지 말고 `isPauseState()` 사용
2. **프롬프트 하드코딩**: 에이전트에 보내는 텍스트는 WORKFLOW.md 템플릿 경유
3. **설정 노출 누락**: 새 동작의 파라미터를 Go 상수로만 두고 WORKFLOW.md에 노출 안 함
4. **상태 비교 시 정규화 누락**: `issue.State == "Todo"` 대신 `NormalizeState(issue.State) == "todo"`
5. **에러 처리 누락**: `onWorkerDone`에서 Linear 피드백 실패는 워커를 실패로 만들지 않고 경고 로그만
