# J-40 Human Review Concurrency Implementation Plan

**Goal:** Claude로 처리하지 않는 프로젝트의 `Human Review` 상태는 concurrent 계산에서 제외하고, 해당 상태에서는 서브프로세스를 내렸다가 다른 active 상태로 바뀌면 다시 진행되게 만든다.
**Architecture:** `codex.command`와 `codex.state_commands`를 기준으로 특정 상태가 Claude로 실행되는지 판단하는 설정 헬퍼를 추가한다. 오케스트레이터는 이 헬퍼를 사용해 `Human Review` 상태의 dispatch/global concurrency/reconcile/retry 동작을 분기한다.
**Tech Stack:** Go 1.22, `log/slog`, Linear tracker client, Go tests

---

### Task 1: Claude 처리 상태 판단 헬퍼 추가

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Step 1: `codex.command`/`state_commands`에서 Claude 실행 여부를 판단하는 메서드를 추가한다**

**Step 2: 기본 커맨드와 상태별 override 조합을 검증하는 테스트를 추가한다**

### Task 2: Human Review dispatch/concurrency 로직 분기

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_test.go`

**Step 1: concurrent/global limiter 계산을 설정 기반으로 바꾼다**

**Step 2: Claude를 쓰지 않는 `Human Review` 상태는 dispatch하지 않도록 막는다**

**Step 3: 이미 실행 중인 이슈가 manual `Human Review`로 바뀌면 reconcile에서 프로세스를 내리도록 한다**

**Step 4: retry 경로에서 manual `Human Review`는 claim을 풀고 대기하도록 만든다**

### Task 3: 사용자 문서 갱신

**Files:**
- Modify: `README.md`
- Modify: `WORKFLOW.md`

**Step 1: `Human Review`가 Claude로 실행될 때만 concurrent를 차지한다는 규칙을 문서화한다**

**Step 2: manual review 프로젝트에서는 `Human Review`에서 서브프로세스를 내린다는 동작을 예시와 함께 정리한다**
