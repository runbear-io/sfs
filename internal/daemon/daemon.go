// Package daemon runs the per-mount background sync loop and manages its
// lifecycle (detached start, pidfile, graceful stop).
//
// The loop scans the working folder every scan-interval (cheap: size+mtime
// against the state cache) and talks to the remote every remote-interval —
// or immediately after local changes, so edits propagate quickly without
// hammering the object store.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// The volume store is keyed by the mount id, so exactly one daemon runs per
// mount and its pid/log live in the store dir.
func PidPath(volDir string) string {
	return filepath.Join(volDir, "daemon.pid")
}
func LogPath(volDir string) string {
	return filepath.Join(volDir, "daemon.log")
}

// LockPath is the file a live daemon holds an exclusive flock on for its
// whole lifetime. Liveness is the LOCK, not the pidfile: the kernel drops a
// flock when the holder dies — including at reboot, and including a crash —
// so a leftover daemon.pid can never be mistaken for a running daemon.
//
// The pid alone cannot answer this. `kill(pid, 0)` only asks "does some
// process own this number", and daemon.pid outlives the process (it sits in
// ~/.bdrive, which survives reboots). Any same-user process that later
// recycles the pid used to read as a live daemon — which made `bdrive status`
// lie and, worse, made Start() a silent no-op, so the one documented recovery
// (`bdrive init`) left the folder unsynced.
func LockPath(volDir string) string {
	return filepath.Join(volDir, "daemon.lock")
}

// Running reports the daemon pid for a mount if one is alive. Aliveness is the
// flock on LockPath. The pid comes out of that same locked file — written by
// the holder itself (see announce) — and falls back to daemon.pid only for
// display: nothing binds THAT file's contents to the process holding the lock,
// and a daemon killed with -9 leaves it behind for the next process to inherit
// the number. Stop signals the announced pid only (see lockPid).
func Running(volDir string) (int, bool) {
	pid, ok := lockPid(volDir)
	if !ok {
		return 0, false
	}
	if pid > 0 {
		return pid, true
	}
	data, err := os.ReadFile(PidPath(volDir))
	if err != nil {
		return 0, true
	}
	if p, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && p > 0 {
		return p, true
	}
	return 0, true
}

// lockPid reports whether a daemon holds the mount's lock and, if it announced
// one, its pid. This is the only pid anything ever signals: it is written
// inside the lock and truncated when the lock is released, so it cannot name a
// process that has exited.
func lockPid(volDir string) (int, bool) {
	f, err := openLock(LockPath(volDir))
	if err != nil {
		return 0, unopenableLockIsRunning(LockPath(volDir))
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false // nobody holds it: no daemon
	}
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil || pid <= 0 {
		return 0, true // alive, but it announced no pid we can signal
	}
	return pid, true
}

// openLock opens the lock file without following a symlink. Following one put
// the flock on whatever the link named, so a single symlink inside
// $BDRIVE_HOME made an unrelated long-lived process's lock read as this
// mount's daemon — Start() a no-op and sync silently never restarting, the
// exact failure the flock design exists to eliminate.
func openLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
}

// held is the set of lock paths THIS process holds. It is the only evidence
// of a daemon available when the lock file cannot be opened.
var held struct {
	sync.Mutex
	paths map[string]bool
}

func markHeld(path string, on bool) {
	held.Lock()
	defer held.Unlock()
	if held.paths == nil {
		held.paths = map[string]bool{}
	}
	if on {
		held.paths[path] = true
	} else {
		delete(held.paths, path)
	}
}

// unopenableLockIsRunning answers the liveness question when the lock file
// cannot be opened at all.
//
// Round 5 made this fail CLOSED with one carve-out for a symlink, and the
// carve-out was on the wrong axis: a directory at the lock path, a lock file
// nobody can open, and a volume directory that does not exist at all all fail
// to open too, and every one of them then read as a live daemon forever —
// Start a permanent no-op, Stop refusing, `bdrive status` reporting a healthy
// mount that has not synced since. The ENOENT case needs no attacker.
//
// The answer is the reason, not the shape: none of those states is a daemon,
// and reporting one wedges the mount. What makes reporting "no daemon" safe
// here is that holdLock opens the SAME path the same way — a second daemon
// cannot start on a lock it cannot open either, so the "two writers of one
// journal" this guard exists for cannot materialize. The one real daemon that
// can exist while the file is unopenable is this process, and it knows.
func unopenableLockIsRunning(path string) bool {
	held.Lock()
	defer held.Unlock()
	return held.paths[path]
}

// locked reports whether another process holds the lock file.
func locked(path string) bool {
	f, err := openLock(path)
	if err != nil {
		return unopenableLockIsRunning(path)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// holdLock takes the daemon's lifetime lock, returning the open file so the
// holder can announce its pid INSIDE it. Process death releases the flock,
// which is the point.
func holdLock(path string) (*os.File, error) {
	f, err := openLock(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another daemon is already running for this mount: %w", err)
	}
	markHeld(path, true)
	return f, nil
}

// release drops the lock and clears the announced pid with it, so no number
// outlives the process that owned it.
func release(f *os.File) {
	markHeld(f.Name(), false)
	f.Truncate(0)
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// hold takes the lock and returns its releaser.
func hold(path string) (func(), error) {
	f, err := holdLock(path)
	if err != nil {
		return nil, err
	}
	return func() { release(f) }, nil
}

// announce writes the holder's pid into the locked file — the only pid Stop
// ever signals. It lives and dies with the lock, so it cannot name a process
// that has exited (and been recycled), which daemon.pid routinely did: a
// daemon killed with -9 leaves that file behind for the next process to
// inherit the number.
func announce(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	_, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// Start launches a detached daemon for the folder (no-op if already running).
func Start(folder, volDir string, scanInterval, remoteInterval time.Duration) (int, error) {
	if pid, ok := Running(volDir); ok {
		return pid, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// A `go test` binary must never re-exec itself as a daemon: os.Args[0] is
	// then the TEST binary, so the spawned "daemon run" re-runs the whole
	// suite — which spawns again. A test that reached this line once took a
	// developer's Mac to load 47 before anyone noticed. Any test that wants a
	// real daemon builds the real binary and runs it (cli_e2e_test.go), so
	// refusing here costs nothing and removes the fork bomb.
	if strings.HasSuffix(exe, ".test") || strings.Contains(exe, "/_test/") {
		return 0, fmt.Errorf("refusing to start a daemon from a test binary (%s)", filepath.Base(exe))
	}
	// 0600: daemon.log names the mount id, the folder's absolute path, the
	// remote URL and the device name+id.
	logf, err := os.OpenFile(LogPath(volDir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "run", folder,
		"--scan-interval", scanInterval.String(),
		"--remote-interval", remoteInterval.String())
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if err := cmd.Process.Release(); err != nil {
		return 0, err
	}

	// The child announces its own pid, and only once it holds the lifetime
	// lock (see Run). The parent must NOT write PidPath: a child that loses
	// the lock race exits without ever being the daemon, and its pid written
	// here would outlive it — leaving Stop signalling a corpse (ESRCH) while
	// the daemon that won keeps syncing, and status printing a phantom pid.
	//
	// So wait for the lock instead of assuming the spawn worked. A caller
	// that gets a pid back can trust that a daemon owns it. In a race the pid
	// is the winner's rather than the child just spawned, which is the honest
	// answer to "which pid is the daemon" — callers that need to distinguish
	// starting from adopting check Running first (see `bdrive resume`).
	deadline := time.Now().Add(startTimeout)
	for {
		if pid, ok := Running(volDir); ok && pid > 0 {
			return pid, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("daemon did not come up within %s; see %s",
				startTimeout, LogPath(volDir))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startTimeout bounds how long Start waits for the child to take the lock and
// write its pid. Generous on purpose: it covers a cold binary on a loaded
// machine, and the only cost of waiting is a slower `bdrive init`.
const startTimeout = 10 * time.Second

// Stop terminates the daemon for a mount and waits for it to exit. Exit is
// observed by the lock being released, not by the pid disappearing: the pid
// could be recycled while we wait, and the lock cannot.
func Stop(volDir string) (bool, error) {
	// The pid to signal comes from inside the lock, never from daemon.pid: a
	// number in a file the daemon does not own is a SIGKILL primitive against
	// any same-user process, and a -9'd daemon leaves exactly such a number
	// behind with no attacker involved.
	pid, ok := lockPid(volDir)
	if !ok {
		os.Remove(PidPath(volDir))
		return false, nil
	}
	legacy := false
	if pid <= 0 {
		// A held lock whose file names no pid is a daemon from before the pid
		// moved into the lock — it wrote daemon.pid instead, and telling its
		// user "kill it by hand" made every pre-upgrade mount unstoppable.
		// daemon.pid alone is never a license to signal (recycled pids, and a
		// number in a file the holder never wrote — see the sec test), so the
		// fallback signals only a process that VERIFIABLY looks like a bdrive
		// daemon: the pidfile's process must be alive and its command line
		// must say `daemon run`. SIGTERM only, below — no SIGKILL escalation
		// on a pre-invariant number.
		data, rerr := os.ReadFile(PidPath(volDir))
		p, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if rerr != nil || perr != nil || p <= 0 || !strings.Contains(cmdline(p), "daemon run") {
			return false, fmt.Errorf("a daemon holds %s but it announced no pid; kill it by hand",
				LockPath(volDir))
		}
		pid, legacy = p, true
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return false, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !locked(LockPath(volDir)) {
			os.Remove(PidPath(volDir))
			return true, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if legacy {
		return false, fmt.Errorf("daemon (pid %d) did not exit after SIGTERM; kill it by hand", pid)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	os.Remove(PidPath(volDir))
	return true, nil
}

// cmdline reports a process's command line, or "" when it cannot be known —
// /proc on Linux, ps everywhere else. Used only to recognize a legacy daemon
// before signalling it; "" fails closed.
func cmdline(pid int) string {
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		return strings.ReplaceAll(string(data), "\x00", " ")
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// Run is the daemon main loop, executed in the foreground of the (usually
// detached) `bdrive daemon run` process.
func Run(folder string, scanInterval, remoteInterval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	proj, ok, err := config.ResolveMount(folder)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%s is not a beardrive project (run `bdrive init`)", folder)
	}
	volDir, err := config.VolumeDir(proj.ID)
	if err != nil {
		return err
	}
	st, err := store.Open(volDir)
	if err != nil {
		return err
	}
	dev, err := config.LoadDevice()
	if err != nil {
		return err
	}
	// Hold the lifetime lock before announcing the pid: it is what makes
	// "is a daemon running" answerable, and it also makes a double start
	// impossible (two daemons on one mount would write one journal twice).
	lockf, err := holdLock(LockPath(volDir))
	if err != nil {
		return err
	}
	defer release(lockf)
	if err := announce(lockf); err != nil {
		return err
	}

	// daemon.pid is the human-readable copy (status output, `kill` by hand).
	// 0600: this directory is 0755 and the file decides what gets signalled.
	if err := os.WriteFile(PidPath(volDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	defer os.Remove(PidPath(volDir))

	log.Printf("daemon started: folder=%s mount=%s volume=%s remote=%q device=%s(%s) scan=%s sync=%s",
		folder, proj.ID, proj.Volume, proj.Remote, dev.Name, dev.ID, scanInterval, remoteInterval)

	var be remote.Backend
	defer func() {
		if be != nil {
			be.Close()
		}
	}()
	var lastRemote time.Time
	var lastToken string
	// Which access state we last logged, so a degraded daemon says it once
	// instead of on every tick.
	lastAccess := store.AccessOK

	for {
		// Re-read the project config each tick: picks up `bdrive remote set`
		// and hand-edits. A vanished config means the folder was moved,
		// renamed, or deleted — exit cleanly (propagating nothing); the next
		// bdrive command in the folder's new location resumes the daemon.
		cur, ok, err := config.LoadProject(folder)
		if err != nil || !ok {
			log.Printf("project config gone (folder moved or deleted); exiting")
			return nil
		}
		if cur.ID != proj.ID {
			log.Printf("mount identity changed; exiting")
			return nil
		}
		// If the registry says this mount now lives elsewhere, a new
		// location has taken over — stand down.
		if m, err := config.LoadMounts(); err == nil {
			if mi, ok := m[proj.ID]; ok && mi.Path != folder {
				log.Printf("mount re-registered at %s; exiting", mi.Path)
				return nil
			}
		}
		if cur.Remote != proj.Remote {
			// A running daemon never follows a folder config to a new remote.
			// .bdrive/config.json is untrusted input (anything with write
			// access inside the mount writes it: an agent session, a
			// dependency's install script), and following it moved the whole
			// project — every path, device name and signed-in email in the
			// journal — to a host the user never chose, on the next 3s tick,
			// with no credential needed for a file:// target. The daemon then
			// PULLED from there too, which is an arbitrary write into the
			// mount. Standing down is the same self-heal as a moved folder:
			// the next bdrive command in this folder starts a daemon for
			// whatever it then says, which is a user action.
			log.Printf("remote changed in .bdrive/config.json (%q -> %q); exiting — run bdrive in this folder to resume",
				proj.Remote, cur.Remote)
			return nil
		}
		proj = cur

		// Re-read settings each tick too, so a login/logout/account switch
		// after the daemon started is reflected in op authorship — otherwise
		// a long-lived daemon stamps every change with a stale identity. The
		// http backend captures its credential at open, so drop it when the
		// token changes and reconnect with the new one.
		settings, _ := config.LoadSettings()
		if settings.Token != lastToken {
			if be != nil {
				be.Close()
				be = nil
			}
			lastToken = settings.Token
			lastRemote = time.Time{}
		}

		doRemote := proj.Remote != "" && time.Since(lastRemote) >= remoteInterval
		if doRemote && be == nil {
			b, err := remote.Open(ctx, proj.Remote)
			if err != nil {
				log.Printf("remote unavailable: %v", err)
				doRemote = false
				lastRemote = time.Now()
			} else {
				be = b
			}
		}

		sess := &syncer.Session{Folder: folder, MountID: proj.ID, Store: st, Device: dev, Account: settings}
		if doRemote {
			sess.Backend = be
		}
		res, err := sess.Cycle(ctx)
		switch {
		case ctx.Err() != nil:
			log.Printf("daemon stopping")
			return nil
		case err != nil:
			log.Printf("cycle error: %v", err)
		case res.NoAccess:
			// The connection is fine, the answer isn't: keep the backend and
			// keep ticking cheaply so a re-grant self-heals. Log the
			// transition only — a paused daemon must stay quiet.
			if lastAccess != store.AccessNone {
				log.Printf("access revoked for this project; sync paused (%v)", res.AccessErr)
				lastAccess = store.AccessNone
			}
			lastRemote = time.Now()
		case res.ReadOnly:
			if lastAccess != store.AccessReadOnly {
				if reason := res.Reason(); reason != "" {
					log.Printf("read-only on this project, pulling only; local changes stay on this device (%s)", reason)
				} else {
					log.Printf("read-only on this project, pulling only; local changes stay on this device")
				}
				lastAccess = store.AccessReadOnly
			}
			lastRemote = time.Now()
		case res.Offline:
			log.Printf("offline, will retry: %v", res.OfflineErr)
			if be != nil {
				be.Close()
				be = nil
			}
			lastRemote = time.Now()
		default:
			// "access restored" is a claim about the hub, and a local-only tick
			// never asked it one. Announcing it there is what made a refused
			// device alternate between this line and the read-only one every few
			// seconds: three cheap scan ticks run between remote passes, each
			// cleared the flag, and the next remote pass set it again.
			if doRemote && lastAccess != store.AccessOK {
				log.Printf("access restored; syncing normally")
				lastAccess = store.AccessOK
			}
			if res.Activity() {
				log.Printf("local+%d pulled+%d conflicts=%d files~%d pushed=%v",
					res.LocalOps, res.PulledOps, res.Conflicts, res.Materialized, res.Pushed)
			}
			if doRemote {
				lastRemote = time.Now()
			}
			if res.LocalOps > 0 && !doRemote {
				lastRemote = time.Time{} // push local edits on the next tick
			}
		}

		select {
		case <-ctx.Done():
			log.Printf("daemon stopping")
			return nil
		case <-time.After(scanInterval):
		}
	}
}
