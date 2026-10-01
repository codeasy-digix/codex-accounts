//go:build darwin || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testCLI string

func TestMain(m *testing.M) {
	if os.Getenv("CODEX_ACCOUNTS_TEST_PROCESS") == "1" {
		fixtureRuntime()
		os.Exit(0)
	}
	// A cross-compiled test executable can exercise a release on a machine with
	// no Go installation, using the already-built CLI rather than compiling it.
	if testCLI = os.Getenv("CODEX_ACCOUNTS_TEST_CLI"); testCLI != "" {
		os.Exit(m.Run())
	}
	root, err := os.MkdirTemp("", "codex-accounts-test-build-")
	if err != nil {
		panic(err)
	}
	testCLI = filepath.Join(root, "codex-accounts")
	cmd := exec.Command("go", "build", "-o", testCLI, ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(output), err)
		os.RemoveAll(root)
		os.Exit(1)
	}
	result := m.Run()
	os.RemoveAll(root)
	os.Exit(result)
}

// This child is a deterministic native-runtime fixture, not a live account.
func fixtureRuntime() {
	if record := os.Getenv("CODEX_ACCOUNTS_TEST_LOG"); record != "" {
		f, _ := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if f != nil {
			json.NewEncoder(f).Encode(map[string]any{
				"args": os.Args[1:], "home": os.Getenv("CODEX_HOME"), "sqlite": os.Getenv("CODEX_SQLITE_HOME"), "pid": os.Getpid(),
				"apiOverridePresent": os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "" || os.Getenv("CODEX_ACCESS_TOKEN") != "",
			})
			f.Close()
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("codex-cli fixture")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "login" {
		if os.Getenv("CODEX_ACCOUNTS_TEST_MODE") == "cancel" {
			os.Exit(7)
		}
		os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"tokens":{"refresh_token":"logged-in"}}`), 0600)
		fmt.Println("fixture device login completed")
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "app-server" {
		if os.Getenv("CODEX_ACCOUNTS_TEST_MODE") == "exit7" {
			os.Exit(7)
		}
		fmt.Println("fixture native command")
		return
	}
	if os.Getenv("CODEX_ACCOUNTS_TEST_MODE") == "hang" {
		time.Sleep(30 * time.Second)
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(scanner.Bytes(), &req)
		if req.ID == nil {
			continue
		}
		if record := os.Getenv("CODEX_ACCOUNTS_TEST_LOG"); record != "" {
			f, _ := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if f != nil {
				json.NewEncoder(f).Encode(map[string]string{"method": req.Method})
				f.Close()
			}
		}
		mode := os.Getenv("CODEX_ACCOUNTS_TEST_MODE")
		auth, _ := os.ReadFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"))
		if strings.Contains(string(auth), "logged-in") && mode == "expired" {
			mode = ""
		}
		if req.Method == "account/read" && (mode == "expired" || mode == "network") {
			msg := "refresh_token_expired SECRET-FIXTURE"
			if mode == "network" {
				msg = "503 connection failure SECRET-FIXTURE"
			}
			json.NewEncoder(os.Stdout).Encode(map[string]any{"id": *req.ID, "error": map[string]any{"message": msg}})
			continue
		}
		var result any = map[string]any{}
		switch req.Method {
		case "account/read":
			if len(auth) == 0 {
				result = map[string]any{"account": nil}
			} else {
				result = map[string]any{"account": map[string]any{"type": "chatgpt", "email": filepath.Base(os.Getenv("CODEX_HOME")) + "@example.test", "planType": "plus"}}
			}
		case "account/rateLimits/read":
			result = map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": 25, "windowDurationMins": 300}, "secondary": map[string]any{"usedPercent": 12, "windowDurationMins": 10080}}}
		}
		// Interleave unrelated notifications and malformed lines, as a real stream may.
		fmt.Println(`{"method":"account/updated","params":{}}`)
		fmt.Println("not-json")
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": *req.ID, "result": result})
	}
}

func sandbox(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	shared, accounts := filepath.Join(root, ".codex"), filepath.Join(root, ".codex-accounts")
	t.Setenv("HOME", root)
	t.Setenv("CODEX_SHARED_HOME", shared)
	t.Setenv("CODEX_ACCOUNTS_DIR", accounts)
	t.Setenv("CODEX_ACCOUNT", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CODEX_SQLITE_HOME", "")
	t.Setenv("CODEX_ACCOUNTS_TEST_PROCESS", "1")
	t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "")
	t.Setenv("CODEX_ACCOUNTS_TEST_LOG", filepath.Join(root, "calls.jsonl"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_ACCOUNTS_RUNTIME", executable)
	return root, shared, accounts
}

func seed(t *testing.T, shared, accounts, name string) string {
	t.Helper()
	home, err := prepareHome(shared, accounts, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"tokens":{"refresh_token":"fixture-only"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func testApp() (application, *bytes.Buffer, *bytes.Buffer) {
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return application{out, stderr, strings.NewReader(""), context.Background()}, out, stderr
}

func TestSharedHistorySeparateCredentials(t *testing.T) {
	_, shared, accounts := sandbox(t)
	one := seed(t, shared, accounts, "ryu")
	two := seed(t, shared, accounts, "kakadais")
	for _, home := range []string{one, two} {
		for _, name := range append(append([]string{}, sharedDirectories...), sharedFiles...) {
			target, err := os.Readlink(filepath.Join(home, name))
			if err != nil || target != filepath.Join(shared, name) {
				t.Fatalf("wrong link %s: %q %v", name, target, err)
			}
		}
		for _, name := range []string{"auth.json", "state_5.sqlite", "state_5.sqlite-wal"} {
			info, err := os.Lstat(filepath.Join(home, name))
			if err == nil && info.Mode()&os.ModeSymlink != 0 {
				t.Fatalf("private/native SQLite file linked: %s", name)
			}
		}
	}
	os.WriteFile(filepath.Join(one, "history.jsonl"), []byte("one conversation\n"), 0600)
	data, _ := os.ReadFile(filepath.Join(two, "history.jsonl"))
	if string(data) != "one conversation\n" {
		t.Fatal("history is not shared")
	}
	info, _ := os.Stat(filepath.Join(one, "auth.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	info, _ = os.Stat(one)
	if info.Mode().Perm() != 0700 {
		t.Fatal("home permissions")
	}
}

func TestRejectUnsafeHomes(t *testing.T) {
	for _, scenario := range []string{"bad-name", "root-symlink", "auth-symlink", "collision", "nested", "accounts-ancestor"} {
		t.Run(scenario, func(t *testing.T) {
			root, shared, accounts := sandbox(t)
			name := "ryu"
			os.MkdirAll(shared, 0700)
			os.MkdirAll(accounts, 0700)
			home := filepath.Join(accounts, name)
			os.Mkdir(home, 0700)
			switch scenario {
			case "bad-name":
				name = "../../escape"
			case "root-symlink":
				os.Remove(home)
				os.Symlink(shared, home)
			case "auth-symlink":
				os.Symlink(filepath.Join(shared, "auth.json"), filepath.Join(home, "auth.json"))
			case "collision":
				os.WriteFile(filepath.Join(home, "config.toml"), []byte("keep"), 0600)
			case "nested":
				accounts = filepath.Join(shared, "private")
			case "accounts-ancestor":
				accounts = root
			}
			if _, err := prepareHome(shared, accounts, name); err == nil {
				t.Fatal("unsafe home accepted")
			}
			if scenario == "collision" {
				data, _ := os.ReadFile(filepath.Join(home, "config.toml"))
				if string(data) != "keep" {
					t.Fatal("existing config overwritten")
				}
			}
		})
	}
}

func TestExistingAccountLayoutCompatible(t *testing.T) {
	_, shared, accounts := sandbox(t)
	seed(t, shared, accounts, "ryu")
	if _, err := prepareHome(shared, accounts, "ryu"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHome(shared, accounts, "default"); err == nil {
		t.Fatal("reserved name allowed")
	}
}

func TestAccountSelectionIncludesInfoAndShellExports(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	a, out, stderr := testApp()
	if err := a.account([]string{"--shell", "ryu"}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Email: ryu@example.test", "Plan: plus", "5h: 75% remaining", "week: 88% remaining"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("missing %s in %s", expected, stderr.String())
		}
	}
	if !strings.Contains(out.String(), "export CODEX_HOME="+shellQuote(home)) || strings.Contains(out.String(), "Email") {
		t.Fatal("shell output polluted")
	}
}

func TestLoginRecoveryAndCancellation(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "force", "cancel", "network"} {
		t.Run(mode, func(t *testing.T) {
			root, shared, accounts := sandbox(t)
			if mode != "missing" {
				seed(t, shared, accounts, "ryu")
			}
			if mode != "missing" && mode != "force" {
				t.Setenv("CODEX_ACCOUNTS_TEST_MODE", mode)
			}
			a, out, stderr := testApp()
			args := []string{"--shell", "ryu"}
			if mode == "force" || mode == "cancel" {
				args = append(args, "--login")
			}
			err := a.account(args)
			if mode == "network" || mode == "cancel" {
				if err == nil || out.Len() != 0 {
					t.Fatal("failed selection emitted shell changes")
				}
				if strings.Contains(fmt.Sprint(err)+stderr.String(), "SECRET-FIXTURE") {
					t.Fatal("RPC body leaked")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			loggedIn := strings.Contains(string(calls), `"login"`)
			if loggedIn == (mode == "network") {
				t.Fatalf("wrong login decision in %s: %s", mode, calls)
			}
		})
	}
}

func TestDefaultClearsTerminalSelectionAndShowsInfo(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	os.WriteFile(filepath.Join(shared, "auth.json"), []byte(`{"tokens":{"refresh_token":"fixture-only"}}`), 0600)
	t.Setenv("CODEX_ACCOUNT", "ryu")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SQLITE_HOME", shared)
	a, out, stderr := testApp()
	if err := a.account([]string{"--shell", "default"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "unset CODEX_ACCOUNT CODEX_HOME CODEX_SQLITE_HOME" {
		t.Fatal(out.String())
	}
	if os.Getenv("CODEX_ACCOUNT") != "" || !strings.Contains(stderr.String(), "Environment: default") || !strings.Contains(stderr.String(), "Email: .codex@example.test") {
		t.Fatal(stderr.String())
	}
}

func TestAccountLockPreventsConcurrentRefresh(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	lock, err := loginLock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	a, out, _ := testApp()
	if err := a.account([]string{"--shell", "ryu"}); err == nil || out.Len() != 0 {
		t.Fatal("concurrent refresh allowed")
	}
}

func TestNamedAccountStripsAPIOverrides(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	env := []string{"OPENAI_API_KEY=fixture", "CODEX_API_KEY=fixture", "CODEX_ACCESS_TOKEN=fixture", "PATH=/bin", "KEEP=yes"}
	for _, isolated := range []bool{true, false} {
		result := strings.Join(accountEnvironment(env, home, shared, isolated), "\n")
		if strings.Contains(result, "OPENAI_API_KEY=") == isolated {
			t.Fatal("wrong API override handling")
		}
		if !strings.Contains(result, "CODEX_SQLITE_HOME="+shared) || !strings.Contains(result, "KEEP=yes") {
			t.Fatal("lost environment")
		}
	}
}

func TestLimitsUseRealDurationsAndMultiBuckets(t *testing.T) {
	var reply limitsReply
	data := `{"rateLimits":{"primary":{"usedPercent":99,"windowDurationMins":300}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":-4,"windowDurationMins":60},"secondary":{"usedPercent":110,"windowDurationMins":10080}},"other":{"primary":{"usedPercent":20,"windowDurationMins":300}}}}`
	if err := json.Unmarshal([]byte(data), &reply); err != nil {
		t.Fatal(err)
	}
	out := new(bytes.Buffer)
	printLimits(out, reply)
	for _, expected := range []string{"codex 60min: 100% remaining", "codex week: 0% remaining", "other 5h: 80% remaining"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatal(out.String())
		}
	}
	out.Reset()
	printLimits(out, limitsReply{})
	if !strings.Contains(out.String(), "no information") {
		t.Fatal("unknown usage reported as zero")
	}
}

func TestWorkspaceUsesSelectedOrganizationOnly(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	claims := `{"https://api.openai.com/auth":{"organizations":[{"id":"other","title":"Wrong"},{"id":"selected","title":"Team"}]}}`
	jwt := "x." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".x"
	data, _ := json.Marshal(map[string]any{"tokens": map[string]string{"id_token": jwt, "account_id": "selected"}})
	os.WriteFile(filepath.Join(home, "auth.json"), data, 0600)
	if workspaceName(home) != "Team" {
		t.Fatal("wrong organization")
	}
	if strings.Contains(cleanText("a\x1b\nb"), "\x1b") {
		t.Fatal("terminal control exposed")
	}
}

func TestRPCDeadlineTerminatesChild(t *testing.T) {
	_, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := newAppServer(ctx, os.Getenv("CODEX_ACCOUNTS_RUNTIME"), home, shared, true)
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatal("child deadline/cleanup failed")
	}
}

func TestShellIntegrationBashAndZsh(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skip("shell not installed")
			}
			root, shared, accounts := sandbox(t)
			seed(t, shared, accounts, "ryu")
			seed(t, shared, accounts, "other")
			os.WriteFile(filepath.Join(shared, "auth.json"), []byte(`{"tokens":{"refresh_token":"fixture-only"}}`), 0600)
			t.Setenv("PATH", filepath.Dir(testCLI)+":"+os.Getenv("PATH"))
			script := `eval "$(codex-accounts shell-init ` + shell + `)"
codex account ryu || exit 11
test "$CODEX_ACCOUNT" = ryu || exit 12
(codex account other >/dev/null) || exit 13
test "$CODEX_ACCOUNT" = ryu || exit 14
codex account >/dev/null || exit 15
codex account default || exit 16
test -z "${CODEX_ACCOUNT-}" && test -z "${CODEX_HOME-}" || exit 17
codex run --help
`
			cmd := exec.Command(shell, "-c", script)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v\n%s", shell, err, output)
			}
			calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			if !strings.Contains(string(calls), `"--no-daemon"`) {
				t.Fatal("native CLI not dispatched")
			}
			// Failure must leave the already-selected terminal unchanged.
			t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "network")
			cmd = exec.Command(shell, "-c", `eval "$(codex-accounts shell-init `+shell+`)"; export CODEX_ACCOUNT=before CODEX_HOME=/before; codex account ryu >/dev/null; test "$CODEX_ACCOUNT" = before && test "$CODEX_HOME" = /before`)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("failed selection changed shell: %v %s", err, output)
			}
		})
	}
}

func TestNativeExitCodeAndJSONOutputPreserved(t *testing.T) {
	root, shared, accounts := sandbox(t)
	home := seed(t, shared, accounts, "ryu")
	t.Setenv("CODEX_ACCOUNT", "ryu")
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "exit7")
	cmd := exec.Command(testCLI, "exec", "--json", "hello")
	output, err := cmd.CombinedOutput()
	if !strings.Contains(fmt.Sprint(err), "exit status 7") || len(output) != 0 {
		t.Fatalf("exit code/output altered: %v %s", err, output)
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if !strings.Contains(string(calls), `"exec","--json","hello"`) || strings.Contains(string(calls), `"auth.json"`) {
		t.Fatal(string(calls))
	}
}

func TestNoModelRPCsDuringSelection(t *testing.T) {
	root, shared, accounts := sandbox(t)
	seed(t, shared, accounts, "ryu")
	a, _, _ := testApp()
	if err := a.account([]string{"ryu"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	methods := 0
	for scanner.Scan() {
		var record struct {
			Method string `json:"method"`
		}
		json.Unmarshal(scanner.Bytes(), &record)
		if record.Method == "" {
			continue
		}
		methods++
		if record.Method != "initialize" && record.Method != "account/read" && record.Method != "account/rateLimits/read" {
			t.Fatalf("unexpected RPC: %s", record.Method)
		}
	}
	if methods != 3 {
		t.Fatalf("account RPCs were not exercised: %d", methods)
	}
}

func TestRealBundledRuntimeUnauthenticated(t *testing.T) {
	binary := os.Getenv("CODEX_ACCOUNTS_INTEGRATION_RUNTIME")
	if binary == "" {
		t.Skip("set CODEX_ACCOUNTS_INTEGRATION_RUNTIME for native integration")
	}
	_, shared, accounts := sandbox(t)
	home, err := prepareHome(shared, accounts, "new")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := newAppServer(ctx, binary, home, shared, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	var reply accountReply
	if err := s.request(ctx, "account/read", map[string]bool{"refreshToken": false}, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Account != nil {
		t.Fatal("isolated home inherited another account")
	}
	var threads map[string]any
	if err := s.request(ctx, "thread/list", map[string]any{"limit": 1}, &threads); err != nil {
		t.Fatal(err)
	}
	// A synthetic stored conversation exercises native history loading without
	// a login, model turn, or copying any personal conversation into the test.
	s.close()
	id := "f223f119-871b-4e7c-9b6d-214cd8e8ea23"
	stamp := "2026-10-01T00:00:00.000Z"
	dir := filepath.Join(shared, "sessions", "2026", "10", "01")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "rollout-2026-10-01T00-00-00-"+id+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []map[string]any{
		{"timestamp": stamp, "type": "session_meta", "payload": map[string]any{
			"id": id, "timestamp": stamp, "cwd": shared, "originator": "codex_cli_rs", "cli_version": codexVersion, "source": "cli", "model_provider": "openai",
		}},
		{"timestamp": stamp, "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "synthetic shared history fixture"}},
		}},
		{"timestamp": stamp, "type": "event_msg", "payload": map[string]any{
			"type": "user_message", "message": "synthetic shared history fixture", "images": []string{}, "local_images": []string{}, "text_elements": []any{},
		}},
	} {
		if err := json.NewEncoder(f).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	for _, name := range []string{"new", "second"} {
		account, err := prepareHome(shared, accounts, name)
		if err != nil {
			t.Fatal(err)
		}
		native, err := newAppServer(ctx, binary, account, shared, true)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Thread struct {
				ID      string `json:"id"`
				Preview string `json:"preview"`
			} `json:"thread"`
		}
		err = native.request(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": true}, &result)
		native.close()
		if err != nil {
			t.Fatalf("native history read as %s: %v", name, err)
		}
		if result.Thread.ID != id || !strings.Contains(result.Thread.Preview, "synthetic shared history") {
			t.Fatalf("wrong shared history as %s: %+v", name, result)
		}
	}
}
