package daemon

import (
	"sync"
	"testing"
	"time"
)

func TestCheckForUpdatesUsesShutdownPathWithoutWaitingForIdle(t *testing.T) {
	t.Parallel()

	prevPrepare := prepareUpdateFn
	prevValidate := validateUpdateFn
	prevExit := updaterExitFn
	t.Cleanup(func() {
		prepareUpdateFn = prevPrepare
		validateUpdateFn = prevValidate
		updaterExitFn = prevExit
	})

	prepareUpdateFn = func() (bool, error) {
		return true, nil
	}
	validateUpdateFn = func(string) error { return nil }

	var cancelOnce sync.Once
	stopped := make(chan struct{})
	exited := make(chan int, 1)
	done := make(chan struct{})
	close(done)

	mgr := &Manager{
		cancel:           func() { cancelOnce.Do(func() { close(stopped) }) },
		done:             done,
		restartRequested: true,
		restartReady:     make(chan struct{}),
	}

	updaterExitFn = func(code int) {
		exited <- code
	}

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		CheckForUpdates(mgr)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should trigger Shutdown without waiting for idle")
	}

	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should exit after Shutdown+Wait")
	}

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("CheckForUpdates should return after updaterExitFn")
	}
}

func TestValidateMacOSSignatureDetailsRejectsAdHoc(t *testing.T) {
	t.Parallel()

	err := validateMacOSSignatureDetails(`Executable=/tmp/symphony
Signature=adhoc
TeamIdentifier=not set
`)
	if err == nil {
		t.Fatal("expected ad-hoc signature to be rejected")
	}
}

func TestValidateMacOSSignatureDetailsRejectsMissingTeamIdentifier(t *testing.T) {
	t.Parallel()

	err := validateMacOSSignatureDetails(`Executable=/tmp/symphony
Authority=Developer ID Application: Symphony Inc (TEAMID1234)
TeamIdentifier=not set
`)
	if err == nil {
		t.Fatal("expected missing TeamIdentifier to be rejected")
	}
}

func TestValidateMacOSSignatureDetailsRejectsNonDeveloperIDAuthority(t *testing.T) {
	t.Parallel()

	err := validateMacOSSignatureDetails(`Executable=/tmp/symphony
Authority=Apple Development: Symphony Inc (TEAMID1234)
TeamIdentifier=TEAMID1234
`)
	if err == nil {
		t.Fatal("expected non-Developer ID authority to be rejected")
	}
}

func TestValidateMacOSSignatureDetailsAcceptsDeveloperIDSignedBinary(t *testing.T) {
	t.Parallel()

	err := validateMacOSSignatureDetails(`Executable=/tmp/symphony
Authority=Developer ID Application: Symphony Inc (TEAMID1234)
Authority=Developer ID Certification Authority
Authority=Apple Root CA
TeamIdentifier=TEAMID1234
`)
	if err != nil {
		t.Fatalf("expected Developer ID signed binary to be accepted: %v", err)
	}
}
