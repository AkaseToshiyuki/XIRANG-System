package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	xirangv1 "github.com/xirang-grid/xirang/gen/xirang/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxAgentTerminalSessions = 4
	maxTerminalFrameBytes    = 64 << 10
	terminalOutputChunkBytes = 16 << 10
)

var terminalSessionPattern = regexp.MustCompile(`^terminal-[a-f0-9]{32}$`)

type agentTerminalSession struct {
	id         string
	command    *exec.Cmd
	pty        *os.File
	outputDone chan struct{}
}

// TerminalManager owns short-lived local PTYs requested over the authenticated
// controller stream. It never opens a listening socket and never handles keys.
type TerminalManager struct {
	mu       sync.Mutex
	sessions map[string]*agentTerminalSession
	events   chan *xirangv1.TerminalEvent
	identity terminalIdentity
	launcher []string
}

type terminalIdentity struct {
	username   string
	home       string
	credential *syscall.Credential
}

func resolveTerminalIdentity(username string) (terminalIdentity, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return terminalIdentity{}, errors.New("administrator terminal requires a dedicated non-root user")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return terminalIdentity{}, fmt.Errorf("lookup terminal user %q: %w", username, err)
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return terminalIdentity{}, errors.New("administrator terminal user must resolve to non-root numeric UID and GID")
	}
	home := strings.TrimSpace(account.HomeDir)
	if home == "" || home == "/" || home[0] != '/' {
		return terminalIdentity{}, errors.New("administrator terminal user requires a dedicated absolute home directory")
	}
	return terminalIdentity{
		username: username,
		home:     home,
		credential: &syscall.Credential{
			Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)},
		},
	}, nil
}

func OpenTerminalManager(username string) (*TerminalManager, error) {
	identity, err := resolveTerminalIdentity(username)
	if err != nil {
		return nil, err
	}
	return newTerminalManager(identity, []string{"/proc/self/exe", "__xirang-terminal-exec", "/bin/sh"})
}

func newTerminalManager(identity terminalIdentity, launcher []string) (*TerminalManager, error) {
	if identity.credential == nil || identity.credential.Uid == 0 || identity.credential.Gid == 0 || len(launcher) == 0 {
		return nil, errors.New("administrator terminal cannot use root or an incomplete execution identity")
	}
	return &TerminalManager{
		sessions: make(map[string]*agentTerminalSession), events: make(chan *xirangv1.TerminalEvent, 128),
		identity: identity, launcher: append([]string(nil), launcher...),
	}, nil
}

func (manager *TerminalManager) Events() <-chan *xirangv1.TerminalEvent { return manager.events }

func (manager *TerminalManager) Open(command *xirangv1.OpenTerminal) (bool, string) {
	if command == nil || !terminalSessionPattern.MatchString(command.SessionId) || !validTerminalSize(command.Columns, command.Rows) {
		return false, "invalid terminal session or dimensions"
	}
	manager.mu.Lock()
	if _, exists := manager.sessions[command.SessionId]; exists {
		manager.mu.Unlock()
		return true, ""
	}
	if len(manager.sessions) >= maxAgentTerminalSessions {
		manager.mu.Unlock()
		return false, "terminal session limit reached"
	}
	manager.mu.Unlock()
	process := exec.Command(manager.launcher[0], manager.launcher[1:]...)
	process.Env = []string{
		"HOME=" + manager.identity.home,
		"USER=" + manager.identity.username,
		"LOGNAME=" + manager.identity.username,
		"SHELL=/bin/sh",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
	}
	process.Dir = manager.identity.home
	process.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL, Credential: manager.identity.credential,
	}
	terminal, err := pty.StartWithSize(process, &pty.Winsize{Cols: uint16(command.Columns), Rows: uint16(command.Rows)})
	if err != nil {
		return false, err.Error()
	}
	session := &agentTerminalSession{id: command.SessionId, command: process, pty: terminal, outputDone: make(chan struct{})}
	manager.mu.Lock()
	if len(manager.sessions) >= maxAgentTerminalSessions {
		manager.mu.Unlock()
		_ = terminal.Close()
		_ = process.Process.Kill()
		return false, "terminal session limit reached"
	}
	manager.sessions[session.id] = session
	manager.mu.Unlock()
	manager.emit(&xirangv1.TerminalEvent{SessionId: session.id, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_OPENED})
	go manager.readOutput(session)
	go manager.wait(session)
	return true, ""
}

// RunTerminalLauncher sets the irreversible no-new-privileges boundary before
// replacing itself with the fixed shell selected by the root Agent. The
// launcher already runs as the dedicated non-root terminal identity.
func RunTerminalLauncher(arguments []string) error {
	if len(arguments) == 0 || arguments[0] != "/bin/sh" {
		return errors.New("administrator terminal launcher only permits /bin/sh")
	}
	if os.Geteuid() == 0 {
		return errors.New("administrator terminal launcher refuses root execution")
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("disable administrator terminal privilege gains: %w", err)
	}
	syscall.Umask(0o077)
	return syscall.Exec(arguments[0], append([]string{arguments[0]}, arguments[1:]...), os.Environ())
}

func (manager *TerminalManager) Input(command *xirangv1.TerminalInput) (bool, string) {
	if command == nil || !terminalSessionPattern.MatchString(command.SessionId) || len(command.Data) > maxTerminalFrameBytes {
		return false, "invalid terminal input"
	}
	session := manager.session(command.SessionId)
	if session == nil {
		return false, "terminal session is not open"
	}
	if _, err := session.pty.Write(command.Data); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (manager *TerminalManager) Resize(command *xirangv1.ResizeTerminal) (bool, string) {
	if command == nil || !validTerminalSize(command.Columns, command.Rows) {
		return false, "invalid terminal dimensions"
	}
	session := manager.session(command.SessionId)
	if session == nil {
		return false, "terminal session is not open"
	}
	if err := pty.Setsize(session.pty, &pty.Winsize{Cols: uint16(command.Columns), Rows: uint16(command.Rows)}); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (manager *TerminalManager) Close(command *xirangv1.CloseTerminal) (bool, string) {
	if command == nil || !terminalSessionPattern.MatchString(command.SessionId) {
		return false, "invalid terminal session"
	}
	session := manager.remove(command.SessionId)
	if session == nil {
		return true, ""
	}
	_ = session.pty.Close()
	if session.command.Process != nil {
		_ = session.command.Process.Signal(syscall.SIGHUP)
	}
	manager.emit(&xirangv1.TerminalEvent{SessionId: session.id, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_CLOSED, Message: command.Reason})
	return true, ""
}

func (manager *TerminalManager) CloseAll(reason string) {
	manager.mu.Lock()
	ids := make([]string, 0, len(manager.sessions))
	for id := range manager.sessions {
		ids = append(ids, id)
	}
	manager.mu.Unlock()
	for _, id := range ids {
		manager.Close(&xirangv1.CloseTerminal{SessionId: id, Reason: reason})
	}
}

func (manager *TerminalManager) readOutput(session *agentTerminalSession) {
	defer close(session.outputDone)
	buffer := make([]byte, terminalOutputChunkBytes)
	for {
		count, err := session.pty.Read(buffer)
		if count > 0 {
			manager.emit(&xirangv1.TerminalEvent{SessionId: session.id, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_OUTPUT, Data: append([]byte(nil), buffer[:count]...)})
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && manager.session(session.id) != nil {
				manager.emit(&xirangv1.TerminalEvent{SessionId: session.id, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_ERROR, Message: err.Error()})
			}
			return
		}
	}
}

func (manager *TerminalManager) wait(session *agentTerminalSession) {
	err := session.command.Wait()
	select {
	case <-session.outputDone:
	case <-time.After(time.Second):
		// A correctly closed PTY becomes readable with EOF/EIO after the child
		// exits. Bound that wait for unusual platform behavior, then force the
		// reader awake and still preserve output-before-close event ordering.
		_ = session.pty.Close()
		<-session.outputDone
	}
	if manager.remove(session.id) == nil {
		return
	}
	_ = session.pty.Close()
	event := &xirangv1.TerminalEvent{SessionId: session.id, Type: xirangv1.TerminalEventType_TERMINAL_EVENT_TYPE_CLOSED}
	if err != nil {
		event.Message = err.Error()
		if exit, ok := err.(*exec.ExitError); ok {
			event.ExitCode = int32(exit.ExitCode())
		}
	}
	manager.emit(event)
}

func (manager *TerminalManager) session(id string) *agentTerminalSession {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.sessions[id]
}

func (manager *TerminalManager) remove(id string) *agentTerminalSession {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current := manager.sessions[id]
	delete(manager.sessions, id)
	return current
}

func (manager *TerminalManager) emit(event *xirangv1.TerminalEvent) {
	event.OccurredAt = timestamppb.New(time.Now().UTC())
	select {
	case manager.events <- event:
	default:
		if session := manager.remove(event.SessionId); session != nil {
			_ = session.pty.Close()
			if session.command.Process != nil {
				_ = session.command.Process.Kill()
			}
		}
	}
}

func validTerminalSize(columns, rows uint32) bool {
	return columns >= 20 && columns <= 500 && rows >= 5 && rows <= 300
}
