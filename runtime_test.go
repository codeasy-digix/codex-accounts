//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		if binary != expectedNative || !slices.Equal(args[1:], []string{"--no-daemon", "--version"}) {
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

func managedRuntimeFixture(t *testing.T, shared, release string) string {
	t.Helper()
	binary := filepath.Join(shared, "packages", "app-server-daemon", "releases", release+"-fixture-target", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(binary), 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = app-server ] && [ "$2" = daemon ] && [ "$3" = version ] && [ "$#" = 3 ]; then
  [ -z "$CODEX_ACCOUNT" ] || exit 91
  [ "$CODEX_HOME" = "$CODEX_SHARED_HOME" ] || exit 92
  [ "$CODEX_SQLITE_HOME" = "$CODEX_SHARED_HOME" ] || exit 93
  printf '%s\n' "$CODEX_ACCOUNTS_TEST_DAEMON_STATUS"
  exit "${CODEX_ACCOUNTS_TEST_DAEMON_EXIT:-0}"
fi
printf '%s\n' 'codex-cli 0.162.0 fixture'
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func selectManagedFixture(t *testing.T, shared, binary string) {
	t.Helper()
	current := filepath.Join(shared, "packages", "app-server-daemon", "current")
	if err := os.Symlink(filepath.Dir(filepath.Dir(binary)), current); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", "")
	t.Setenv("CODEX_ACCOUNTS_TEST_DAEMON_EXIT", "0")
}

func TestManagedRuntimeSharedAcrossDefaultAndNamedAccounts(t *testing.T) {
	root, shared, accounts := sandbox(t)
	binary := managedRuntimeFixture(t, shared, "0.162.0")
	selectManagedFixture(t, shared, binary)
	pathDir := filepath.Join(root, "npm")
	if err := os.MkdirAll(pathDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathDir, "codex"), []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	for _, name := range []string{"", "first", "second"} {
		t.Setenv("CODEX_ACCOUNT", name)
		t.Setenv("CODEX_HOME", "")
		if name != "" {
			t.Setenv("CODEX_HOME", seed(t, shared, accounts, name))
		}
		selected, args, env, err := nativeCommand([]string{"resume", "--last"})
		if err != nil || selected != binary {
			t.Fatalf("account %q selected a different runtime: %s %v", name, selected, err)
		}
		if !slices.Contains(args, "--no-daemon") {
			t.Fatalf("account daemon policy changed: %q %v", name, args)
		}
		if name != "" && (!slices.Contains(env, "CODEX_SQLITE_HOME="+shared) || !slices.Contains(args, "sqlite_home="+shellJSONString(shared))) {
			t.Fatalf("shared conversation storage changed: %v %v", args, env)
		}
	}
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", filepath.Join(pathDir, "codex"))
	if selected, err := selectRuntime(); err != nil || selected.source != "override" || selected.binary != filepath.Join(pathDir, "codex") {
		t.Fatalf("explicit override lost precedence: %+v %v", selected, err)
	}
}

func shellJSONString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestManagedRuntimeMatchesRunningReleaseWithoutRestart(t *testing.T) {
	for _, tc := range []struct {
		name, status, daemonExit string
		older, wantOlder, fail   bool
	}{
		{"current", `{"status":"running","managedCodexVersion":"0.163.0","appServerVersion":"0.163.0"}`, "0", false, false, false},
		{"busy previous release", `{"status":"running","managedCodexVersion":"0.163.0","appServerVersion":"0.162.0"}`, "0", true, true, false},
		{"stopped", `{"status":"stopped"}`, "0", false, false, false},
		{"previous package missing", `{"status":"running","managedCodexVersion":"0.163.0","appServerVersion":"0.162.0"}`, "0", false, false, true},
		{"unsafe release", `{"status":"running","managedCodexVersion":"0.163.0","appServerVersion":"../other"}`, "0", true, false, true},
		{"invalid response", `not-json`, "0", false, false, true},
		{"inspection failed", `SECRET-FIXTURE`, "7", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, shared, accounts := sandbox(t)
			current := managedRuntimeFixture(t, shared, "0.163.0")
			selectManagedFixture(t, shared, current)
			older := ""
			if tc.older {
				older = managedRuntimeFixture(t, shared, "0.162.0")
			}
			daemonDir := filepath.Join(shared, "app-server-daemon")
			if err := os.MkdirAll(daemonDir, 0700); err != nil {
				t.Fatal(err)
			}
			pidFile := filepath.Join(daemonDir, "daemon.pid")
			pidData := []byte(`{"pid":12345}`)
			if err := os.WriteFile(pidFile, pidData, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_ACCOUNT", "first")
			t.Setenv("CODEX_HOME", seed(t, shared, accounts, "first"))
			t.Setenv("CODEX_ACCOUNTS_TEST_DAEMON_STATUS", tc.status)
			t.Setenv("CODEX_ACCOUNTS_TEST_DAEMON_EXIT", tc.daemonExit)
			selected, err := selectRuntime()
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected runtime selection: %+v %v", selected, err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET-FIXTURE") {
				t.Fatal("version response leaked")
			}
			if !tc.fail {
				want := current
				if tc.wantOlder {
					want = older
				}
				if selected.binary != want || selected.source != "daemon" {
					t.Fatalf("running server and CLI diverged: %+v want %s", selected, want)
				}
			}
			if data, err := os.ReadFile(pidFile); err != nil || !bytes.Equal(data, pidData) {
				t.Fatal("runtime selection changed daemon state")
			}
			if target, err := os.Readlink(filepath.Join(shared, "packages", "app-server-daemon", "current")); err != nil || target != filepath.Dir(filepath.Dir(current)) {
				t.Fatal("runtime selection changed the installed package")
			}
		})
	}
}

func TestManagedRuntimeUpdatesSharedPackageAndReportsSource(t *testing.T) {
	_, shared, accounts := sandbox(t)
	binary := managedRuntimeFixture(t, shared, "0.162.0")
	selectManagedFixture(t, shared, binary)
	for _, name := range []string{"", "first"} {
		t.Setenv("CODEX_ACCOUNT", name)
		t.Setenv("CODEX_HOME", "")
		if name != "" {
			t.Setenv("CODEX_HOME", seed(t, shared, accounts, name))
		}
		for _, args := range [][]string{{"update"}, {"-c", `model="update"`, "update", "--help"}} {
			_, actual, env, err := nativeCommand(args)
			index := upstreamCommandIndex(args)
			want := append([]string{}, args[:index]...)
			want = append(want, "app-server", "daemon", "update")
			want = append(want, args[index+1:]...)
			if err != nil || !slices.Equal(actual[1:], want) || !slices.Contains(env, "CODEX_HOME="+shared) || !slices.Contains(env, "CODEX_ACCOUNT=") {
				t.Fatalf("update would target a different installation: %v %v %v", actual, env, err)
			}
		}
		_, args, _, err := nativeCommand([]string{"exec", "--", "update"})
		if err != nil || slices.Contains(args, "app-server") {
			t.Fatalf("prompt was interpreted as an updater: %v %v", args, err)
		}
	}
	var out bytes.Buffer
	a := application{&out, &out, strings.NewReader(""), context.Background()}
	if err := a.doctor([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["runtimeSource"] != "daemon" || result["runtime"] != binary {
		t.Fatalf("doctor did not report the common package: %s %v", out.String(), err)
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
			for _, arguments := range []string{`"args":["--no-daemon","--version"]`, `"args":["--no-daemon","--help"]`, `"args":["update","--help"]`, `"args":["--no-daemon","doctor","--json"]`, `"args":["--no-daemon","future-upstream-command","--flag"]`} {
				if !strings.Contains(string(calls), arguments) {
					t.Fatalf("native arguments changed: %s\n%s", arguments, calls)
				}
			}
		})
	}
}

func TestDefaultAndNamedAccountsDisableSharedDaemon(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "fixture")
	for _, name := range []string{"", "fixture"} {
		t.Setenv("CODEX_ACCOUNT", name)
		t.Setenv("CODEX_HOME", "")
		if name != "" {
			t.Setenv("CODEX_HOME", home)
		}
		for _, input := range [][]string{nil, {"resume", "--last"}, {"exec", "--", "--no-daemon"}} {
			_, args, _, err := nativeCommand(input)
			if err != nil || len(args) < 2 || args[1] != "--no-daemon" || !slices.Equal(args[len(args)-len(input):], input) {
				t.Fatalf("account %q could use the shared daemon or alter the prompt: %v %v", name, args, err)
			}
		}
		_, args, _, err := nativeCommand([]string{"--no-daemon", "resume", "--help"})
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, argument := range args[1:] {
			if argument == "--no-daemon" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("explicit native flag duplicated for account %q: %v", name, args)
		}
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
