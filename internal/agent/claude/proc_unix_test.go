//go:build unix

package claude

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestForceKillReapsGrandchild spawns `sh -c 'sleep 60 & echo $!; wait'` — a
// child shell with a grandchild `sleep`. With setpgid, a group-wide SIGKILL must
// terminate the grandchild too (otherwise it orphans and lingers).
func TestForceKillReapsGrandchild(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	prepareCmdForKill(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Read the grandchild PID printed by the shell.
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() {
		t.Fatalf("did not read grandchild pid")
	}
	gpid, err := strconv.Atoi(strings.TrimSpace(sc.Text()))
	if err != nil {
		t.Fatalf("parse grandchild pid %q: %v", sc.Text(), err)
	}

	if !alive(gpid) {
		t.Fatalf("grandchild %d should be alive before kill", gpid)
	}

	if err := forceKillCmd(cmd); err != nil {
		t.Fatalf("forceKillCmd: %v", err)
	}
	_ = cmd.Wait()

	// The grandchild must die because it shared the killed process group.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(gpid) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Clean up the orphan if the test is about to fail.
	_ = syscall.Kill(gpid, syscall.SIGKILL)
	t.Fatalf("grandchild %d survived group kill", gpid)
}

// alive reports whether a process exists (signal 0 probes without sending).
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
