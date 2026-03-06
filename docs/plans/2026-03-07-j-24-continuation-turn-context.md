# J-24 Continuation Turn Context Implementation Plan

**Goal:** turn 2+ continuation 프롬프트와 재시도 프롬프트가 이전 workspace 진행 상황을 바로 활용하게 만든다.
**Architecture:** `workspace.Manager`가 git diff/log 및 마지막 `after_run` stdout을 turn context 문자열로 조합한다. `orchestrator.runAttempt`는 초기 `workflow.Render`와 turn 2+ continuation prompt에서 이 context를 사용하고, 조회 실패 시 기존 프롬프트로 fallback 한다.
**Tech Stack:** Go 1.22, `os/exec`, git CLI, pongo2 templates

---

### Task 1: Workspace turn context 수집

**Files:**
- Modify: `internal/workspace/workspace.go`
- Test: `internal/workspace/workspace_test.go`

**Step 1: hook stdout을 반환하고 after_run 출력 저장을 구현한다**

**Step 2: `GetTurnContext`로 git diff/log + hook output을 조합한다**

**Step 3: workspace 테스트로 persistence와 fallback 에러를 검증한다**

### Task 2: Orchestrator / workflow prompt 연결

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Modify: `internal/workflow/workflow.go`
- Test: `internal/orchestrator/orchestrator_test.go`
- Test: `internal/workflow/workflow_test.go`

**Step 1: `workflow.Render`에 `turn_context` 템플릿 변수를 노출한다**

**Step 2: turn 2+ continuation prompt builder를 추가한다**

**Step 3: context 미존재/조회 실패 시 기존 one-line prompt로 fallback 한다**

**Step 4: 관련 unit test를 추가한다**
