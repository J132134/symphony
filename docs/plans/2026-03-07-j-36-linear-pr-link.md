# J-36 Linear PR Link Implementation Plan

**Goal:** Human Review 전환 시 Linear 이슈의 Add link에 PR URL을 자동으로 추가한다.
**Architecture:** 성공 피드백 경로에서 워크스페이스 요약으로 계산한 PR URL을 재사용하고, `tracker.LinearClient`에 attachment mutation helper를 추가해 상태 전환 직후 링크를 생성한다. 테스트 서버 recorder를 확장해 코멘트, 상태 전환, 링크 생성이 함께 검증되도록 한다.
**Tech Stack:** Go 1.22, Linear GraphQL, `log/slog`, `net/http/httptest`

---

### Task 1: Linear tracker 링크 mutation 추가

**Files:**
- Modify: `internal/tracker/linear.go`
- Test: `internal/tracker/linear_test.go`

**Step 1: `AddLink(ctx, issueID, title, url)` helper를 추가한다**

**Step 2: 입력 검증과 mutation 실패 케이스를 테스트한다**

---

### Task 2: Human Review 성공 피드백 경로 연결

**Files:**
- Modify: `internal/orchestrator/linear_feedback.go`
- Test: `internal/orchestrator/orchestrator_test.go`

**Step 1: 성공 경로에서 workspace summary를 1회 수집해 코멘트/링크 생성에 재사용한다**

**Step 2: `on_success_state`가 Human Review이고 PR URL이 있으면 상태 전환 직후 링크를 추가한다**

**Step 3: recorder 테스트에 링크 생성 검증을 추가한다**

---

### Task 3: 사용자 문서 반영

**Files:**
- Modify: `README.md`

**Step 1: Human Review 전환 시 PR 링크를 Add link로도 남긴다는 동작을 문서화한다**
