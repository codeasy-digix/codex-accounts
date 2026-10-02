//go:build darwin || linux

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
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
	fmt.Fprintln(a.err, "  1. Device code (also works over SSH)")
	fmt.Fprintln(a.err, "  2. Browser sign-in (opens the local browser)")
	fmt.Fprint(a.err, "Login [1/device, 2/browser, q] (default: 1): ")
	if a.in == nil {
		return "device", nil
	}
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if err != nil && line == "" {
		return "device", nil // Preserve unattended device-code behavior at EOF.
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "1", "d", "device", "code":
		return "device", nil
	case "2", "b", "browser":
		return "browser", nil
	case "q", "quit", "cancel":
		return "", errors.New("login cancelled; your terminal account was not changed")
	default:
		return "", errors.New("choose 1/device or 2/browser, or use --browser/--device")
	}
}
