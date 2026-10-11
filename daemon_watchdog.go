//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const daemonWatchdogSession = "codex-daemon-watchdog"

type watchdogSettings struct {
	Enabled    bool   `json:"enabled"`
	SharedHome string `json:"sharedHome"`
}

func watchdogRoot() (string, error) {
	if root := os.Getenv("CODEX_ACCOUNTS_STATE_HOME"); root != "" {
		return filepath.Abs(filepath.Join(root, "daemon-watchdog"))
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Abs(filepath.Join(state, "codex-accounts", "daemon-watchdog"))
}

func readWatchdog(root string) (watchdogSettings, bool, error) {
	var settings watchdogSettings
	data, exists, err := readPrivateFile(filepath.Join(root, "settings.json"), true)
	if err != nil || !exists {
		return settings, exists, err
	}
	if json.Unmarshal(data, &settings) != nil || !filepath.IsAbs(settings.SharedHome) {
		return settings, true, errors.New("invalid Codex daemon watchdog settings")
	}
	return settings, true, nil
}

// Serialize intent changes against a recovery attempt. In particular a manual
// daemon stop waits for an in-flight start, then prevents every subsequent start.
func withWatchdogLock(ctx context.Context, root string, f func() error) error {
	if err := realDirectory(root, true); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(root, "operation.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	for {
		if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return f()
}

func setWatchdogEnabled(shared string, enabled, create bool) error {
	root, err := watchdogRoot()
	if err != nil {
		return err
	}
	if !create {
		if _, exists, err := readWatchdog(root); err != nil || !exists {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return withWatchdogLock(ctx, root, func() error {
		settings, exists, err := readWatchdog(root)
		if err != nil {
			return err
		}
		if exists && settings.SharedHome != shared {
			return errors.New("daemon watchdog belongs to another Codex home")
		}
		if !exists && !create {
			return nil
		}
		data, _ := json.Marshal(watchdogSettings{Enabled: enabled, SharedHome: shared})
		return atomicPrivateWrite(filepath.Join(root, "settings.json"), data)
	})
}

func watchdogSessionExists() bool {
	return exec.Command("tmux", tmuxArgs("has-session", "-t", "="+daemonWatchdogSession)...).Run() == nil
}

func watchdogSessionOwned(root string) bool {
	data, err := exec.Command("tmux", tmuxArgs("show-options", "-v", "-t", daemonWatchdogSession, "@codex-accounts-watchdog")...).Output()
	return err == nil && strings.TrimSpace(string(data)) == root
}

func (a application) daemonWatchdog(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	if len(args) > 2 || (len(args) == 2 && (args[0] != "status" || args[1] != "--json")) {
		return errors.New("use codexm daemon watch|unwatch|status [--json]")
	}
	shared, _, err := locations()
	if err != nil {
		return err
	}
	root, err := watchdogRoot()
	if err != nil {
		return err
	}
	switch args[0] {
	case "start":
		selected, err := selectRuntimeInspect(false)
		if err != nil {
			return err
		}
		if selected.source != "daemon" {
			return errors.New("install the official managed package with codex app-server daemon bootstrap before enabling its watchdog")
		}
		if _, err := exec.LookPath("tmux"); err != nil {
			return errors.New("tmux is required for the Codex daemon watchdog")
		}
		if watchdogSessionExists() && !watchdogSessionOwned(root) {
			return errors.New("tmux session codex-daemon-watchdog is owned by another process; it was not changed")
		}
		if err := setWatchdogEnabled(shared, true, true); err != nil {
			return err
		}
		if !watchdogSessionExists() {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			self, err = filepath.EvalSymlinks(self)
			if err != nil {
				return err
			}
			parts := []string{"exec", "env", shellQuote("CODEX_SHARED_HOME=" + shared), shellQuote("CODEX_HOME=" + shared), shellQuote("CODEX_SQLITE_HOME=" + shared), shellQuote("CODEX_ACCOUNT="), shellQuote("CODEX_ACCOUNTS_STATE_HOME=" + filepath.Dir(root)), shellQuote("PATH=" + os.Getenv("PATH")), shellQuote("CODEX_ACCOUNTS_RUNTIME=")}
			for _, variable := range os.Environ() {
				if strings.HasPrefix(variable, "CODEX_ACCOUNTS_TEST_") {
					parts = append(parts, shellQuote(variable))
				}
			}
			parts = append(parts, shellQuote(self), "__daemon-watchdog")
			if err := exec.Command("tmux", tmuxArgs("new-session", "-d", "-s", daemonWatchdogSession, "-c", shared, strings.Join(parts, " "))...).Run(); err != nil {
				return errors.New("could not start Codex daemon watchdog tmux session")
			}
			if data, err := exec.Command("tmux", tmuxArgs("set-option", "-t", daemonWatchdogSession, "@codex-accounts-watchdog", root)...).CombinedOutput(); err != nil {
				return fmt.Errorf("could not record Codex daemon watchdog tmux ownership: %s", cleanText(string(data)))
			}
		}
		fmt.Fprintf(a.out, "Codex daemon recovery enabled (checks every 5 seconds; no model calls).\nSession: %s\nAttach: %s\nLog: %s\n", daemonWatchdogSession, tmuxAttach(daemonWatchdogSession), filepath.Join(root, "watchdog.log"))
		return nil
	case "stop":
		if err := setWatchdogEnabled(shared, false, false); err != nil {
			return err
		}
		if watchdogSessionExists() {
			if !watchdogSessionOwned(root) {
				return errors.New("watchdog tmux ownership could not be verified; session was not changed")
			}
			if err := exec.Command("tmux", tmuxArgs("kill-session", "-t", "="+daemonWatchdogSession)...).Run(); err != nil {
				return err
			}
		}
		fmt.Fprintln(a.out, "Automatic Codex daemon recovery stopped. The official daemon was not stopped.")
		return nil
	case "status":
		settings, _, err := readWatchdog(root)
		if err != nil {
			return err
		}
		pid, running, processErr := daemonProcess(shared)
		watching := watchdogSessionExists() && watchdogSessionOwned(root)
		status := map[string]any{"enabled": settings.Enabled, "watchdogRunning": watching, "daemonRunning": running, "daemonPid": pid, "session": daemonWatchdogSession, "log": filepath.Join(root, "watchdog.log"), "modelCalls": 0}
		if processErr != nil {
			status["error"] = processErr.Error()
		}
		if len(args) == 2 {
			return json.NewEncoder(a.out).Encode(status)
		}
		fmt.Fprintf(a.out, "Automatic recovery: %t | Watchdog running: %t | Official daemon running: %t (PID %d)\nAttach: %s\nLog: %s\n", settings.Enabled, watching, running, pid, tmuxAttach(daemonWatchdogSession), filepath.Join(root, "watchdog.log"))
		if processErr != nil {
			fmt.Fprintln(a.out, processErr)
		}
		return nil
	default:
		return errors.New("use codexm daemon watch|unwatch|status [--json]")
	}
}

func (a application) runDaemonWatchdog() error {
	root, err := watchdogRoot()
	if err != nil {
		return err
	}
	if err := realDirectory(root, true); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(root, "worker.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("Codex daemon watchdog already running")
	}
	logFD, err := syscall.Open(filepath.Join(root, "watchdog.log"), syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	logFile := os.NewFile(uintptr(logFD), "watchdog log")
	defer logFile.Close()
	log := io.MultiWriter(a.out, logFile)
	last := ""
	nextAttempt := time.Time{}
	delay := 5 * time.Second
	for a.ctx.Err() == nil {
		state := "paused"
		err := withWatchdogLock(a.ctx, root, func() error {
			settings, exists, err := readWatchdog(root)
			if err != nil {
				return err
			}
			if !exists || !settings.Enabled {
				return nil
			}
			pid, alive, err := daemonProcess(settings.SharedHome)
			if err != nil {
				return err
			}
			if alive {
				state = fmt.Sprintf("running PID %d", pid)
				nextAttempt = time.Time{}
				delay = 5 * time.Second
				return nil
			}
			state = "daemon absent; waiting to recover"
			if time.Now().Before(nextAttempt) {
				return nil
			}
			selected, err := selectRuntimeInspect(false)
			if err != nil {
				return err
			}
			if selected.source != "daemon" {
				return errors.New("official managed Codex package is unavailable")
			}
			fmt.Fprintf(log, "%s daemon absent; starting official Codex\n", time.Now().Format(time.RFC3339))
			ctx, cancel := context.WithTimeout(a.ctx, 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, selected.binary, "app-server", "daemon", "start")
			cmd.Env = accountEnvironment(runtimeEnvironment(selected.binary, os.Environ()), settings.SharedHome, settings.SharedHome, false)
			cmd.Env = setEnvironment(cmd.Env, "CODEX_ACCOUNT", "")
			cmd.Dir = settings.SharedHome
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			if err := cmd.Run(); err != nil {
				return errors.New("official Codex daemon start failed; retrying with backoff")
			}
			pid, alive, err = daemonProcess(settings.SharedHome)
			if err != nil {
				return err
			}
			if !alive {
				return errors.New("official Codex daemon did not publish a running process; retrying with backoff")
			}
			state = fmt.Sprintf("recovered PID %d", pid)
			nextAttempt = time.Time{}
			delay = 5 * time.Second
			return nil
		})
		if err != nil {
			if a.ctx.Err() != nil {
				break
			}
			state = err.Error()
			nextAttempt = time.Now().Add(delay)
			delay = min(60*time.Second, delay*2)
		}
		if state != last {
			fmt.Fprintf(log, "%s %s\n", time.Now().Format(time.RFC3339), state)
			last = state
		}
		select {
		case <-a.ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}

func nativeDaemonCommand(args []string) (string, bool) {
	index := upstreamCommandIndex(args)
	if index < 0 || args[index] != "app-server" {
		return "", false
	}
	group := upstreamCommandIndex(args[index+1:])
	if group < 0 || args[index+1+group] != "daemon" {
		return "", false
	}
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return "", true
		}
	}
	action := upstreamCommandIndex(args[index+group+2:])
	if action < 0 {
		return "", true
	}
	return args[index+group+2+action], true
}
