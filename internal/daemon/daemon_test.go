package daemon

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A pidfile outlives the process that wrote it — it sits in ~/.bdrive, which
// survives the reboot that killed the daemon. Liveness therefore cannot be
// "some process owns this number": any same-user process that later recycles
// the pid used to read as a live daemon, which made `bdrive status` lie and
// made Start() a silent no-op, so `bdrive init` left the folder unsynced.
//
// os.Getpid() stands in for the recycler: it is alive, same-user, and
// certainly not a bdrive daemon.
func TestRecycledPidIsNotALiveDaemon(t *testing.T) {
	vdir := t.TempDir()
	writePid(t, vdir, os.Getpid())

	if pid, ok := Running(vdir); ok {
		t.Fatalf("Running reports pid %d as a live daemon; only the lock may say that", pid)
	}
}

// Garbage and stale-but-plausible pidfiles are equally not daemons.
func TestPidFileWithoutLockIsNeverRunning(t *testing.T) {
	for _, body := range []string{"", "\n", "not-a-number", "0", "-1", "999999999"} {
		vdir := t.TempDir()
		if err := os.WriteFile(PidPath(vdir), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := Running(vdir); ok {
			t.Errorf("pidfile %q read as running", body)
		}
	}
}

// The lock is the answer: held → running, released → not. This is what makes
// the reboot case correct without asking the OS about processes at all.
func TestLockDecidesLiveness(t *testing.T) {
	vdir := t.TempDir()
	if _, ok := Running(vdir); ok {
		t.Fatal("a fresh volume dir cannot have a running daemon")
	}

	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	writePid(t, vdir, 4242)
	pid, ok := Running(vdir)
	if !ok {
		t.Fatal("a held lock means a running daemon")
	}
	if pid != 4242 {
		t.Fatalf("pid = %d, want the pidfile's 4242 (informational only)", pid)
	}

	// A second holder must be refused: two daemons on one mount would both
	// write the same device journal.
	if _, err := hold(LockPath(vdir)); err == nil {
		t.Fatal("hold succeeded twice — a double daemon is possible")
	}

	release()
	if _, ok := Running(vdir); ok {
		t.Fatal("releasing the lock must end the daemon's liveness")
	}
}

// A held lock with an unreadable pid is still a running daemon — we just
// cannot name it. Stop must say so rather than pretending it stopped one.
func TestLockedWithoutPidFile(t *testing.T) {
	vdir := t.TempDir()
	release, err := hold(LockPath(vdir))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	pid, ok := Running(vdir)
	if !ok || pid != 0 {
		t.Fatalf("Running = (%d, %v), want (0, true)", pid, ok)
	}
	if stopped, err := Stop(vdir); stopped || err == nil {
		t.Fatalf("Stop = (%v, %v), want (false, error) — nothing to signal", stopped, err)
	}
}

// Stopping when nothing runs is success, and it cleans up the stale pidfile.
func TestStopWithNoDaemon(t *testing.T) {
	vdir := t.TempDir()
	writePid(t, vdir, os.Getpid())

	stopped, err := Stop(vdir)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped {
		t.Fatal("Stop reported killing a daemon that was never running")
	}
	if _, err := os.Stat(PidPath(vdir)); !os.IsNotExist(err) {
		t.Fatal("Stop left the stale pidfile behind")
	}
}

func writePid(t *testing.T, vdir string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vdir, "daemon.pid"),
		[]byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHelperLegacyDaemon is not a test: re-invoked by
// TestStopRecoversLegacyDaemon it plays a daemon from before the pid moved
// into the lock — holds the flock with nothing written inside it, announces
// itself only via daemon.pid, and exits (releasing the flock) on SIGTERM. Its
// argv carries "daemon run" so Stop's command-line check recognizes it.
func TestHelperLegacyDaemon(t *testing.T) {
	vdir := os.Getenv("BDRIVE_TEST_LEGACY_VDIR")
	if vdir == "" {
		return
	}
	f, err := os.OpenFile(LockPath(vdir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Exit(1)
	}
	if err := os.WriteFile(PidPath(vdir), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(1)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	select {
	case <-ch:
		os.Exit(0) // the kernel releases the flock with us
	case <-time.After(30 * time.Second):
		os.Exit(1)
	}
}

// A pre-upgrade daemon — flock held, lock file empty, pid only in daemon.pid —
// must still be stoppable: refusing made every pre-upgrade mount unstoppable
// short of kill-by-hand. The fallback is gated on the pidfile's process
// verifiably looking like a bdrive daemon (see the sec test for the negative:
// an arbitrary process named by daemon.pid is never signalled).
func TestStopRecoversLegacyDaemon(t *testing.T) {
	vdir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperLegacyDaemon", "daemon", "run")
	cmd.Env = append(os.Environ(), "BDRIVE_TEST_LEGACY_VDIR="+vdir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() {
		cmd.Process.Kill()
		<-exited
	}()

	// 30s, where the package's other waits use 10s, because this is the only
	// one that waits on a REAL CHILD PROCESS: os.Args[0] re-invoked, which
	// boots Go's whole test framework before it reaches the helper and takes
	// the flock. It had the shortest deadline in the package for its slowest
	// operation, and duly flaked on a loaded macOS runner (main and a PR, same
	// line, both green on re-run with no code change).
	//
	// The number is only a ceiling on how long a GENUINE failure takes to
	// report: the loop returns the moment the lock appears, so a healthy run
	// pays nothing for the headroom.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, ok := Running(vdir); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper daemon never took the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopped, err := Stop(vdir)
	if err != nil || !stopped {
		t.Fatalf("Stop = (%v, %v), want (true, nil) for a legacy daemon", stopped, err)
	}
	if _, ok := Running(vdir); ok {
		t.Fatal("legacy daemon still holds the lock after Stop")
	}
}

// A test binary must never be re-exec'd as a daemon: os.Args[0] is then the
// TEST binary, so "daemon run" re-runs the whole suite — which starts another
// daemon, and so on. One such test took a developer's Mac to load 47 before
// anyone noticed. Start refuses instead; tests that need a real daemon build
// the real binary (cli_e2e_test.go).
func TestStartRefusesToForkTheTestBinary(t *testing.T) {
	vdir := t.TempDir()
	pid, err := Start(t.TempDir(), vdir, time.Second, time.Second)
	if err == nil {
		t.Fatalf("Start returned pid %d from a test binary — that is the fork bomb", pid)
	}
	if !strings.Contains(err.Error(), "test binary") {
		t.Fatalf("error = %v, want it to name the reason", err)
	}
	if _, ok := Running(vdir); ok {
		t.Fatal("nothing may be left holding the lock")
	}
}
