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

func TestAccountMenuCancelKeepsCredentialsAndShellOutput(t *testing.T) {
	for _, input := range []string{"0\n", "", "invalid\n0\n"} {
		t.Run(strings.ReplaceAll(input, "\n", "-"), func(t *testing.T) {
			root, shared, accounts := sandbox(t)
			home := seed(t, shared, accounts, "chosen")
			t.Setenv("CODEX_ACCOUNT", "chosen")
			t.Setenv("CODEX_HOME", home)
			before, _ := os.ReadFile(filepath.Join(home, "auth.json"))
			a, out, stderr := testApp()
			a.in = strings.NewReader(input)
			if err := a.account([]string{"--shell"}); err != nil || out.Len() != 0 {
				t.Fatal("cancel emitted shell code or failed", err, out.String())
			}
			if !strings.Contains(stderr.String(), "Environment: chosen") || !strings.Contains(stderr.String(), "0. Keep current login") {
				t.Fatal("current details or numeric menu missing", stderr.String())
			}
			after, _ := os.ReadFile(filepath.Join(home, "auth.json"))
			if string(after) != string(before) || len(fixtureLoginCalls(t, root)) != 0 || os.Getenv("CODEX_HOME") != home {
				t.Fatal("cancel changed login or selected environment")
			}
		})
	}
}

func TestAccountMenuAuthenticatesCurrentCredentialStore(t *testing.T) {
	for _, store := range []string{"default", "named", "managed-default"} {
		for _, number := range []string{"1", "2"} {
			t.Run(store+"-"+number, func(t *testing.T) {
				root, shared, accounts := sandbox(t)
				home := seed(t, shared, accounts, "chosen")
				expectedHome, expectedName := shared, ""
				if store == "named" {
					expectedHome, expectedName = home, "chosen"
					t.Setenv("CODEX_ACCOUNT", "chosen")
					t.Setenv("CODEX_HOME", home)
				} else if store == "managed-default" {
					a, _, _ := testApp()
					if _, err := a.publishDefault(shared, accounts, home); err != nil {
						t.Fatal(err)
					}
					expectedHome = home
				} else {
					os.WriteFile(filepath.Join(shared, "auth.json"), []byte("default-fixture"), 0600)
				}
				previousLink, _ := os.Readlink(filepath.Join(shared, "auth.json"))
				a, _, stderr := testApp()
				a.in = strings.NewReader(number + "\n")
				if err := a.account(nil); err != nil {
					t.Fatal("numbered login failed", err, stderr.String())
				}
				calls := fixtureLoginCalls(t, root)
				if len(calls) != 1 || calls[0].Home != expectedHome {
					t.Fatal("wrong credential store", calls)
				}
				device := false
				for _, arg := range calls[0].Args {
					device = device || arg == "--device-auth"
				}
				if device != (number == "1") {
					t.Fatal("wrong authentication method", calls[0].Args)
				}
				currentLink, _ := os.Readlink(filepath.Join(shared, "auth.json"))
				if previousLink != currentLink || os.Getenv("CODEX_ACCOUNT") != expectedName {
					t.Fatal("authentication changed the default role or terminal account")
				}
				data, _ := os.ReadFile(filepath.Join(expectedHome, "auth.json"))
				if !strings.Contains(string(data), "logged-in") {
					t.Fatal("selected credential file was not updated")
				}
			})
		}
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
