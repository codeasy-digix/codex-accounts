//go:build darwin || linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type loginOptions struct {
	force       bool
	method      string
	makeDefault bool
}

func parseLoginOptions(args []string) (loginOptions, error) {
	var options loginOptions
	for _, arg := range args {
		switch arg {
		case "--login":
			options.force = true
		case "default":
			if options.makeDefault {
				return options, errors.New(accountUsage)
			}
			options.makeDefault = true
		case "--browser", "--device", "--device-auth":
			if options.method != "" {
				return options, errors.New("choose one login method: --browser or --device")
			}
			options.force = true
			options.method = "device"
			if arg == "--browser" {
				options.method = "browser"
			}
		default:
			return options, errors.New(accountUsage)
		}
	}
	return options, nil
}

const accountUsage = "usage: codex account NAME [default] [--login] [--browser|--device]"

func (a application) chooseLoginMethod(method string) (string, error) {
	if method != "" {
		return method, nil
	}
	fmt.Fprintln(a.err, "Login required. Choose authentication method:")
	method, err := a.chooseAuthentication(false)
	if err == nil && method == "" {
		err = errors.New("login cancelled; your terminal account was not changed")
	}
	return method, err
}

func (a application) chooseAuthentication(optional bool) (string, error) {
	fmt.Fprintln(a.err, "  1. Device code (also works over SSH)")
	fmt.Fprintln(a.err, "  2. Browser sign-in (opens the local browser)")
	fmt.Fprintln(a.err, "  0. Keep current login / cancel")
	if a.in == nil {
		if optional {
			return "", nil
		}
		return "device", nil
	}
	reader := bufio.NewReader(a.in)
	for {
		fmt.Fprint(a.err, "Select a number [1/2/0]: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "":
			if optional {
				return "", nil
			}
			return "device", nil // Preserve device-code defaults for required login.
		case "1", "d", "device", "code":
			return "device", nil
		case "2", "b", "browser":
			return "browser", nil
		case "0", "q", "quit", "cancel":
			return "", nil
		default:
			if errors.Is(err, io.EOF) {
				return "", errors.New("choose 1/device, 2/browser, or 0 to cancel")
			}
			fmt.Fprintln(a.err, "Invalid selection. Enter 1, 2, or 0.")
		}
	}
}

func (a application) loginArguments(method string) []string {
	if method == "device" {
		fmt.Fprintln(a.err, "Sign in using the device code shown below.")
		return []string{"login", "--device-auth"}
	}
	fmt.Fprintln(a.err, "Opening browser sign-in…")
	return []string{"login"}
}

func (a application) runLogin(binary string, args, env []string, home string) error {
	cmd := exec.CommandContext(a.ctx, binary, args...)
	cmd.Env, cmd.Dir = env, home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, a.err, a.err
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Run(); err != nil {
		return errors.New("login did not complete; your terminal account was not changed")
	}
	return nil
}

func (a application) accountMenu(shared, accounts string, out io.Writer) error {
	if err := a.showStatus(out, shared); err != nil {
		return err
	}
	fmt.Fprintln(a.err, "\nRe-authenticate the current account:")
	method, err := a.chooseAuthentication(true)
	if err != nil || method == "" {
		return err
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		home = shared
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return err
	}
	options := loginOptions{force: true, method: method}
	if name := os.Getenv("CODEX_ACCOUNT"); name != "" {
		expected, err := prepareHome(shared, accounts, name)
		if err != nil {
			return err
		}
		if expected != home {
			return errors.New("CODEX_HOME does not match the selected account")
		}
		info, err := a.selectAccountWithLogin(home, shared, options, nil)
		if err != nil {
			return err
		}
		printStatus(out, name, home, shared, info)
		return nil
	}
	if err := realDirectory(home, true); err != nil {
		return err
	}
	if home == shared {
		lock, err := machineDefaultLock(shared)
		if err != nil {
			return err
		}
		defer lock.Close()
		name, err := defaultAccount(shared, accounts)
		if err != nil {
			return err
		}
		if name != "" {
			// Re-login the one credential target; the default link is unchanged.
			credentialHome, err := prepareHome(shared, accounts, name)
			if err != nil {
				return err
			}
			info, err := a.selectAccountWithLogin(credentialHome, shared, options, nil)
			if err != nil {
				return err
			}
			printStatus(out, "", home, shared, info)
			return nil
		}
	}
	if err := privateAuth(home); err != nil {
		return err
	}
	lock, err := loginLock(home)
	if err != nil {
		return err
	}
	defer lock.Close()
	binary, args, env, err := nativeCommand(a.loginArguments(method))
	if err != nil {
		return err
	}
	if err := a.runLogin(binary, args[1:], setEnvironment(env, "CODEX_HOME", home), home); err != nil {
		return err
	}
	return a.showStatus(out, shared)
}
