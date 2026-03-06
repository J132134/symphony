# J-41 macOS Permission Prompt Implementation Plan

**Goal:** 데몬 시작 시 launch agent가 레포 경로를 불필요하게 접근해 macOS 문서 폴더 권한 알림을 띄우는 문제를 제거한다.
**Architecture:** launch agent의 작업 디렉터리와 로그 경로를 레포 경로에서 분리하고, daemon config 상대 경로를 config 파일 위치 기준으로 해석해 작업 디렉터리 제거 이후에도 설정이 안정적으로 동작하게 만든다.
**Tech Stack:** Go 1.22, launchd plist, GNU Make

---

### Task 1: daemon config 상대 경로를 config 파일 기준으로 고정

**Files:**
- Modify: `internal/config/daemon.go`
- Test: `internal/config/daemon_test.go`

**Step 1: 상대 workflow/repo_dir 경로가 현재 작업 디렉터리에 의존하는 지점을 식별한다**

`LoadDaemonConfig`는 `resolvePath`에서 `filepath.Abs`를 바로 호출해 현재 작업 디렉터리에 의존한다.

**Step 2: config 파일 디렉터리를 base dir로 전달하도록 구현한다**

`LoadDaemonConfig`가 `config.yaml`의 디렉터리를 계산해 `resolvePath`에 전달하고, 비절대 경로는 그 디렉터리 기준으로 정규화한다.

**Step 3: 상대 경로 해석 테스트를 추가한다**

상대 `workflow`, `auto_update.repo_dir`가 config 파일 위치 기준으로 절대 경로화되는지 검증한다.

### Task 2: launch agent의 레포 경로 의존 제거

**Files:**
- Modify: `Makefile`
- Modify: `scripts/com.symphony.daemon.plist`
- Modify: `scripts/com.symphony.menubar.plist`

**Step 1: install-launchagents가 로그 전용 디렉터리를 생성하도록 변경한다**

`~/Library/Logs/Symphony`를 만들고 plist 치환 변수로 전달한다.

**Step 2: plist에서 WorkingDirectory와 레포 기반 로그 경로를 제거한다**

daemon/menubar 모두 로그를 `~/Library/Logs/Symphony`로 보내고, 시작 시 레포 디렉터리를 건드리지 않도록 한다.

### Task 3: 설치 문서 갱신 및 검증

**Files:**
- Modify: `README.md`

**Step 1: 설치 설명에 새 launch agent 동작을 문서화한다**

레포가 Documents/Desktop에 있어도 launchd가 불필요하게 해당 폴더를 접근하지 않도록 바뀐 이유를 설명한다.

**Step 2: 검증을 수행한다**

`go build ./...`와 `go test ./...`를 실행해 변경 범위를 확인한다.
