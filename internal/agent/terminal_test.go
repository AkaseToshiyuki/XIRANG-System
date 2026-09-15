package agent

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
)

func TestTerminalManagerRunsPTYAndEmitsBoundedOutput(t *testing.T) {
	identity := terminalTestIdentity(t)
	manager, err := newTerminalManager(identity, []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	id := "terminal-0123456789abcdef0123456789abcdef"
	accepted, reason := manager.Open(&xirangv1.OpenTerminal{SessionId: id, Columns: 80, Rows: 24})
	if !accepted {
		t.Fatalf("open PTY: %s", reason)
	}
	defer manager.CloseAll("test cleanup")
	if accepted, reason := manager.Input(&xirangv1.TerminalInput{SessionId: id, Data: []byte("id -u\nprintf XIRANG_PTY_OK\\n\nexit\n")}); !accepted {
		t.Fatalf("write PTY: %s", reason)
	}
	deadline := time.After(5 * time.Second)
	var output strings.Builder
	closed := false
	for !closed {
		select {
		case event := <-manager.Events():
			if len(event.Data) > terminalOutputChunkBytes {
				t.Fatalf("oversized PTY frame: %d", len(event.Data))
			}
			output.Write(event.Data)
			closed = event.Type == xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_CLOSED
		case <-deadline:
			t.Fatal("PTY did not close")
		}
	}
	if !strings.Contains(output.String(), "XIRANG_PTY_OK") {
		t.Fatalf("PTY output missing marker: %q", output.String())
	}
	if !strings.Contains(output.String(), strconv.FormatUint(uint64(identity.credential.Uid), 10)) {
		t.Fatalf("PTY did not run as the configured non-root identity: %q", output.String())
	}
}

func TestTerminalCommandsAreEphemeralAndNotWrittenToLedger(t *testing.T) {
	directory := t.TempDir()
	state := commandState{Mode: "ready"}
	manager, err := newTerminalManager(terminalTestIdentity(t), []string{"/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	id := "terminal-fedcba9876543210fedcba9876543210"
	message := &xirangv1.ControlMessage{MessageId: "terminal-command", Payload: &xirangv1.ControlMessage_OpenTerminal{
		OpenTerminal: &xirangv1.OpenTerminal{SessionId: id, Columns: 80, Rows: 24},
	}}
	updated, acknowledgement, err := applyControlCommandWithHandlers(t.Context(), directory, state, message, commandHandlers{terminal: manager})
	if err != nil || !acknowledgement.Accepted {
		t.Fatalf("terminal command rejected: ack=%#v err=%v", acknowledgement, err)
	}
	defer manager.CloseAll("test cleanup")
	if len(updated.Acknowledgements) != 0 {
		t.Fatalf("terminal payload was persisted in command ledger: %#v", updated.Acknowledgements)
	}
}

func TestTerminalLauncherSetsNoNewPrivileges(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("launcher intentionally refuses root and is covered by the physical Agent acceptance")
	}
	if os.Getenv("XIRANG_TEST_TERMINAL_LAUNCHER") == "1" {
		if err := RunTerminalLauncher([]string{"/bin/sh", "-c", "grep -Eq ^NoNewPrivs:[[:space:]]*1$ /proc/self/status"}); err != nil {
			os.Exit(125)
		}
		return
	}
	command := exec.Command(os.Args[0], "-test.run=^TestTerminalLauncherSetsNoNewPrivileges$")
	command.Env = append(os.Environ(), "XIRANG_TEST_TERMINAL_LAUNCHER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("terminal launcher did not establish no-new-privileges: %v: %s", err, output)
	}
}

func terminalTestIdentity(t *testing.T) terminalIdentity {
	t.Helper()
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 || gid == 0 {
		t.Skip("direct PTY test requires a non-root test runner")
	}
	home := t.TempDir()
	return terminalIdentity{
		username: "xirang-test", home: home,
		credential: &syscall.Credential{Uid: uid, Gid: gid, NoSetGroups: true},
	}
}
