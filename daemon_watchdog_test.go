//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestManagedRuntimeIgnoresDeadPIDWithoutChangingState(t *testing.T) {
	_, shared, _ := sandbox(t)
	binary := managedRuntimeFixture(t, shared, "0.163.0")
	selectManagedFixture(t, shared, binary)
	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	child.Process.Kill()
	child.Wait()
	dir := filepath.Join(shared, "app-server-daemon")
	os.MkdirAll(dir, 0700)
	record := []byte(fmt.Sprintf(`{"pid":%d,"processIdentity":"retained-fixture"}`, child.Process.Pid))
	p := filepath.Join(dir, "daemon.pid")
	if err := os.WriteFile(p, record, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_ACCOUNTS_TEST_DAEMON_EXIT", "7")
	t.Setenv("CODEX_ACCOUNTS_TEST_DAEMON_STATUS", "PRIVATE-ERROR")
	selected, err := selectRuntime()
	if err != nil || selected.binary != binary {
		t.Fatalf("dead server blocked account runtime: %+v %v", selected, err)
	}
	after, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(record, after) {
		t.Fatal("selector changed native PID record")
	}
}

func TestDaemonManagementUsesSharedHomeAndRespectsStopIntent(t *testing.T) {
	_, shared, accounts := sandbox(t)
	binary := managedRuntimeFixture(t, shared, "0.163.0")
	selectManagedFixture(t, shared, binary)
	home := seed(t, shared, accounts, "ryu")
	t.Setenv("CODEX_ACCOUNT", "ryu")
	t.Setenv("CODEX_HOME", home)
	if err := setWatchdogEnabled(shared, true, true); err != nil {
		t.Fatal(err)
	}
	root, _ := watchdogRoot()
	for _, action := range []string{"stop", "start", "restart", "version", "--help"} {
		selected, args, env, err := nativeCommand([]string{"-c", "model=fixture", "app-server", "daemon", action})
		if err != nil {
			t.Fatal(err)
		}
		if selected != binary || slices.Contains(args, "--no-daemon") || !slices.Contains(env, "CODEX_HOME="+shared) || !slices.Contains(env, "CODEX_SQLITE_HOME="+shared) || !slices.Contains(env, "CODEX_ACCOUNT=") {
			t.Fatalf("wrong daemon environment: %v %v", args, env)
		}
		settings, _, err := readWatchdog(root)
		if err != nil || settings.Enabled != (action != "stop") {
			t.Fatalf("manual %s lost recovery intent: %+v %v", action, settings, err)
		}
		if os.Getenv("CODEX_HOME") != home || os.Getenv("CODEX_ACCOUNT") != "ryu" {
			t.Fatal("daemon command changed parent account")
		}
	}
}

func TestDaemonStopWaitsForInFlightRecovery(t *testing.T) {
	_, shared, _ := sandbox(t)
	if err := setWatchdogEnabled(shared, true, true); err != nil {
		t.Fatal(err)
	}
	root, _ := watchdogRoot()
	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- withWatchdogLock(context.Background(), root, func() error { close(locked); <-release; return nil })
	}()
	<-locked
	stopped := make(chan error, 1)
	go func() { stopped <- setWatchdogEnabled(shared, false, false) }()
	select {
	case err := <-stopped:
		t.Fatalf("stop raced active recovery: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	settings, _, err := readWatchdog(root)
	if err != nil || settings.Enabled {
		t.Fatal("manual stop did not persist")
	}
}

func TestDaemonWatchdogRecoversKilledProcessAndHonorsPause(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	root, shared, _ := sandbox(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CODEX_ACCOUNTS_STATE_HOME", "")
	t.Setenv("CODEX_ACCOUNTS_TMUX_SOCKET", fmt.Sprintf("codex-daemon-test-%d", time.Now().UnixNano()))
	t.Cleanup(func() { exec.Command("tmux", tmuxArgs("kill-server")...).Run() })
	binary := managedRuntimeFixture(t, shared, "0.163.0")
	selectManagedFixture(t, shared, binary)
	script := `#!/bin/sh
if [ "$1 $2 $3" = 'app-server daemon start' ]; then
  [ -z "$CODEX_ACCOUNT" ] && [ "$CODEX_HOME" = "$CODEX_SHARED_HOME" ] || exit 91
  /bin/mkdir -p "$CODEX_HOME/app-server-daemon"
  /bin/sleep 120 >/dev/null 2>&1 &
  printf '{"pid":%s}\n' "$!" > "$CODEX_HOME/app-server-daemon/daemon.pid"
  exit 0
fi
exit 92
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	a := application{out: &out, err: io.Discard, in: strings.NewReader(""), ctx: context.Background()}
	if output, err := exec.Command(testCLI, "account", "--daemon-watchdog", "start").CombinedOutput(); err != nil {
		watchRoot, _ := watchdogRoot()
		data, _ := os.ReadFile(filepath.Join(watchRoot, "watchdog.log"))
		t.Fatalf("%v; startup: %s; watcher output: %s", err, output, data)
	}
	t.Cleanup(func() {
		a.daemonWatchdog([]string{"stop"})
		if pid, alive, _ := daemonProcess(shared); alive {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	waitPID := func(previous int) int {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if pid, alive, err := daemonProcess(shared); err == nil && alive && pid != previous {
				return pid
			}
			time.Sleep(50 * time.Millisecond)
		}
		watchRoot, _ := watchdogRoot()
		data, _ := os.ReadFile(filepath.Join(watchRoot, "watchdog.log"))
		pane, _ := exec.Command("tmux", tmuxArgs("capture-pane", "-p", "-t", daemonWatchdogSession)...).Output()
		t.Fatalf("watchdog did not recover dead process; previous=%d log=%s pane=%s", previous, data, pane)
		return 0
	}
	first := waitPID(0)
	if err := syscall.Kill(first, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	second := waitPID(first)
	if err := setWatchdogEnabled(shared, false, false); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(second, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5500 * time.Millisecond)
	if _, alive, err := daemonProcess(shared); err != nil || alive {
		t.Fatal("explicit stop was automatically undone")
	}
	if err := a.daemonWatchdog([]string{"stop"}); err != nil {
		t.Fatal(err)
	}
	if watchdogSessionExists() {
		t.Fatal("temporary watchdog session left behind")
	}
}

func TestAccountHelpUsesPublicCodexCommands(t *testing.T) {
	var out bytes.Buffer
	a := application{out: &out, err: io.Discard, in: strings.NewReader(""), ctx: context.Background()}
	if err := a.account([]string{"--help"}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "codex-accounts") || !strings.Contains(text, "codex account NAME") || !strings.Contains(text, "codex app-server daemon restart") {
		t.Fatal("public help still exposes internal controller commands")
	}
	cmd := exec.Command(testCLI, "account", "--list", "unexpected")
	data, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(data), "codex: usage: codex account --list") || strings.Contains(string(data), "codex-accounts:") {
		t.Fatal("public error prefix changed")
	}
}

func TestWatchdogStatusSafeForAlreadyLoadedShellIntegration(t *testing.T) {
	sandbox(t)
	var out, notice bytes.Buffer
	a := application{out: &out, err: &notice, in: strings.NewReader(""), ctx: context.Background()}
	if err := a.account([]string{"--shell", "--daemon-watchdog", "status"}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || !strings.Contains(notice.String(), "Automatic recovery:") {
		t.Fatal("status output could be evaluated as shell code")
	}
}

func TestNativeDaemonFlagsAndHelpDoNotChangeStopIntent(t *testing.T) {
	_, shared, _ := sandbox(t)
	binary := managedRuntimeFixture(t, shared, "0.163.0")
	selectManagedFixture(t, shared, binary)
	if err := setWatchdogEnabled(shared, true, true); err != nil {
		t.Fatal(err)
	}
	root, _ := watchdogRoot()
	for _, args := range [][]string{{"app-server", "daemon", "stop", "--help"}, {"app-server", "daemon", "restart", "-h"}} {
		if _, _, _, err := nativeCommand(args); err != nil {
			t.Fatal(err)
		}
		settings, _, err := readWatchdog(root)
		if err != nil || !settings.Enabled {
			t.Fatal("daemon help changed recovery setting")
		}
	}
	args := []string{"app-server", "-c", "model=fixture", "daemon", "--config", "model=fixture", "stop"}
	if action, matched := nativeDaemonCommand(args); !matched || action != "stop" {
		t.Fatal("daemon global flags lost action")
	}
	if _, _, _, err := nativeCommand(args); err != nil {
		t.Fatal(err)
	}
	settings, _, err := readWatchdog(root)
	if err != nil || settings.Enabled {
		t.Fatal("daemon stop with global flags was not honored")
	}
}

func TestDaemonProcessRejectsInvalidPIDAndSupportsNewRecordName(t *testing.T) {
	_, shared, _ := sandbox(t)
	dir := filepath.Join(shared, "app-server-daemon")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(`{"pid":-1}`), 0600)
	if _, alive, err := daemonProcess(shared); err == nil || alive {
		t.Fatal("invalid PID accepted")
	}
	data, _ := json.Marshal(map[string]int{"pid": os.Getpid()})
	os.WriteFile(filepath.Join(dir, "app-server.pid"), data, 0600)
	if pid, alive, err := daemonProcess(shared); err != nil || !alive || pid != os.Getpid() {
		t.Fatal("new native record ignored")
	}
}
