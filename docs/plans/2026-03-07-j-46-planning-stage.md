# J-46 Planning Stage Implementation Plan

**Goal:** Todo와 In Progress 사이에 Planning pause state를 추가하고, pause-state 판정을 설정 기반으로 일반화한다.
**Architecture:** `SymphonyConfig`가 `tracker.pause_states`를 파싱해 정규화된 집합을 제공하고, orchestrator가 dispatch/retry/reconcile/concurrency/feedback 경로에서 이 집합을 공통으로 사용한다. 문서는 README 예시와 운영용 `WORKFLOW.md`에 Planning 흐름을 반영해 상태 전이와 대기 규칙을 명확히 한다.
**Tech Stack:** Go 1.22, Linear GraphQL, `log/slog`, `gopkg.in/yaml.v3`

---

### Task 1: Pause state 설정 추가

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Step 1: `tracker.pause_states`를 파싱하는 `PauseStates()`와 `PauseNorm()`을 추가한다**

**Step 2: 설정이 없을 때 `Human Review`를 기본값으로 유지하는 테스트를 추가한다**

**Step 3: custom pause states 정규화 동작 테스트를 추가한다**

---

### Task 2: Orchestrator pause-state 일반화

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Modify: `internal/orchestrator/dispatch.go`
- Modify: `internal/orchestrator/capacity.go`
- Modify: `internal/orchestrator/reconcile.go`
- Modify: `internal/orchestrator/retry.go`
- Modify: `internal/orchestrator/linear_feedback.go`
- Test: `internal/orchestrator/orchestrator_test.go`

**Step 1: 공통 `isPauseState` 헬퍼를 추가한다**

**Step 2: dispatch/retry/reconcile/concurrency/feedback 경로를 `cfg.PauseNorm()` 기반으로 교체한다**

**Step 3: Planning 같은 custom pause state가 dispatch와 concurrency에서 제외되는 테스트를 추가한다**

---

### Task 3: Planning 문서화

**Files:**
- Modify: `README.md`
- Modify: `WORKFLOW.md` (local ignored file)

**Step 1: README 예시에 `pause_states`와 Planning 상태를 반영한다**

**Step 2: 운영용 `WORKFLOW.md`에 Todo → Planning → In Progress 흐름과 pause-state 설정을 반영한다**
