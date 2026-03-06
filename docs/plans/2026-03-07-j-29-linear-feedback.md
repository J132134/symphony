# J-29 Linear Feedback Implementation Plan

**Goal:** 에이전트 실행 결과를 Linear 이슈 코멘트와 상태 전환으로 자동 피드백한다.
**Architecture:** `onWorkerDone`에서 성공/최종 실패를 분기하고, Linear tracker client mutation을 통해 코멘트와 상태 전환을 수행한다. 코멘트 본문은 워크스페이스 git 메타데이터와 토큰/지속시간 정보를 조합해 생성하며, 설정으로 기능을 제어한다.
**Tech Stack:** Go 1.22, Linear GraphQL, `os/exec`, `log/slog`

---

### Task 1: 설정 표면 추가

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Step 1: tracker feedback 설정과 `agent.max_attempts` getter를 추가한다**

**Step 2: 기본값과 override 동작을 테스트한다**

---

### Task 2: Linear tracker mutation 확장

**Files:**
- Modify: `internal/tracker/linear.go`

**Step 1: `AddComment(ctx, issueID, body)`를 추가한다**

**Step 2: 상태 이름으로 issue state를 전환하는 mutation helper를 추가한다**

---

### Task 3: orchestrator 결과 피드백 구현

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Create: `internal/orchestrator/linear_feedback.go`
- Test: `internal/orchestrator/orchestrator_test.go`

**Step 1: 성공 시 Linear 코멘트와 옵션 상태 전환을 수행한다**

**Step 2: 최종 실패(`max_attempts` 도달) 시에만 실패 코멘트와 옵션 상태 전환을 수행한다**

**Step 3: 재시도 큐 내부 지연은 시도 횟수를 소모하지 않도록 정리한다**

**Step 4: 성공/최종 실패/중간 실패 테스트를 추가한다**
