//go:build !windows

package tool

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"syscall"
)

// setDetached puts the child in its own process group so it survives the
// per-turn context and can be killed as a whole tree.
func setDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// treeKill terminates the process group, falling back to the lone process.
func treeKill(p *os.Process) {
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		_ = p.Kill()
	}
}

// The per-task memory watchdog (norma v0.4.4 feature, re-implemented here on
// the vendored fork): a Bash-spawned process tree that grows past the RSS cap
// is killed as a whole group, so a runaway nuclei/ffuf/bbot run cannot drive
// the host into the OOM killer and take the platform down with it. The cap is
// NORMA_TASK_RSS_MB in megabytes; unset defaults to 4096, 0 or a negative
// value disables the watchdog.
func taskRSSCapBytes() int64 {
	v := strings.TrimSpace(os.Getenv("NORMA_TASK_RSS_MB"))
	if v == "" {
		return 4096 << 20
	}
	mb, err := strconv.ParseInt(v, 10, 64)
	if err != nil || mb <= 0 {
		return 0 // disabled
	}
	return mb << 20
}

// pageOffset is the page size the /proc/<pid>/statm fields are counted in.
// os.Getpagesize never fails; 4096 covers every architecture this product
// builds for (arm64 and amd64 both use 4 KiB pages).
var pageOffset = func() int64 { return int64(os.Getpagesize()) }

// processTreeRSS sums the resident set of every process whose process group is
// pgid, by walking /proc and matching stat field 5 (the process group id). A
// process that disappears between listing and reading is skipped: its RSS is
// either already gone or counted next round. The sum is a moment-in-time
// sample, not a quota — a tree spikes and shrinks, so the watchdog fires on a
// sustained overshoot, not a single allocation.
func processTreeRSS(pgid int) int64 {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	page := pageOffset()
	var total int64
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a numeric /proc/<pid> entry
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited between listing and reading
		}
		// stat's comm field is parenthesised and may contain spaces: fields after
		// the last ')' are stable (proc(5) field numbering restarts at state=3).
		i := strings.LastIndexByte(string(raw), ')')
		if i < 0 {
			continue
		}
		fields := strings.Fields(string(raw[i+1:]))
		if len(fields) < 3 {
			continue
		}
		if pgrp, err := strconv.Atoi(fields[2]); err != nil || pgrp != pgid {
			continue // different process group
		}
		statm, err := os.ReadFile("/proc/" + e.Name() + "/statm")
		if err != nil {
			continue
		}
		rssPages, err := strconv.ParseInt(strings.Fields(string(statm))[0], 10, 64)
		if err != nil {
			continue
		}
		total += rssPages * page
	}
	return total
}

// taskRSSWatchdog polls the task's process-tree RSS every 2 seconds and
// terminates the whole group once it exceeds the configured cap. Runs in the
// task's watch goroutine family; stops when the task's stop channel closes.
func (m *Manager) taskRSSWatchdog(t *Task, stop chan struct{}) {
	capBytes := taskRSSCapBytes()
	if capBytes <= 0 || t.cmd.Process == nil {
		return // disabled, or the process never started
	}
	pgid := t.cmd.Process.Pid // Setpgid: the leader's pid IS the process group id
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if rss := processTreeRSS(pgid); rss > capBytes {
				m.terminate(t, TaskFailed, false)
				return
			}
		}
	}
}
