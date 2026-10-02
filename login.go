//go:build darwin || linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
		case "--set-default", "default":
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

const accountUsage = "usage: codex account NAME [--set-default] [--login] [--browser|--device]"

func (a application) chooseLoginMethod(method string) (string, error) {
	if method != "" {
		return method, nil
	}
	fmt.Fprintln(a.err, "Login required. Choose authentication method:")
	method, err := a.chooseAuthentication()
	if err == nil && method == "" {
		err = errors.New("login cancelled; your terminal account was not changed")
	}
	return method, err
}

func (a application) chooseAuthentication() (string, error) {
	fmt.Fprintln(a.err, "  1. Device code (also works over SSH)")
	fmt.Fprintln(a.err, "  2. Browser sign-in (opens the local browser)")
	fmt.Fprintln(a.err, "  0. Cancel")
	if a.in == nil {
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
