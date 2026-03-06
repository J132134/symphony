# J-23 notifCh terminal event protection plan

**Goal:** `notifCh`가 포화되어도 turn 종료 계열 이벤트가 드롭되지 않도록 보장한다.
**Architecture:** `internal/agent/codex.go`에서 turn 종료 계열 notification을 별도 고우선순위 채널로 분리하고, `consumeUntilDone`가 일반 notification보다 먼저 혹은 병행해서 이를 소비하도록 만든다. 일반 notification의 기존 best-effort 동작은 유지한다.
**Tech Stack:** Go 1.22, JSON-RPC agent runner, `testing`

---

### Task 1: 종료 notification 우선 경로 추가

**Files:**
- Modify: `internal/agent/codex.go`

**Step 1: 일반 notification과 종료 notification 분기 지점을 식별한다**
`dispatchLine`에서 notification을 하나의 `notifCh`로만 보내는 현재 구조를 확인한다.

**Step 2: 종료 notification 전용 채널을 추가한다**
`turn/completed`, `turn/failed`, `turn/cancelled`를 별도 채널로 라우팅하고, 일반 notification만 기존 `notifCh`를 사용한다.

**Step 3: 소비 루프를 갱신한다**
`RunTurn` 시작 시 두 채널을 모두 비우고, `consumeUntilDone`에서 종료 notification 채널을 함께 읽도록 수정한다.

### Task 2: 포화 상황 회귀 테스트 추가

**Files:**
- Create: `internal/agent/codex_test.go`

**Step 1: 실패 재현 테스트를 만든다**
일반 notification으로 `notifCh`를 가득 채운 뒤 `turn/completed`를 보내도 turn이 정상 종료되는지 검증한다.

**Step 2: 일반 notification 드롭 동작 유지 여부를 확인한다**
일반 notification은 채널 포화 시 여전히 best-effort로 드롭되는지 확인한다.

### Task 3: 검증

**Files:**
- Test: `internal/agent/codex_test.go`

**Step 1: 변경 범위 테스트를 실행한다**
`go test ./internal/agent`

**Step 2: 전체 검증을 실행한다**
`go build ./...`
`go test ./...`
