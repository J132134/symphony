# Symphony

Linear 이슈를 자동으로 폴링해서 이슈별 격리 워크스페이스를 생성하고, 코딩 에이전트(Codex 또는 Claude Code)를 실행해주는 오케스트레이터 데몬.

## 설치

### 요구사항

- macOS (메뉴바 UI)
- Go 1.22+
- [Codex CLI](https://github.com/openai/codex) 또는 [Claude Code](https://claude.ai/code)

### 빌드 및 설치

```bash
git clone https://github.com/J132134/symphony.git
cd symphony

# 바이너리 빌드 → ~/.local/bin/symphony
make install

# LaunchAgent 등록 (데몬 + 메뉴바 자동 시작)
make install-launchagents
```

`make install-launchagents`는 `scripts/` 안의 plist 템플릿에서 현재 홈 디렉토리와 레포 경로를 자동으로 채워 `~/Library/LaunchAgents/`에 설치한다. 로그인 시 데몬과 메뉴바가 자동으로 실행된다.

### 설치 확인

```bash
launchctl list | grep symphony
# com.symphony.daemon   → 데몬
# com.symphony.menubar  → 메뉴바
```

### 바이너리 경로 변경 시

기본 설치 경로는 `~/.local/bin`이다. 다른 경로를 사용하려면:

```bash
make install INSTALL_DIR=/usr/local/bin
make install-launchagents  # plist도 해당 경로로 재생성
```

## 빠른 시작

### 1. `WORKFLOW.md` 작성

프로젝트 루트에 `WORKFLOW.md`를 만든다. YAML front matter가 설정이고, `---` 이후가 에이전트에 전달되는 Jinja2 프롬프트 템플릿이다.

```markdown
---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: my-project
  active_states: [Todo, In Progress]
  terminal_states: [Done, Cancelled, Duplicate]

polling:
  interval_ms: 10000

workspace:
  root: ~/.symphony/workspaces

agent:
  max_concurrent_agents: 3
  max_turns: 10

codex:
  command: codex app-server
  approval_policy: auto-edit
  turn_timeout_ms: 3600000
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

`symphony daemon`은 실행 중에도 `config.yaml`의 변경을 감지해 설정을 다시 읽는다. 새 설정이 유효하면 프로젝트 목록 diff를 계산해 바뀐 프로젝트만 선택적으로 시작, 교체, 종료하고, 변경 없는 프로젝트는 그대로 유지한다. `status_server`, `auto_update`, `agent.max_total_concurrent_sessions` 같은 daemon 전역 설정이 바뀐 경우에만 상태 서버와 update loop를 포함한 전체 runtime을 다시 띄운다. 유효하지 않은 설정은 적용하지 않고 기존 실행 상태를 유지한 채 오류만 로그에 남긴다.

`agent.max_total_concurrent_sessions`는 데몬 전체에서 동시에 실행할 수 있는 에이전트 세션 수 상한이다. 값을 생략하면 실행 중인 머신의 CPU 개수를 기준으로 동적으로 계산한다: `NumCPU() <= 2`면 `1`, `<= 4`면 `2`, 그 외에는 `NumCPU()/2`를 사용하되 최대 `8`로 제한한다. 각 프로젝트의 `WORKFLOW.md`에 있는 `agent.max_concurrent_agents`는 그대로 유지되며, 실제 dispatch는 `프로젝트별 제한`과 `데몬 전체 제한`을 모두 만족해야 한다.

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

## HTTP 대시보드

`--port` 옵션 또는 `server.port` 설정 시 활성화된다.

| 엔드포인트 | 설명 |
|---|---|
| `GET /` | 실시간 HTML 대시보드 (10초 자동 갱신) |
| `GET /api/v1/summary` | 메뉴바 UI용 데몬 요약 상태(JSON) |
| `GET /api/v1/state` | 현재 실행 상태 JSON |
| `GET /api/v1/{issue_identifier}` | 특정 이슈 상세 정보 |
| `POST /api/v1/refresh` | 즉시 폴링+조정 트리거 |

`symphony menubar`는 macOS 메뉴바에서 데몬 상태를 보여준다. 정상 실행 중에는 회전하는 원형 인디케이터를, 에러가 있으면 경고 아이콘을, status server 또는 tracker 연결이 끊기면 일시정지 아이콘을 표시한다. 마우스 오버 툴팁과 메뉴 항목에서 현재 git hash, 실행 중인 서브프로세스 수, 이슈 ID 목록을 확인할 수 있다.

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
| `workspace.root` | `~/.symphony/workspaces` |
| `agent.max_concurrent_agents` | `10` |
| `agent.max_turns` | `3` |
| `agent.max_retry_backoff_ms` | `300000` (5분) |
| `codex.command` | `codex app-server` |
| `codex.turn_timeout_ms` | `3600000` (1시간) |
| `codex.stall_timeout_ms` | `300000` (5분) |
| `hooks.timeout_ms` | `60000` (60초) |

### 데몬 전용 기본값

| 항목 | 기본값 |
|---|---|
| `agent.max_total_concurrent_sessions` | `동적` (`NumCPU() <= 2` → `1`, `<= 4` → `2`, 그 외 `NumCPU()/2`, 최대 `8`) |

## 동작 흐름

```
Linear 폴링 (interval_ms마다)
  ↓
활성 이슈 fetch (active_states 필터)
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
src/symphony/
├── cli.py              # CLI 진입점 (run, validate, daemon)
├── orchestrator.py     # 메인 오케스트레이션 루프
├── config.py           # WORKFLOW.md 설정 파싱
├── workflow.py         # WORKFLOW.md 로드 + Jinja2 렌더링
├── workspace.py        # 이슈별 디렉토리 관리 + hooks
├── models.py           # 데이터 모델
├── agent/
│   ├── base.py         # AgentRunner Protocol
│   ├── codex.py        # Codex/Claude Code 프로세스 실행 (JSON-RPC)
│   └── protocol.py     # JSON-RPC 메시지 파싱/포맷
├── tracker/
│   ├── base.py         # TrackerClient Protocol
│   └── linear.py       # Linear GraphQL API 클라이언트
├── daemon/
│   ├── config.py       # 데몬 config.yaml 파싱
│   ├── manager.py      # 멀티 프로젝트 DaemonManager
│   └── updater.py      # 자동 업데이트
└── status/
    └── server.py       # FastAPI HTTP 대시보드
```
