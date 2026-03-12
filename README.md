# Symphony

Linear 이슈를 자동으로 폴링해서 이슈별 격리 워크스페이스를 생성하고, 코딩 에이전트(Codex 또는 Claude Code)를 실행해주는 오케스트레이터 데몬.

## 설치

### 요구사항

- macOS (메뉴바 UI)
- [Codex CLI](https://github.com/openai/codex) 또는 [Claude Code](https://claude.ai/code)

### 바이너리 설치 (권장)

```bash
curl -fsSL https://raw.githubusercontent.com/J132134/symphony/main/scripts/install.sh | bash
```

`~/.local/bin/symphony`에 최신 릴리스 바이너리를 설치한다.

**LaunchAgent(데몬 + 메뉴바 자동 시작)까지 한 번에 설치:**

```bash
curl -fsSL https://raw.githubusercontent.com/J132134/symphony/main/scripts/install.sh | bash -s -- --with-launchagents
```

`LINEAR_API_KEY` 환경변수가 설정되어 있으면 plist에 자동으로 주입된다.

### 업그레이드

```bash
curl -fsSL https://raw.githubusercontent.com/J132134/symphony/main/scripts/install.sh | bash
```

### 설치 확인

```bash
launchctl list | grep symphony
# com.symphony.daemon   → 데몬
# com.symphony.menubar  → 메뉴바
```

### 소스에서 빌드 (개발용)

```bash
git clone https://github.com/J132134/symphony.git
cd symphony

# 바이너리 빌드 → ~/.local/bin/symphony
make install

# LaunchAgent 등록
make install-launchagents
```

`make install-launchagents`는 `scripts/` 안의 plist 템플릿에서 현재 홈 디렉토리와 로그 디렉토리를 채워 `~/Library/LaunchAgents/`에 설치한다. 로그는 `~/Library/Logs/Symphony`에 기록되며, launchd 작업 디렉터리도 레포로 고정하지 않으므로 레포가 `Documents`나 `Desktop` 아래 있어도 데몬 시작만으로 해당 보호 폴더 접근 알림을 불필요하게 띄우지 않는다.

## 빠른 시작

### 1. `WORKFLOW.md` 작성

프로젝트 루트에 `WORKFLOW.md`를 만든다. YAML front matter가 설정이고, `---` 이후가 에이전트에 전달되는 Jinja2 호환 프롬프트 템플릿이다.

```markdown
---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
  active_states: [Todo, Plan Review, In Progress, Human Review]
  pause_states: [Plan Review, Human Review]
  terminal_states: [Done, Cancelled, Duplicate]
  post_comments: true
  on_success_state: Human Review
  on_failure_state: ""

polling:
  interval_ms: 10000

workspace:
  root: ~/.symphony/workspaces

agent:
  max_concurrent_agents: 3
  max_concurrent_agents_by_state:
    Merging: 1
  max_attempts: 3
  max_turns: 10

codex:
  command: codex app-server
  approval_policy: auto-edit
  turn_timeout_ms: 3600000

daemon:
  drain_timeout_ms: 360000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

{% if issue.description %}
## Description
{{ issue.description }}
{% endif %}

Complete the issue. When done, update the issue state to "In Review".
```

### 2. 환경변수 설정

```bash
export LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxx
```

또는 `.env` 파일:

```
LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxx
```

### 3. 실행

```bash
# 설정 검증
symphony validate --workflow WORKFLOW.md

# 실행
symphony run --workflow WORKFLOW.md

# 대시보드 포함
symphony run --workflow WORKFLOW.md --port 8080
```

## 멀티 프로젝트 데몬

여러 프로젝트를 단일 프로세스로 실행하려면 `~/.config/symphony/config.yaml`을 작성한다.

```yaml
projects:
  - name: backend
    workflow: ~/projects/backend/WORKFLOW.md
  - name: frontend
    workflow: ~/projects/frontend/WORKFLOW.md

agent:
  max_total_concurrent_sessions: 4

status_server:
  enabled: true
  port: 7777

webhook:
  enabled: false
  port: 7778
  bind_address: 127.0.0.1
  signing_secret: $LINEAR_WEBHOOK_SECRET

auto_update:
  enabled: true
  interval_minutes: 30
```

```bash
symphony daemon
# 또는 커스텀 설정 경로
symphony daemon --config /path/to/config.yaml

# mac 메뉴바 상태 표시
symphony menubar
```

`symphony daemon`은 실행 중에도 `config.yaml`의 변경을 감지해 설정을 다시 읽는다. 새 설정이 유효하면 프로젝트 목록 diff를 계산해 바뀐 프로젝트만 선택적으로 시작, 교체, 종료하고, 변경 없는 프로젝트는 그대로 유지한다. `status_server`, `webhook`, `auto_update`, `agent.max_total_concurrent_sessions` 같은 daemon 전역 설정이 바뀐 경우에만 상태 서버, webhook 서버, update loop를 포함한 전체 runtime을 다시 띄운다. 유효하지 않은 설정은 적용하지 않고 기존 실행 상태를 유지한 채 오류만 로그에 남긴다.

`config.yaml` 안의 상대 경로(`projects[].workflow`)는 현재 셸의 작업 디렉터리가 아니라 `config.yaml` 파일이 있는 디렉터리 기준으로 해석된다. launch agent가 특정 레포 디렉터리를 작업 디렉터리로 잡지 않아도 동일하게 동작하도록 하기 위한 동작이다.

`agent.max_total_concurrent_sessions`는 데몬 전체에서 동시에 실행할 수 있는 에이전트 세션 수 상한이다. 값을 생략하면 실행 중인 머신의 CPU 개수를 기준으로 동적으로 계산한다: `NumCPU() <= 2`면 `1`, `<= 4`면 `2`, 그 외에는 `NumCPU()/2`를 사용하되 최대 `8`로 제한한다. 각 프로젝트의 `WORKFLOW.md`에 있는 `agent.max_concurrent_agents`와 `agent.max_concurrent_agents_by_state`는 그대로 유지되며, 실제 dispatch는 `프로젝트별 전체 제한`, `상태별 제한`, `데몬 전체 제한`을 모두 만족해야 한다.

각 프로젝트의 `WORKFLOW.md`에는 `daemon.drain_timeout_ms`를 둘 수 있다. graceful drain의 기본값은 `codex.stall_timeout_ms + hooks.timeout_ms`이며, hot-reload나 shutdown 중에는 새 작업을 막은 뒤 현재 turn/tool call과 `after_run`, `before_remove` 훅이 이 상한 안에서 끝나기를 기다린다. 상한을 넘기면 Codex subprocess는 `SIGTERM`, 10초 후 `SIGKILL` 순서로 종료된다. full runtime reload는 `new runtime start -> old runtime drain` 순서로 실행돼 세션 공백을 줄인다.

## Webhook 연동

Linear webhook은 데몬 모드에서만 사용된다. webhook이 켜지면 각 프로젝트 orchestrator는 짧은 active/idle 폴링 대신 `polling.webhook_fallback_interval_ms` 간격의 장주기 폴백 폴링만 유지하고, 실제 즉시 refresh는 `/webhook/linear` 요청으로 트리거된다.

```yaml
webhook:
  enabled: true
  port: 7778
  bind_address: 127.0.0.1
  signing_secret: $LINEAR_WEBHOOK_SECRET
```

Linear에서 `Settings -> API -> Webhooks`로 들어가 URL을 `https://<public-host>/symphony/webhook/linear` 형태로 등록하면 된다. Symphony는 `/webhook/linear`를 직접 수신하고, reverse proxy가 `/symphony` 접두사를 제거하는 구조를 가정한다.

외부 공개 HTTPS 엔드포인트와 reverse proxy(Caddy, Tailscale Funnel 등)는 Symphony 저장소 범위 밖에서 관리한다. 일반적인 구성은 `handle_path /symphony/*`를 `127.0.0.1:7778`로 프록시하고, Caddy 헬스체크는 `GET /symphony/webhook/health`에 연결하는 방식이다.

`webhook.signing_secret`를 비워두면 개발 모드로 동작하며, 서명 검증 없이 200을 반환하고 refresh를 수행한다. 값이 있으면 `Linear-Signature` HMAC-SHA256 검증에 성공한 Issue 이벤트만 refresh를 트리거한다. 검증 실패나 잘못된 payload는 재시도를 피하기 위해 항상 200으로 응답하고 로그만 남긴다.

## 에이전트 선택

`codex.command` 첫 번째 단어가 `claude` 또는 `claude-code`이면 Claude Code가 사용되고, 그 외에는 Codex가 사용된다.

```yaml
# Codex (기본)
codex:
  command: codex app-server

# Claude Code
codex:
  command: claude
```

## 상태 확인

단일 프로젝트 실행(`symphony run`)에서는 `--port` 옵션으로 status server를 켤 수 있고, 멀티 프로젝트 데몬(`symphony daemon`)에서는 `status_server.port` 설정으로 항상 같은 API를 노출한다.

```bash
# 기본 daemon 설정(~/.config/symphony/config.yaml)의 status_server.port 사용
symphony status

# 다른 호스트나 SSH 포트 포워딩 대상 조회
symphony status --url http://127.0.0.1:7777

# 자동화용 JSON 출력
symphony status --json
```

`--url`을 주지 않으면 daemon 설정 파일에서 `status_server.port`를 읽고, 설정 파일이 없으면 기본값 `http://127.0.0.1:7777`로 조회한다. 실행 중 이슈는 현재 단계(`status`), turn 수, 마지막 이벤트 시각까지 함께 출력한다.

원격 웹 대시보드는 이번 범위에 포함하지 않는다. 필요하면 동일한 status API를 SSH 포트 포워딩, Tailscale, reverse proxy 같은 운영 경로로 노출해 후속으로 붙일 수 있다.

## HTTP 상태 API

| 엔드포인트 | 설명 |
|---|---|
| `GET /api/v1/summary` | 메뉴바 UI용 데몬 요약 상태(JSON) |
| `GET /api/v1/projects` | 프로젝트별 상태와 실행 중 이슈 상세(JSON) |
| `POST /api/v1/refresh` | 즉시 폴링+조정 트리거 |

`symphony menubar`는 macOS 메뉴바에서 데몬 상태를 보여준다. 정상 실행 중에는 회전하는 원형 인디케이터를, 에러가 있으면 경고 아이콘을, status server 또는 tracker 연결이 끊기면 일시정지 아이콘을 표시한다. 마우스 오버 툴팁과 메뉴 항목에서 현재 버전, 실행 중인 서브프로세스 수, 이슈 ID 목록을 확인할 수 있다.

## Workspace Hooks

이슈별 작업 디렉토리 생명주기에 실행될 스크립트를 정의한다.

```yaml
hooks:
  after_create: |
    git clone https://github.com/myorg/myrepo.git .
    npm install
  before_run: |
    git fetch origin
    git rebase origin/main
  after_run: |
    git push origin HEAD
  before_remove: |
    echo "Cleaning up workspace"
  timeout_ms: 60000
```

스크립트 실행 시 `SYMPHONY_WORKSPACE` 환경변수로 현재 워크스페이스 절대경로가 전달된다.

`codex.turn_sandbox_policy: workspace-write`를 사용할 때 Symphony는 워크스페이스 디렉터리만 믿지 않고, 실행 직전 `git rev-parse --git-dir`와 `--git-common-dir`로 실제 git admin 경로를 해석해 `codex --add-dir`로 함께 전달한다. 따라서 일반 clone과 linked worktree 모두에서 `.git` metadata 쓰기가 가능하고, 워크스페이스를 프로젝트 하위로 옮기는 것만으로 해결되지는 않는다.

## Linear 피드백

기본적으로 Symphony는 에이전트 실행이 성공하면 Linear 이슈에 실행 요약 코멘트를 남기고, `tracker.on_success_state`가 pause state(`tracker.pause_states`)일 때는 확인된 실제 PR URL이 있을 때만 Add link도 함께 등록한다. 성공 코멘트는 turn 종료 직전에 캡처한 스냅샷만 사용하며, `Tokens`, `PR`, `Branch`, `Changes`는 신뢰 가능한 값이 확보된 경우에만 표시한다. 예를 들어 토큰 집계가 0이거나, workspace branch와 issue branch가 충돌하거나, clean worktree라 변경 파일을 확정할 수 없거나, GitHub에서 실제 PR을 찾지 못하면 해당 줄은 통째로 생략된다. 최종 실패(`agent.max_attempts` 초과) 시에만 실패 코멘트를 남기며, 코멘트/링크/상태 전환 등록 실패는 워커 실행을 실패로 만들지 않고 경고 로그만 남긴다.

```yaml
tracker:
  post_comments: true
  pause_states: [Plan Review, Human Review]
  on_success_state: Human Review
  on_failure_state: Rework

agent:
  max_concurrent_agents_by_state:
    Merging: 1
  max_attempts: 3
```

- `tracker.post_comments`: Linear 코멘트 등록 on/off. 기본값은 `true`.
- `tracker.pause_states`: active 상태 중 dispatch/retry/concurrency 계산에서 제외할 상태 목록. 기본값은 `["Human Review"]`.
- `tracker.on_success_state`: 성공 후 자동 전환할 상태 이름. 비우면 전환하지 않는다.
- `tracker.on_failure_state`: 최종 실패 후 자동 전환할 상태 이름. 비우면 전환하지 않는다.
- `agent.max_concurrent_agents_by_state`: 상태별 동시 실행 상한. 명시한 상태만 개별 quota를 적용하고, 나머지 활성 상태는 `agent.max_concurrent_agents` 전체 quota를 공유한다. 기본값은 비어 있음.
- `agent.max_attempts`: 워커 실행 최대 시도 횟수. 기본값은 `3`.

## 프롬프트 템플릿 변수

| 변수 | 타입 | 설명 |
|---|---|---|
| `issue.id` | string | Linear 내부 ID |
| `issue.identifier` | string | 티켓 번호 (예: `MY-123`) |
| `issue.title` | string | 이슈 제목 |
| `issue.description` | string\|null | 이슈 설명 |
| `issue.priority` | int\|null | 우선순위 (1=긴급, 4=낮음) |
| `issue.state` | string | 현재 상태 |
| `issue.labels` | list[string] | 레이블 목록 |
| `issue.url` | string\|null | Linear 이슈 URL |
| `issue.branch_name` | string\|null | 연결된 브랜치 이름 |
| `attempt` | int | 시도 횟수 (1=최초, 2+=재시도) |

## 설정 기본값

| 항목 | 기본값 |
|---|---|
| `polling.interval_ms` | `10000` (10초) |
| `polling.webhook_fallback_interval_ms` | `300000` (5분) |
| `workspace.root` | `~/.symphony/workspaces` |
| `agent.max_concurrent_agents` | `10` |
| `agent.max_concurrent_agents_by_state` | `없음` |
| `agent.max_attempts` | `3` |
| `agent.max_turns` | `20` |
| `agent.max_retry_backoff_ms` | `300000` (5분) |
| `tracker.post_comments` | `true` |
| `codex.command` | `codex app-server` |
| `codex.turn_timeout_ms` | `3600000` (1시간) |
| `codex.stall_timeout_ms` | `300000` (5분) |
| `hooks.timeout_ms` | `60000` (60초) |
| `daemon.drain_timeout_ms` | `codex.stall_timeout_ms + hooks.timeout_ms` (`360000`, 6분) |

### 데몬 전용 기본값

| 항목 | 기본값 |
|---|---|
| `agent.max_total_concurrent_sessions` | `동적` (`NumCPU() <= 2` → `1`, `<= 4` → `2`, 그 외 `NumCPU()/2`, 최대 `8`) |
| `webhook.enabled` | `false` |
| `webhook.port` | `7778` |
| `webhook.bind_address` | `127.0.0.1` |
| `webhook.signing_secret` | `없음` |

## 동작 흐름

```
Linear webhook (`/webhook/linear`)
  ↓
즉시 refresh trigger
  ↓
활성 이슈 fetch (active_states 필터)
  ↑
폴백 폴링 (`webhook_fallback_interval_ms`, daemon webhook 모드일 때)
  또는
일반 폴링 (`interval_ms` / `idle_interval_ms`)
  ↓
우선순위 정렬: priority 오름차순 → 생성일 오래된 순 → identifier 사전순
  ↓
dispatch 조건 확인:
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
  → 각 turn 후 이슈 상태 재확인 → terminal이면 중단
  ↓
after_run hook 실행
  ↓
재시도 예약 (정상 종료: 1초, 비정상: 지수 백오프)
```

### 재시도 지연

| 상황 | 지연 |
|---|---|
| 정상 종료 후 continuation | 1초 |
| 비정상 종료 attempt 1 | 10초 |
| 비정상 종료 attempt 2 | 20초 |
| 비정상 종료 attempt 3 | 40초 |
| 비정상 종료 attempt 4+ | 최대 5분 |

### Reconciliation (매 폴링 tick마다)

- **stall 감지**: 마지막 에이전트 이벤트로부터 `stall_timeout_ms` 초과 시 강제 종료 후 재시도
- **terminal 상태**: Linear에서 terminal로 전환된 이슈 → 에이전트 종료 + 워크스페이스 삭제

## 프로젝트 구조

```
symphony/
├── cmd/symphony/       # CLI 진입점
├── internal/
│   ├── agent/          # 에이전트 프로세스 실행 (JSON-RPC)
│   ├── config/         # WORKFLOW.md + daemon config 파싱
│   ├── daemon/         # 멀티 프로젝트 매니저, 자동 업데이트
│   ├── filewatch/      # 파일 변경 감지
│   ├── menubar/        # macOS 메뉴바 UI
│   ├── orchestrator/   # 메인 오케스트레이션 루프
│   ├── status/         # HTTP 상태 API
│   ├── tracker/        # Linear GraphQL 클라이언트
│   ├── update/         # GitHub Releases 업데이트 체커
│   ├── version/        # 버전 정보
│   ├── webhook/        # Linear webhook 수신 서버
│   ├── workflow/       # WORKFLOW.md 로드 + 템플릿 렌더링
│   └── workspace/      # 이슈별 디렉토리 관리 + hooks
├── scripts/            # LaunchAgent plist 템플릿
├── .github/workflows/  # CI/CD (release)
└── Makefile
```
