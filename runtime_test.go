//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledRuntimeSkipsRecursiveShimsAndFollowsUpdates(t *testing.T) {
	root, _, _ := sandbox(t)
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", "")
	shimDir, nativeDir := filepath.Join(root, "shim"), filepath.Join(root, "official")
	for _, directory := range []string{shimDir, nativeDir} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	shim := filepath.Join(shimDir, "codex")
	if err := os.Symlink(testCLI, shim); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(nativeDir, "codex")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(native, []byte("#!/bin/sh\nprintf '%s\\n' '"+text+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("native before update")
	expectedNative, err := filepath.EvalSymlinks(native)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+nativeDir)
	for _, version := range []string{"native before update", "native after update"} {
		write(version)
		binary, args, env, err := nativeCommand([]string{"--version"})
		if err != nil {
			t.Fatal(err)
		}
		if binary != expectedNative || len(args) != 2 || args[1] != "--version" {
			t.Fatalf("native command changed: %s %v", binary, args)
		}
		cmd := exec.Command(binary, args[1:]...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil || strings.TrimSpace(string(output)) != version {
			t.Fatalf("updated runtime not followed: %v %s", err, output)
		}
	}
	if err := os.Remove(shim); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec codex-accounts __native \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if binary, err := runtimeBinary(); err != nil || binary != expectedNative {
		t.Fatalf("recursive shell shim selected: %s %v", binary, err)
	}
	fixture, _ := os.Executable()
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", fixture)
	if binary, err := runtimeBinary(); err != nil || binary != fixture {
		t.Fatalf("explicit runtime override ignored: %s %v", binary, err)
	}
}

func TestShellPassesUpstreamHelpVersionUpdateAndFutureCommands(t *testing.T) {
	for _, shell := range []string{"zsh", "bash"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skip("shell unavailable")
			}
			root, _, _ := sandbox(t)
			t.Setenv("PATH", filepath.Dir(testCLI)+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd := exec.Command(shell, "-c", `eval "$(codex-accounts shell-init `+shell+`)"
codex --version || exit 11
codex --help || exit 12
codex update --help || exit 13
codex doctor --json || exit 14
codex future-upstream-command --flag || exit 15
`)
			output, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(output), "codex-cli fixture") {
				t.Fatalf("shell intercepted upstream command: %v %s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, arguments := range []string{`"args":["--version"]`, `"args":["--help"]`, `"args":["update","--help"]`, `"args":["doctor","--json"]`, `"args":["future-upstream-command","--flag"]`} {
				if !strings.Contains(string(calls), arguments) {
					t.Fatalf("native arguments changed: %s\n%s", arguments, calls)
				}
			}
		})
	}
}

func TestOnlyNamedAccountDisablesSharedDaemon(t *testing.T) {
	_, shared, accounts := sandbox(t)
	_, args, _, err := nativeCommand([]string{"resume", "--last"})
	if err != nil || strings.Contains(strings.Join(args, " "), "--no-daemon") {
		t.Fatalf("default daemon behavior changed: %v %v", args, err)
	}
	home := seed(t, shared, accounts, "fixture")
	t.Setenv("CODEX_ACCOUNT", "fixture")
	t.Setenv("CODEX_HOME", home)
	_, args, _, err = nativeCommand([]string{"resume", "--last"})
	if err != nil || len(args) < 2 || args[1] != "--no-daemon" {
		t.Fatalf("named account could use another account's daemon: %v %v", args, err)
	}
	_, args, _, err = nativeCommand([]string{"--no-daemon", "resume", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, argument := range args[1:] {
		if argument == "--no-daemon" {
			count++
		}
	}
	if err != nil || count != 1 {
		t.Fatalf("explicit native flag duplicated: %v %v", args, err)
	}
	_, args, _, err = nativeCommand([]string{"exec", "--", "--no-daemon"})
	if err != nil || len(args) < 2 || args[1] != "--no-daemon" {
		t.Fatalf("prompt text disabled account isolation: %v %v", args, err)
	}
}

func TestUpdateUsesInstallationHomeWithoutChangingSelectedShell(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "fixture")
	t.Setenv("CODEX_ACCOUNT", "fixture")
	t.Setenv("CODEX_HOME", home)
	for _, command := range [][]string{{"update"}, {"-c", "model=fixture", "update"}} {
		_, args, env, err := nativeCommand(command)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(args[1:], " ") != strings.Join(command, " ") {
			t.Fatalf("updater arguments modified: %v", args)
		}
		hasSharedHome, hasNoAccount := false, false
		for _, item := range env {
			hasSharedHome = hasSharedHome || item == "CODEX_HOME="+shared
			hasNoAccount = hasNoAccount || item == "CODEX_ACCOUNT="
		}
		if !hasSharedHome || !hasNoAccount {
			t.Fatal("updater did not use the shared installation home")
		}
		if os.Getenv("CODEX_HOME") != home || os.Getenv("CODEX_ACCOUNT") != "fixture" {
			t.Fatal("update changed selected shell")
		}
	}
}
