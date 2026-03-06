# Symphony 사용 가이드

Symphony는 Linear 이슈를 자동으로 폴링해서 이슈별 격리 워크스페이스를 생성하고,
코딩 에이전트(Codex 또는 Claude Code)를 실행해주는 오케스트레이터 데몬입니다.

---

## 설치

```bash
uv sync   # pyproject.toml 의존성 설치
```

---

## 빠른 시작

### 1. `WORKFLOW.md` 작성

프로젝트 루트에 `WORKFLOW.md`를 만듭니다.
앞부분 YAML front matter가 설정이고, `---` 뒤 나머지가 에이전트에게 전달되는 프롬프트 템플릿입니다.

```markdown
---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY          # 환경변수 참조 ($VAR 형태로 쓰면 자동 치환)
  project_slug: my-project          # Linear 프로젝트 slugId
  active_states: [Todo, In Progress]
  terminal_states: [Done, Cancelled, Duplicate]

polling:
  interval_ms: 10000                # 10초마다 폴링

workspace:
  root: ~/.symphony/workspaces      # 이슈별 작업 디렉토리가 생성될 루트

agent:
  max_concurrent_agents: 3          # 동시 에이전트 최대 수
  max_turns: 10                     # 에이전트 최대 턴 수
  max_retry_backoff_ms: 300000      # 재시도 최대 대기 시간 (5분)

codex:
  command: codex app-server         # 실행할 에이전트 명령어
  approval_policy: auto-edit
  turn_timeout_ms: 3600000          # 턴당 최대 1시간

server:
  port: 8080                        # 대시보드 포트 (생략하면 대시보드 비활성)
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

{% if issue.description %}
## Description
{{ issue.description }}
{% endif %}

{% if issue.labels %}Labels: {{ issue.labels | join(", ") }}{% endif %}

{% if attempt > 1 %}
Note: This is retry attempt #{{ attempt }}.
{% endif %}

Complete the issue. When done, update the issue state to "In Review".
```

### 2. 환경변수 설정

```bash
export LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxx
```

또는 프로젝트 루트 `.env` 파일에:

```
LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxx
```

### 3. 설정 검증

```bash
symphony validate --workflow WORKFLOW.md
```

출력 예시:
```
Workflow valid: /path/to/WORKFLOW.md
  Tracker: linear
  Project: my-project
  Active states: ['Todo', 'In Progress']
  Terminal states: ['Done', 'Cancelled', 'Duplicate']
  Max concurrent: 3
  Agent command: codex app-server
```

### 4. 실행

```bash
# 기본 실행
symphony run --workflow WORKFLOW.md

# 대시보드 포함
symphony run --workflow WORKFLOW.md --port 8080

# 로그 레벨 지정
symphony run --workflow WORKFLOW.md --log-level DEBUG
```

---

## 에이전트 선택

`codex.command` 첫 번째 단어가 `claude` 또는 `claude-code`이면 **ClaudeCodeRunner**,
그 외에는 **CodexRunner**가 자동으로 선택됩니다.

```yaml
# Codex (기본)
codex:
  command: codex app-server

# Claude Code
codex:
  command: claude
  turn_timeout_ms: 1800000
```

---

## HTTP 대시보드

`--port` 옵션 또는 `server.port` 설정이 있으면 HTTP 서버가 활성화됩니다.

| 엔드포인트 | 설명 |
|---|---|
| `GET /` | 실시간 HTML 대시보드 (10초 자동 갱신) |
| `GET /api/v1/state` | 현재 실행 상태 JSON |
| `GET /api/v1/{issue_identifier}` | 특정 이슈 상세 정보 |
| `POST /api/v1/refresh` | 즉시 폴링+조정 트리거 |

```bash
# 상태 확인
curl http://localhost:8080/api/v1/state | jq

# 특정 이슈 조회
curl http://localhost:8080/api/v1/MY-123

# 수동 새로고침
curl -X POST http://localhost:8080/api/v1/refresh
```

---

## Workspace Hooks

이슈별 작업 디렉토리 생명주기에 실행될 셸 스크립트를 정의할 수 있습니다.

```yaml
hooks:
  # 워크스페이스가 처음 생성될 때만 실행
  after_create: |
    git clone https://github.com/myorg/myrepo.git .
    npm install

  # 에이전트 실행 직전마다 실행 (실패하면 해당 시도 중단)
  before_run: |
    git fetch origin
    git rebase origin/main

  # 에이전트 종료 후 실행 (실패해도 무시)
  after_run: |
    git push origin HEAD

  # 워크스페이스 삭제 직전 실행 (실패해도 무시)
  before_remove: |
    echo "Cleaning up workspace"

  timeout_ms: 60000   # hook 타임아웃 (기본 60초)
```

스크립트 실행 시 `SYMPHONY_WORKSPACE` 환경변수로 현재 워크스페이스 절대경로가 전달됩니다.

---

## 프롬프트 템플릿

Jinja2 문법을 사용합니다. 사용 가능한 변수:

### `issue` 객체

| 필드 | 타입 | 설명 |
|---|---|---|
| `issue.id` | string | Linear 내부 ID |
| `issue.identifier` | string | 티켓 번호 (예: `MY-123`) |
| `issue.title` | string | 이슈 제목 |
| `issue.description` | string\|null | 이슈 설명 |
| `issue.priority` | int\|null | 우선순위 (1=긴급, 4=낮음) |
| `issue.state` | string | 현재 상태 (예: `In Progress`) |
| `issue.labels` | list[string] | 레이블 목록 (소문자) |
| `issue.url` | string\|null | Linear 이슈 URL |
| `issue.branch_name` | string\|null | 연결된 브랜치 이름 |

### `attempt`

| 값 | 의미 |
|---|---|
| `1` | 첫 번째 실행 |
| `2+` | 재시도 또는 continuation |

### 예시

```markdown
---
...
---
# {{ issue.identifier }}: {{ issue.title }}

{% if issue.priority == 1 %}⚠️ 긴급 이슈입니다.{% endif %}

## 작업 내용
{{ issue.description or "설명 없음" }}

{% if attempt > 1 %}
이전 시도가 실패했습니다 (attempt {{ attempt }}).
마지막으로 중단된 지점부터 이어서 작업하세요.
{% endif %}

## 완료 조건
- 변경사항을 커밋하고 PR을 생성하세요
- 이슈 상태를 "In Review"로 변경하세요
```

---

## 설정 기본값 (생략 시 적용)

| 항목 | 기본값 |
|---|---|
| `tracker.endpoint` | `https://api.linear.app/graphql` |
| `tracker.active_states` | `Todo, In Progress` |
| `tracker.terminal_states` | `Closed, Cancelled, Canceled, Duplicate, Done` |
| `polling.interval_ms` | `10000` (10초) |
| `workspace.root` | `~/.symphony/workspaces` |
| `hooks.timeout_ms` | `60000` (60초) |
| `agent.max_concurrent_agents` | `10` |
| `agent.max_turns` | `3` |
| `agent.max_retry_backoff_ms` | `300000` (5분) |
| `codex.command` | `codex app-server` |
| `codex.turn_timeout_ms` | `3600000` (1시간) |
| `codex.read_timeout_ms` | `5000` (5초) |
| `codex.stall_timeout_ms` | `300000` (5분) |

---

## 동작 흐름

```
Linear 폴링 (interval_ms 마다)
  ↓
활성 이슈 가져오기 (active_states 필터)
  ↓
우선순위 정렬: priority 오름차순 → 생성일 오래된 순 → identifier 사전순
  ↓
dispatch 가능한 이슈 선택:
  - running/claimed에 없을 것
  - global/per-state concurrency 여유 있을 것
  - Todo 상태면 blocker가 모두 terminal일 것
  ↓
워크스페이스 생성 (없으면 생성 + after_create hook)
  ↓
before_run hook 실행
  ↓
에이전트 프로세스 시작 → JSON-RPC 핸드셰이크
  ↓
turn 실행 (최대 max_turns회)
  → 각 turn 완료 후 이슈 상태 재확인
  → 여전히 활성이면 continuation turn
  → terminal/비활성이면 중단
  ↓
after_run hook 실행
  ↓
1초 후 continuation retry 예약
  (이슈가 여전히 활성이면 새 worker 세션 시작)
```

### 재시도 지연 시간

| 상황 | 지연 |
|---|---|
| 정상 종료 후 continuation | 1초 |
| 비정상 종료 attempt 1 | 10초 |
| 비정상 종료 attempt 2 | 20초 |
| 비정상 종료 attempt 3 | 40초 |
| 비정상 종료 attempt 4+ | 최대 5분 (설정 가능) |

### Reconciliation (매 폴링 tick마다)

- **stall 감지**: 마지막 에이전트 이벤트로부터 `stall_timeout_ms` 경과 시 강제 종료 후 재시도
- **상태 새로고침**: 실행 중인 이슈의 Linear 상태 확인
  - terminal 상태 → 에이전트 종료 + 워크스페이스 삭제
  - 비활성 상태 → 에이전트 종료 (워크스페이스 유지)
  - 활성 상태 → 계속 실행

---

## Linear 프로젝트 slugId 찾기

Linear 앱에서 Settings → Projects → 해당 프로젝트 → URL에서 확인:
```
https://linear.app/my-team/projects/my-project-abc123
                                     ^^^^^^^^^^^^^^^^
                                     이것이 project_slug
```

또는 Linear GraphQL API로 직접 조회:
```graphql
query {
  projects {
    nodes {
      name
      slugId
    }
  }
}
```

---

## 트러블슈팅

| 증상 | 원인 | 해결 |
|---|---|---|
| `tracker.api_key is required` | LINEAR_API_KEY 미설정 | `export LINEAR_API_KEY=...` |
| `tracker.project_slug is required` | project_slug 미설정 | WORKFLOW.md에 `tracker.project_slug` 추가 |
| 이슈가 dispatch 안 됨 | blocker 이슈가 비terminal | blocker 이슈 먼저 완료 |
| 에이전트가 stall됨 | 응답 없음 상태 | `stall_timeout_ms` 후 자동 재시도 |
| 워크스페이스 쌓임 | terminal 이슈 workspace 미정리 | 재시작 시 자동 정리 (startup cleanup) |
