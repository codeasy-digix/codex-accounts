//go:build darwin || linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginMethodChoiceAndExplicitFlags(t *testing.T) {
	cases := []struct {
		name, input    string
		args           []string
		device, cancel bool
	}{
		{"choose-browser", "2\n", nil, false, false},
		{"choose-device", "1\n", nil, true, false},
		{"retry-browser", "99\n2\n", nil, false, false},
		{"default-device", "", nil, true, false},
		{"explicit-browser", "q\n", []string{"--browser"}, false, false},
		{"explicit-device", "q\n", []string{"--device"}, true, false},
		{"cancel", "q\n", nil, false, true},
		{"cancel-number", "0\n", nil, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := sandbox(t)
			a, out, stderr := testApp()
			a.in = strings.NewReader(tc.input)
			args := append([]string{"--shell", "fixture"}, tc.args...)
			err := a.account(args)
			calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
			if tc.cancel {
				if err == nil || len(calls) != 0 || out.Len() != 0 {
					t.Fatal("cancel started login or emitted environment changes", err)
				}
				return
			}
			if err != nil || !strings.Contains(out.String(), "export CODEX_ACCOUNT='fixture'") || strings.Contains(out.String(), "Login required") {
				t.Fatal("login failed or prompt polluted shell environment", err, out.String(), stderr.String())
			}
			found := false
			for _, line := range strings.Split(string(calls), "\n") {
				var call struct {
					Args []string `json:"args"`
				}
				json.Unmarshal([]byte(line), &call)
				if len(call.Args) > 0 && call.Args[0] == "login" {
					found = true
					device := false
					for _, arg := range call.Args {
						if arg == "--device-auth" {
							device = true
						}
					}
					if device != tc.device {
						t.Fatalf("wrong login command: %v", call.Args)
					}
				}
			}
			if !found {
				t.Fatal("login command was not invoked")
			}
		})
	}
}

func fixtureLoginCalls(t *testing.T, root string) []struct {
	Args []string
	Home string
} {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	var calls []struct {
		Args []string
		Home string
	}
	for _, line := range strings.Split(string(data), "\n") {
		var call struct {
			Args []string
			Home string
		}
		if json.Unmarshal([]byte(line), &call) != nil {
			continue
		}
		for _, arg := range call.Args {
			if arg == "login" {
				calls = append(calls, call)
				break
			}
		}
	}
	return calls
}

func TestAccountWithoutArgsOnlyShowsStatus(t *testing.T) {
	for _, store := range []string{"default", "named", "managed-default", "signed-out", "invalid"} {
		t.Run(store, func(t *testing.T) {
			root, shared, accounts := sandbox(t)
			home := seed(t, shared, accounts, "chosen")
			credentialHome, expectedName := shared, ""
			expectedHome, expectedEmail := "", ".codex@example.test"
			switch store {
			case "named":
				credentialHome, expectedName = home, "chosen"
				expectedHome, expectedEmail = home, "chosen@example.test"
				t.Setenv("CODEX_ACCOUNT", expectedName)
				t.Setenv("CODEX_HOME", expectedHome)
			case "managed-default":
				a, _, _ := testApp()
				if _, err := a.publishDefault(shared, accounts, home); err != nil {
					t.Fatal(err)
				}
				credentialHome = home
			case "signed-out":
				expectedEmail = ""
			case "invalid":
				if err := os.WriteFile(filepath.Join(shared, "auth.json"), []byte("invalid-fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_ACCOUNTS_TEST_MODE", "expired")
				expectedEmail = ""
			default:
				if err := os.WriteFile(filepath.Join(shared, "auth.json"), []byte("default-fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.ReadFile(filepath.Join(credentialHome, "auth.json"))
			previousLink, _ := os.Readlink(filepath.Join(shared, "auth.json"))
			a, out, stderr := testApp()
			input := strings.NewReader("2\n")
			a.in = input
			if err := a.account(nil); err != nil {
				t.Fatal("status lookup failed", err, stderr.String())
			}
			label := expectedName
			if label == "" {
				label = "default"
			}
			if !strings.Contains(out.String(), "Environment: "+label) || (expectedEmail != "" && !strings.Contains(out.String(), "Email: "+expectedEmail)) {
				t.Fatal("current account details missing", out.String())
			}
			if store == "signed-out" && !strings.Contains(out.String(), "Login: not signed in") {
				t.Fatal("signed-out state missing", out.String())
			}
			if store == "invalid" && !strings.Contains(out.String(), "lookup unavailable") {
				t.Fatal("expired authentication state missing", out.String())
			}
			if stderr.Len() != 0 || strings.Contains(out.String(), "Select a number") || input.Len() != len("2\n") || len(fixtureLoginCalls(t, root)) != 0 {
				t.Fatal("information command prompted or started authentication", out.String(), stderr.String())
			}
			after, afterErr := os.ReadFile(filepath.Join(credentialHome, "auth.json"))
			currentLink, _ := os.Readlink(filepath.Join(shared, "auth.json"))
			if string(before) != string(after) || os.IsNotExist(beforeErr) != os.IsNotExist(afterErr) || previousLink != currentLink || os.Getenv("CODEX_ACCOUNT") != expectedName || os.Getenv("CODEX_HOME") != expectedHome {
				t.Fatal("information command changed credentials or selected environment")
			}
		})
	}
}

func TestValidLoginDoesNotPromptAndConflictingMethodsDoNotLogin(t *testing.T) {
	root, shared, accounts := sandbox(t)
	seed(t, shared, accounts, "fixture")
	a, out, stderr := testApp()
	a.in = strings.NewReader("q\n")
	if err := a.account([]string{"--shell", "fixture"}); err != nil || strings.Contains(stderr.String(), "Choose authentication") {
		t.Fatal("valid login asked to sign in again", err)
	}
	calls, _ := os.ReadFile(filepath.Join(root, "calls.jsonl"))
	if strings.Contains(string(calls), `"login"`) {
		t.Fatal("valid login reauthenticated")
	}
	a, out, _ = testApp()
	if err := a.account([]string{"--shell", "fixture", "--browser", "--device"}); err == nil || out.Len() != 0 {
		t.Fatal("conflicting methods changed the shell")
	}
}
