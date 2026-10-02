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
		fmt.Fprintln(os.Stderr, "codex-accounts:", err)
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
			fmt.Fprintf(a.out, "codex-accounts %s (Codex %s)\n", version, codexVersion)
			return nil
		case "help", "--help", "-h":
			fmt.Fprint(a.out, helpText)
			return nil
		case "shell-init":
			if len(args) != 2 || (args[1] != "zsh" && args[1] != "bash") {
				return errors.New("usage: codex-accounts shell-init zsh|bash")
			}
			fmt.Fprint(a.out, shellInit)
			return nil
		case "account":
			return a.account(args[1:])
		case "continue":
			return a.continueAccounts(args[1:])
		case "__continue-worker":
			return a.continueWorker(args[1:])
		case "doctor":
			return a.doctor(args[1:])
		case "--account":
			if len(args) < 2 {
				return errors.New("usage: codex-accounts --account NAME [Codex arguments]")
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
			return errors.New("usage: codex-accounts account --list")
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
	if len(args) == 2 && args[1] == "default" && args[0] != "default" && args[0] != "--default" {
		return a.makeDefault(shared, accounts, args[0], shell)
	}
	if len(args) > 2 || (len(args) == 2 && args[1] != "--login") {
		return errors.New("usage: codex-accounts account NAME [--login|default]")
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
	home, err := prepareHome(shared, accounts, args[0])
	if err != nil {
		return err
	}
	info, err := a.selectAccount(home, shared, len(args) == 2)
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
		fmt.Fprintf(a.out, "Use codex-accounts --account %s, or enable shell-init for per-terminal selection.\n", args[0])
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
	binary, err := runtimeBinary()
	if err != nil {
		return "", nil, nil, err
	}
	env := runtimeEnvironment(binary, os.Environ())
	nativeArgs := []string{binary, "--no-daemon"}
	if name := os.Getenv("CODEX_ACCOUNT"); name != "" {
		home, err := prepareHome(shared, accounts, name)
		if err != nil {
			return "", nil, nil, err
		}
		if os.Getenv("CODEX_HOME") != home {
			return "", nil, nil, errors.New("CODEX_HOME does not match the selected account; run codex account NAME again")
		}
		env = accountEnvironment(env, home, shared, true)
		nativeArgs = append(nativeArgs, configArguments(shared, true)...)
	} else if os.Getenv("CODEX_HOME") == "" && os.Getenv("CODEX_SHARED_HOME") != "" {
		env = setEnvironment(env, "CODEX_HOME", shared)
	}
	nativeArgs = append(nativeArgs, args...)
	return binary, nativeArgs, env, nil
}

func (a application) doctor(args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		return errors.New("usage: codex-accounts doctor [--json]")
	}
	binary, err := runtimeBinary()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = runtimeEnvironment(binary, os.Environ())
	data, err := cmd.Output()
	if err != nil {
		return errors.New("bundled Codex runtime could not start")
	}
	_, tmuxErr := exec.LookPath("tmux")
	result := map[string]any{"version": version, "runtime": binary, "codexVersion": strings.TrimSpace(string(data)), "runtimeDependencies": []string{"tmux"}, "tmuxAvailable": tmuxErr == nil}
	if len(args) == 1 {
		return json.NewEncoder(a.out).Encode(result)
	}
	fmt.Fprintf(a.out, "codex-accounts: %s\nRuntime: %s\n%s\nNo Python, Node.js, or Go runtime required. Continue uses tmux (available: %t).\n", version, binary, cleanText(string(data)), tmuxErr == nil)
	return nil
}

func runtimeBinary() (string, error) {
	if override := os.Getenv("CODEX_ACCOUNTS_RUNTIME"); override != "" {
		p, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		if executable(p) {
			return p, nil
		}
		return "", errors.New("CODEX_ACCOUNTS_RUNTIME is not an executable file")
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(self), "..", "libexec", "codex", "bin", "codex")
	if executable(p) {
		return filepath.Clean(p), nil
	}
	return "", errors.New("bundled Codex is missing; reinstall with brew reinstall codex-accounts")
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

const helpText = `codex-accounts: local Codex accounts with shared conversations

  codex-accounts account                  Account, workspace and remaining limits
  codex-accounts account NAME [--login]   Validate/login with a device code
  codex-accounts account NAME default    Set the machine's default login
  codex-accounts account --list           List account nicknames
  codex-accounts --account NAME [args]    Run Codex as NAME without shell setup
  codex-accounts continue [--list|--json] List quota-interrupted conversations
  codex-accounts continue UUID|--all     Continue in separate tmux sessions
  codex-accounts [Codex arguments]        Run the bundled Codex CLI
  codex-accounts doctor [--json]          Check the bundled runtime (no network)
  codex-accounts shell-init zsh|bash      Print per-terminal shell integration

Add once to ~/.zshrc (or use bash for ~/.bashrc):
  eval "$(codex-accounts shell-init zsh)"

Then: codex account NAME; codex account NAME default; codex account default
      codex continue; codex resume --all
Credentials: ~/.codex-accounts/NAME. Shared conversations: ~/.codex.
No server synchronization or GUI. No Python/Node.js runtime required. Continue uses tmux.
`

const shellInit = `# codex-accounts: a separate account in each shell, with shared local history.
codex_account() {
  local _codex_accounts_env
  case "${1-}" in
    ""|--list|--help|-h) command codex-accounts account "$@" ;;
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
    *) command codex-accounts "$@" ;;
  esac
}
`

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
