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
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

var version = "dev"
var codexVersion = "0.159.3"

type application struct {
	out, err io.Writer
	in       io.Reader
	ctx      context.Context
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()
	app := application{os.Stdout, os.Stderr, os.Stdin, ctx}
	if err := app.execute(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "codexm:", err)
		if ctx.Err() != nil {
			os.Exit(130)
		}
		os.Exit(1)
	}
}

func (a application) execute(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-V":
			fmt.Fprintf(a.out, "Codex account extension %s (bundled Codex %s)\n", version, codexVersion)
			return nil
		case "help", "--help", "-h":
			fmt.Fprint(a.out, helpText)
			return nil
		case "shell-init":
			if len(args) != 2 || (args[1] != "zsh" && args[1] != "bash") {
				return errors.New("shell integration requires zsh or bash")
			}
			fmt.Fprint(a.out, shellInit)
			return nil
		case "account":
			return a.account(args[1:])
		case "continue":
			return a.continueAccounts(args[1:])
		case "__native":
			// The shell wrapper reserves only account/continue. Preserve the
			// upstream CLI's help, version, doctor and all future commands.
			return a.run(args[1:])
		case "__continue-worker":
			return a.continueWorker(args[1:])
		case "__daemon-watchdog":
			if len(args) != 1 {
				return errors.New("daemon watchdog worker does not accept arguments")
			}
			return a.runDaemonWatchdog()
		case "doctor":
			return a.doctor(args[1:])
		case "--account":
			if len(args) < 2 {
				return errors.New("use codexm account NAME and open the job through codexm")
			}
			if args[1] == "default" {
				for _, key := range []string{"CODEX_ACCOUNT", "CODEX_HOME", "CODEX_SQLITE_HOME"} {
					os.Unsetenv(key)
				}
			} else {
				shared, accounts, err := locations()
				if err != nil {
					return err
				}
				home, err := prepareHome(shared, accounts, args[1])
				if err != nil {
					return err
				}
				if _, err := a.selectAccount(home, shared, false); err != nil {
					return err
				}
				os.Setenv("CODEX_ACCOUNT", args[1])
				os.Setenv("CODEX_HOME", home)
				os.Setenv("CODEX_SQLITE_HOME", shared)
			}
			args = args[2:]
			if len(args) > 0 && args[0] == "continue" {
				return a.continueAccounts(args[1:])
			}
		case "run":
			args = args[1:]
		}
	}
	return a.run(args)
}

func (a application) account(args []string) error {
	shell := false
	if len(args) > 0 && args[0] == "--shell" {
		shell = true
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--daemon-watchdog" {
		// Older shells treat every unfamiliar account flag as a selection and
		// eval --shell stdout. Never emit human-readable text as shell code.
		if shell {
			a.out = a.err
		}
		return a.daemonWatchdog(args[1:])
	}
	shared, accounts, err := locations()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return a.showStatus(a.out, shared)
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(a.out, helpText)
		return nil
	}
	if args[0] == "--list" {
		if len(args) != 1 {
			return errors.New("usage: codexm account --list")
		}
		names, err := listAccounts(accounts)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Fprintln(a.out, "No registered accounts.")
		}
		selected, _ := defaultAccount(shared, accounts)
		for _, name := range names {
			if name == selected {
				fmt.Fprintln(a.out, name, "(default)")
			} else {
				fmt.Fprintln(a.out, name)
			}
		}
		return nil
	}
	if args[0] == "default" || args[0] == "--default" {
		if len(args) != 1 {
			return errors.New("default does not accept --login; use codex login")
		}
		for _, key := range []string{"CODEX_ACCOUNT", "CODEX_HOME", "CODEX_SQLITE_HOME"} {
			os.Unsetenv(key)
		}
		w := a.out
		if shell {
			w = a.err
		}
		if err := a.showStatus(w, shared); err != nil {
			return err
		}
		if shell {
			fmt.Fprintln(a.out, "unset CODEX_ACCOUNT CODEX_HOME CODEX_SQLITE_HOME")
		}
		return nil
	}
	options, err := parseLoginOptions(args[1:])
	if err != nil {
		return err
	}
	if options.makeDefault {
		return a.makeDefaultWithLogin(shared, accounts, args[0], shell, options)
	}
	home, err := prepareHome(shared, accounts, args[0])
	if err != nil {
		return err
	}
	info, err := a.selectAccountWithLogin(home, shared, options, nil)
	if err != nil {
		return err
	}
	w := a.out
	if shell {
		w = a.err
	}
	printStatus(w, args[0], home, shared, info)
	if shell {
		fmt.Fprintf(a.out, "export CODEX_ACCOUNT=%s\nexport CODEX_HOME=%s\nexport CODEX_SQLITE_HOME=%s\n",
			shellQuote(args[0]), shellQuote(home), shellQuote(shared))
	} else {
		fmt.Fprintf(a.out, "Use codexm account %s, then open the conversation through codexm.\n", args[0])
	}
	return nil
}

func (a application) run(args []string) error {
	binary, nativeArgs, env, err := nativeCommand(args)
	if err != nil {
		return err
	}
	// Replace the process so the native CLI owns signals, terminal I/O and exit codes.
	return syscall.Exec(binary, nativeArgs, env)
}

func nativeCommand(args []string) (string, []string, []string, error) {
	shared, accounts, err := locations()
	if err != nil {
		return "", nil, nil, err
	}
	daemonAction, daemonManagement := nativeDaemonCommand(args)
	selected, err := selectRuntimeInspect(!daemonManagement)
	if err != nil {
		return "", nil, nil, err
	}
	binary := selected.binary
	env := runtimeEnvironment(binary, os.Environ())
	nativeArgs := []string{binary}
	command := upstreamCommand(args)
	if command != "update" && !daemonManagement {
		// Default and named sessions own their runtime so exiting releases the
		// conversation without waiting for the shared daemon's idle timeout.
		options := args
		if end := slices.Index(options, "--"); end >= 0 {
			options = options[:end]
		}
		if !slices.Contains(options, "--no-daemon") {
			nativeArgs = append(nativeArgs, "--no-daemon")
		}
	}
	if command == "update" && selected.source == "daemon" {
		// CLI and daemon share one managed package. Updating a separate CLI
		// installation would leave account execution on the previous release.
		index := upstreamCommandIndex(args)
		mapped := append([]string{}, args[:index]...)
		mapped = append(mapped, "app-server", "daemon", "update")
		args = append(mapped, args[index+1:]...)
	}
	if name := os.Getenv("CODEX_ACCOUNT"); daemonManagement || (command == "update" && (name != "" || selected.source == "daemon")) {
		// Installation is shared across accounts. A standalone updater must
		// locate its releases under the original home, not a credential home.
		env = setEnvironment(env, "CODEX_HOME", shared)
		env = setEnvironment(env, "CODEX_SQLITE_HOME", shared)
		env = setEnvironment(env, "CODEX_ACCOUNT", "")
		if daemonManagement {
			switch daemonAction {
			case "stop":
				if err := setWatchdogEnabled(shared, false, false); err != nil {
					return "", nil, nil, err
				}
			case "start", "restart", "bootstrap":
				if err := setWatchdogEnabled(shared, true, false); err != nil {
					return "", nil, nil, err
				}
			}
		}
	} else if name != "" {
		home, err := prepareHome(shared, accounts, name)
		if err != nil {
			return "", nil, nil, err
		}
		if os.Getenv("CODEX_HOME") != home {
			return "", nil, nil, errors.New("CODEX_HOME does not match the legacy account; use codexm account NAME in the manager")
		}
		env = accountEnvironment(env, home, shared, true)
		nativeArgs = append(nativeArgs, configArguments(shared, true)...)
	} else if os.Getenv("CODEX_HOME") == "" && os.Getenv("CODEX_SHARED_HOME") != "" {
		env = setEnvironment(env, "CODEX_HOME", shared)
	}
	nativeArgs = append(nativeArgs, args...)
	return binary, nativeArgs, env, nil
}

func upstreamCommand(args []string) string {
	if index := upstreamCommandIndex(args); index >= 0 {
		return args[index]
	}
	return ""
}

func upstreamCommandIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			return -1
		case "-c", "--config", "-C", "--cd", "-p", "--profile", "-m", "--model", "-s", "--sandbox", "-a", "--ask-for-approval", "-P", "--permission-profile", "-i", "--image", "--add-dir", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider":
			i++
		default:
			if !strings.HasPrefix(args[i], "-") {
				return i
			}
		}
	}
	return -1
}

func (a application) doctor(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		return errors.New("runtime diagnostics accept only --json")
	}
	selected, err := selectRuntime()
	if err != nil {
		return err
	}
	binary := selected.binary
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = runtimeEnvironment(binary, os.Environ())
	data, err := cmd.Output()
	if err != nil {
		return errors.New("selected Codex runtime could not start")
	}
	_, tmuxErr := exec.LookPath("tmux")
	result := map[string]any{"version": version, "runtime": binary, "runtimeSource": selected.source, "codexVersion": strings.TrimSpace(string(data)), "runtimeDependencies": []string{"tmux"}, "tmuxAvailable": tmuxErr == nil}
	if len(args) == 1 {
		return json.NewEncoder(a.out).Encode(result)
	}
	fmt.Fprintf(a.out, "Codex account extension: %s\nRuntime: %s (%s)\n%s\nNo Python, Node.js, or Go runtime required. Continue uses tmux (available: %t).\n", version, binary, selected.source, cleanText(string(data)), tmuxErr == nil)
	return nil
}

func runtimeBinary() (string, error) {
	selected, err := selectRuntime()
	return selected.binary, err
}

func installedRuntime(self string) string {
	selfInfo, _ := os.Stat(self)
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, "codex")
		if !executable(candidate) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil || filepath.Base(resolved) == "codex-accounts" {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || (selfInfo != nil && os.SameFile(selfInfo, info)) {
			continue
		}
		// Avoid recursively invoking a user-created shell shim for this
		// controller. Official npm launcher scripts remain supported.
		file, err := os.Open(resolved)
		if err != nil {
			continue
		}
		prefix, readErr := io.ReadAll(io.LimitReader(file, 8192))
		file.Close()
		if readErr != nil || (strings.HasPrefix(string(prefix), "#!") && strings.Contains(string(prefix), "codex-accounts")) {
			continue
		}
		return resolved
	}
	return ""
}

func executable(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func runtimeEnvironment(binary string, env []string) []string {
	root := filepath.Dir(filepath.Dir(binary))
	p := filepath.Join(root, "codex-path") + string(os.PathListSeparator) + filepath.Dir(binary)
	for _, item := range env {
		if strings.HasPrefix(item, "PATH=") {
			p += string(os.PathListSeparator) + strings.TrimPrefix(item, "PATH=")
			break
		}
	}
	return setEnvironment(env, "PATH", p)
}

const helpText = `codexm account — account management has moved to codexm

  codexm account                         Saved worker account and existing logins
  codexm account NAME                    Select the default for new/imported jobs
  codexm account NAME --login --browser  Sign in using the local browser
  codexm account NAME --login --device   Sign in using a device code
  codexm account --list                  List account nicknames
  codexm setup --manager NAME            Select the separate manager account
  codexm daemon watch                    Enable automatic official daemon recovery
  codexm daemon unwatch                  Stop recovery; leave official daemon running
  codexm daemon status [--json]          Show recovery and daemon state
  codexm daemon stop                     Stop daemon and pause automatic recovery
  codexm                                Manage/open existing or new conversations

Official daemon:
  codex app-server daemon start
  codex app-server daemon restart
  codex app-server daemon version
Use codex --help for the unchanged official CLI.
New codexm selection does not change this shell or the machine's native login.
Existing managed jobs keep their accounts. Legacy compatibility flags remain
available for existing installations; new distribution belongs to codexm.
`

const shellInit = `# codex-accounts: a separate account in each shell, with shared local history.
codex_account() {
  local _codex_accounts_env
  case "${1-}" in
    ""|--list|--help|-h|--daemon-watchdog) command codex-accounts account "$@" ;;
    *)
      _codex_accounts_env="$(command codex-accounts account --shell "$@")" || return $?
      eval "$_codex_accounts_env"
      ;;
  esac
}
codex() {
  case "${1-}" in
    account) shift; codex_account "$@" ;;
    continue) shift; command codex-accounts continue "$@" ;;
    *) command codex-accounts __native "$@" ;;
  esac
}
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
