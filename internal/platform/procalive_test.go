package platform_test

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/dinstein/agent-hub/internal/platform"
)

func TestProcessAliveSelf(t *testing.T) {
	alive, known := platform.ProcessAlive(os.Getpid())
	if !known {
		t.Fatalf("ProcessAlive(self) could not answer; it must know about this very process")
	}
	if !alive {
		t.Fatalf("ProcessAlive(self) = false; this process is running")
	}
}

func TestProcessAliveNonPositivePidIsUnknown(t *testing.T) {
	// 0 and negatives address process GROUPS on unix, not processes. Reading
	// them as "alive" or "dead" would both be answers to a question that was
	// never asked.
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if alive, known := platform.ProcessAlive(pid); alive || known {
			t.Errorf("ProcessAlive(%d) = (%v, %v), want (false, false)", pid, alive, known)
		}
	}
}

func TestProcessAliveReportsAnExitedChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the windows branch has never run on a real machine; see docs/status/windows.md")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a throwaway child: %v", err)
	}
	pid := cmd.Process.Pid
	// Wait reaps it: an unreaped child stays a zombie, and a zombie still
	// answers signal 0. The reap is what makes this a real "gone" test.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for the child: %v", err)
	}
	alive, known := platform.ProcessAlive(pid)
	if !known {
		t.Fatalf("ProcessAlive(reaped child) could not answer")
	}
	if alive {
		t.Fatalf("ProcessAlive(reaped child pid %d) = true, want false", pid)
	}
}
