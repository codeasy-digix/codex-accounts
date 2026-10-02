//go:build darwin || linux

package main

import (
	"crypto/rand"
	"encoding/hex"
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

type continueJob struct {
	Thread  interruptedThread `json:"conversation"`
	Rollout string            `json:"rollout"`
	Session string            `json:"tmuxSession"`
	State   string            `json:"state"`
	Started time.Time         `json:"startedAt"`
	Ended   *time.Time        `json:"endedAt,omitempty"`
	Account string            `json:"account"`
	Exit    int               `json:"exitCode"`
	Env     []string          `json:"environment,omitempty"`
}

func tmuxArgs(args ...string) []string {
	if socket := os.Getenv("CODEX_ACCOUNTS_TMUX_SOCKET"); socket != "" {
		return append([]string{"-L", socket}, args...)
	}
	return args
}

func tmuxAttach(session string) string {
	args := tmuxArgs("attach", "-t", session)
	for i := range args {
		args[i] = shellQuote(args[i])
	}
	return "tmux " + strings.Join(args, " ")
}

func jobPath(accounts, id string) string {
	return filepath.Join(accounts, ".continue-jobs", id+".json")
}

func activeContinueJob(accounts, id string) (continueJob, bool) {
	var job continueJob
	data, _, err := readPrivateFile(jobPath(accounts, id), true)
	if err != nil || json.Unmarshal(data, &job) != nil || (job.State != "queued" && job.State != "running") {
		return job, false
	}
	if exec.Command("tmux", tmuxArgs("has-session", "-t", "="+job.Session)...).Run() == nil {
		return job, true
	}
	// Allow for the short interval between writing the job and tmux creation.
	return job, job.State == "queued" && time.Since(job.Started) < time.Minute
}

func writeJob(p string, job continueJob) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivateWrite(p, data)
}

func (a application) launchContinuations(shared, accounts string, threads []interruptedThread) error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux is required for continue; install it with Homebrew or your system package manager")
	}
	if _, _, _, err := nativeCommand(nil); err != nil {
		return err
	}
	// Validate/refresh once before launching a batch. Every worker uses this same
	// terminal account; failed authentication launches no partial batch.
	if name := os.Getenv("CODEX_ACCOUNT"); name != "" {
		home, err := prepareHome(shared, accounts, name)
		if err != nil {
			return err
		}
		if _, err := a.selectAccount(home, shared, false); err != nil {
			return err
		}
	}
	root := filepath.Join(accounts, ".continue-jobs")
	if err := realDirectory(root, true); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(root, ".launch.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another terminal is launching continuations; retry when it finishes")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return err
	}
	started, failed := 0, 0
	for _, thread := range threads {
		if thread.Active || threadHasWriter(shared, thread.ID) {
			fmt.Fprintln(a.out, "Skipped active conversation:", thread.ID)
			continue
		}
		if job, active := activeContinueJob(accounts, thread.ID); active {
			fmt.Fprintln(a.out, "Already continuing:", thread.ID, "|", tmuxAttach(job.Session))
			continue
		}
		// Re-read immediately before dispatch; stale cached errors must not restart
		// conversations completed or changed since the list was displayed.
		current, blocked, err := inspectRollout(thread.Rollout)
		if err != nil || !blocked || !sameInterruptedState(current, thread) {
			fmt.Fprintln(a.out, "Skipped changed conversation:", thread.ID)
			continue
		}
		info, err := os.Stat(current.Cwd)
		if err != nil || !info.IsDir() {
			fmt.Fprintln(a.err, "Cannot resume; original working directory is unavailable:", thread.ID)
			failed++
			continue
		}
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		session := "codex-continue-" + thread.ID[:8] + "-" + hex.EncodeToString(random[:])
		name := os.Getenv("CODEX_ACCOUNT")
		if name == "" {
			name = "default"
		}
		job := continueJob{Thread: current, Rollout: thread.Rollout, Session: session, State: "queued", Started: time.Now(), Account: name, Env: os.Environ()}
		p := jobPath(accounts, thread.ID)
		if err := writeJob(p, job); err != nil {
			return err
		}
		// tmux servers preserve the first client's environment. Keep the full
		// caller snapshot in a private file instead of putting secrets in argv.
		command := shellQuote(self) + " __continue-worker " + shellQuote(thread.ID)
		args := tmuxArgs("new-session", "-d", "-s", session, "-c", current.Cwd,
			"-e", "HOME="+os.Getenv("HOME"), "-e", "CODEX_ACCOUNTS_DIR="+accounts,
			"-e", "CODEX_SHARED_HOME="+shared, command)
		cmd := exec.CommandContext(a.ctx, "tmux", args...)
		if err := cmd.Run(); err != nil {
			job.State, job.Env = "failed", nil
			ended := time.Now()
			job.Ended = &ended
			writeJob(p, job)
			fmt.Fprintln(a.err, "Could not start tmux for:", thread.ID)
			failed++
			continue
		}
		fmt.Fprintf(a.out, "Started %s as %s\n  Session: %s\n  Attach: %s\n  Log: %s\n", thread.ID, name, session, tmuxAttach(session), filepath.Join(root, thread.ID+".log"))
		started++
	}
	fmt.Fprintf(a.out, "Started %d conversation(s). Sessions close automatically when the job exits.\n", started)
	if failed > 0 {
		return fmt.Errorf("%d conversation(s) could not start; other jobs continue", failed)
	}
	return nil
}

func (a application) continueWorker(args []string) error {
	if len(args) != 1 || !threadIDPattern.MatchString(args[0]) {
		return errors.New("invalid continuation worker")
	}
	shared, accounts, err := locations()
	if err != nil {
		return err
	}
	p := jobPath(accounts, args[0])
	data, _, err := readPrivateFile(p, false)
	if err != nil {
		return err
	}
	var job continueJob
	if json.Unmarshal(data, &job) != nil || job.Thread.ID != args[0] || job.State != "queued" || len(job.Env) == 0 {
		return errors.New("invalid or already started continuation job")
	}
	lockFD, err := syscall.Open(filepath.Join(accounts, ".continue-jobs", args[0]+".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(lockFD)
	if err := syscall.Flock(lockFD, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("this conversation already has a continuation worker")
	}
	// The worker starts in a separate process, so replacing its environment does
	// not affect the parent terminal or another tmux session.
	tmuxEnvironment := map[string]string{"TMUX": os.Getenv("TMUX"), "TMUX_PANE": os.Getenv("TMUX_PANE"), "TERM": os.Getenv("TERM")}
	os.Clearenv()
	for _, item := range job.Env {
		if key, value, ok := cutEnvironment(item); ok {
			os.Setenv(key, value)
		}
	}
	for key, value := range tmuxEnvironment {
		if value != "" {
			os.Setenv(key, value)
		}
	}
	job.Env, job.State = nil, "running"
	defer func() {
		ended := time.Now()
		job.Ended = &ended
		writeJob(p, job)
		// Remove only this job's session, even when the user's tmux config keeps
		// dead panes with remain-on-exit. Other sessions and the tmux server stay.
		if strings.HasPrefix(job.Session, "codex-continue-") && os.Getenv("TMUX") != "" {
			exec.Command("tmux", tmuxArgs("kill-session", "-t", "="+job.Session)...).Run()
		}
	}()
	if err := writeJob(p, job); err != nil {
		job.State, job.Exit = "failed", 1
		return err
	}
	if threadHasWriter(shared, job.Thread.ID) {
		job.State, job.Exit = "failed", 1
		return errors.New("conversation became active before continuation; it was not restarted")
	}
	current, blocked, err := inspectRollout(job.Rollout)
	if err != nil || !blocked || !sameInterruptedState(current, job.Thread) {
		job.State, job.Exit = "failed", 1
		return errors.New("conversation changed before continuation; it was not restarted")
	}
	if err := os.Chdir(job.Thread.Cwd); err != nil {
		job.State, job.Exit = "failed", 1
		return errors.New("original working directory is no longer available")
	}
	logFD, err := syscall.Open(filepath.Join(accounts, ".continue-jobs", args[0]+".log"), syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		job.State, job.Exit = "failed", 1
		return err
	}
	log := os.NewFile(uintptr(logFD), "continuation output")
	defer log.Close()
	fmt.Fprintf(log, "\n--- Continue %s, account %s, %s ---\n", job.Thread.ID, job.Account, time.Now().Format(time.RFC3339))
	binary, nativeArgs, env, err := nativeCommand([]string{"exec", "resume", "--skip-git-repo-check", job.Thread.ID, "continue"})
	if err != nil {
		job.State, job.Exit = "failed", 1
		return err
	}
	cmd := exec.CommandContext(a.ctx, binary, nativeArgs[1:]...)
	cmd.Env, cmd.Dir = env, job.Thread.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, io.MultiWriter(a.out, log), io.MultiWriter(a.err, log)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err = cmd.Run()
	job.State = "completed"
	if err != nil {
		job.State, job.Exit = "failed", 1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			job.Exit = exit.ExitCode()
		}
		return errors.New("continuation stopped; check its private log and conversation history")
	}
	return nil
}

func sameInterruptedState(current, listed interruptedThread) bool {
	return current.ID == listed.ID && current.Cwd == listed.Cwd && current.Category == listed.Category &&
		current.Reason == listed.Reason && current.Stopped == listed.Stopped
}
