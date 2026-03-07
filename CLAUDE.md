# Symphony — CLAUDE.md

## 프로젝트 개요

Linear 이슈를 자동 폴링해 이슈별 격리 워크스페이스를 생성하고, 코딩 에이전트(Codex/Claude Code)를 실행하는 오케스트레이터 데몬.

## 빌드 & 테스트

```bash
go build ./...
go test ./...
go vet ./...
```

설치: `make install` → `~/.local/bin/symphony`

## 패키지 구조

```
cmd/symphony/        CLI 진입점 (run, daemon, validate, menubar)
internal/
  agent/             에이전트 프로세스 실행 (JSON-RPC over stdin/stdout)
  config/            WORKFLOW.md YAML front matter 파싱 → SymphonyConfig
  daemon/            멀티 프로젝트 매니저, hot-reload, 자동 업데이트
  orchestrator/      메인 오케스트레이션 루프 (dispatch, reconcile, retry)
  tracker/           Linear GraphQL 클라이언트
  workflow/          WORKFLOW.md 로드 + pongo2 템플릿 렌더링
  workspace/         이슈별 디렉토리 생성/삭제, hooks 실행
  status/            HTTP 상태 API (/api/v1/summary)
  menubar/           macOS 메뉴바 UI
  filewatch/         파일 변경 감지 (config hot-reload)
  update/            GitHub Releases 자동 업데이트
```

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

## 설정 계층

```
WORKFLOW.md front matter     ← 프로젝트별 설정 (tracker, agent, codex, hooks 등)
~/.config/symphony/config.yaml ← 데몬 전역 설정 (멀티 프로젝트, global concurrency)
```

`SymphonyConfig` (`internal/config/config.go`)는 WORKFLOW.md front matter를 파싱한다. 새 설정 키를 추가할 때는:
1. `config.go`에 typed getter 메서드 추가
2. `config_test.go`에 기본값 + override 테스트 추가
3. README.md 설정 기본값 표에 추가

## 상태 처리 규칙

- 상태 비교는 항상 `config.NormalizeState()` (소문자 + trim) 적용 후 수행
- `isPauseState(cfg, state)` — pause_states에 속하는지 확인
- `isTerminalState(cfg, state)` — terminal_states에 속하는지 확인
- 특정 상태 이름을 Go 코드에 직접 문자열로 쓰지 않는다

## 프롬프트 템플릿

`internal/workflow/workflow.go`:
- `workflow.Render(def, issueCtx, attemptNum)` — 최초 프롬프트 (전체 WORKFLOW.md 템플릿)
- `workflow.RenderContinuation(def, issueCtx, turnNum, maxTurns)` — turn 2+ 연속 프롬프트

템플릿 엔진: pongo2 (Django/Jinja2 호환). 사용 가능한 변수:

| 변수 | 설명 |
|------|------|
| `issue.identifier` | 티켓 번호 (J-123) |
| `issue.title` | 이슈 제목 |
| `issue.state` | 현재 상태 |
| `issue.description` | 설명 (없으면 null) |
| `issue.labels` | 레이블 목록 |
| `issue.url` | Linear URL |
| `issue.branch_name` | 연결 브랜치 |
| `attempt` | 시도 횟수 (1=최초) |
| `turn_context` | 워크스페이스 진행 요약 |
| `turn_num` | 현재 턴 번호 (continuation only) |
| `max_turns` | 최대 턴 수 (continuation only) |

## 오케스트레이터 핵심 흐름

```
reconcile() → tick마다 호출
  ↓
canDispatch(cfg, issue) → pause/terminal/running/concurrency 체크
  ↓
dispatch() → goroutine 시작
  ↓
runAttempt()
  ├── workspace.Setup() + PrepareForRun() (before_run hook)
  ├── workflow.Render() → 초기 프롬프트
  ├── agent.StartSession()
  ├── for turn 1..maxTurns:
  │     turn 1: 초기 프롬프트
  │     turn 2+: workflow.RenderContinuation()
  │     runner.RunTurn() → 에이전트 결과 대기
  │     isStillActive() → terminal이면 중단
  └── workspace.FinishRun() (after_run hook)
  ↓
onWorkerDone() → 성공/실패 피드백, 재시도 예약
```

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
