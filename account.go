//go:build darwin || linux

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

type accountInfo struct {
	Type     string `json:"type"`
	Email    string `json:"email"`
	PlanType string `json:"planType"`
}
type accountReply struct {
	Account *accountInfo `json:"account"`
}
type limitWindow struct {
	Used    *float64 `json:"usedPercent"`
	Minutes *int     `json:"windowDurationMins"`
	Reset   *int64   `json:"resetsAt"`
}
type limitBucket struct {
	Name      string       `json:"limitName"`
	Primary   *limitWindow `json:"primary"`
	Secondary *limitWindow `json:"secondary"`
}
type limitsReply struct {
	Single  *limitBucket            `json:"rateLimits"`
	Buckets map[string]*limitBucket `json:"rateLimitsByLimitId"`
}
type statusInfo struct {
	Account     *accountInfo
	Limits      limitsReply
	Workspace   string
	LimitsError string
}

var errLoginRequired = errors.New("ChatGPT login is required")

func savedLogin(home string) bool {
	info, err := os.Lstat(filepath.Join(home, "auth.json"))
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8*1024*1024 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return false
	}
	var auth struct {
		Tokens *struct {
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	return json.Unmarshal(data, &auth) == nil && auth.Tokens != nil && auth.Tokens.Refresh != ""
}

func lookup(ctx context.Context, binary, home, shared string, isolated, refresh, validateLimits bool) (statusInfo, error) {
	s, err := newAppServer(ctx, binary, home, shared, isolated)
	if err != nil {
		return statusInfo{}, err
	}
	defer s.close()
	var reply accountReply
	if err := s.request(ctx, "account/read", map[string]bool{"refreshToken": refresh}, &reply); err != nil {
		if isAuthFailure(err) {
			return statusInfo{}, errLoginRequired
		}
		return statusInfo{}, safeLookupError(err)
	}
	info := statusInfo{Account: reply.Account, Workspace: workspaceName(home)}
	if reply.Account == nil || reply.Account.Type != "chatgpt" {
		return info, nil
	}
	if err := s.request(ctx, "account/rateLimits/read", map[string]any{}, &info.Limits); err != nil {
		if validateLimits {
			if isAuthFailure(err) {
				return statusInfo{}, errLoginRequired
			}
			return statusInfo{}, safeLookupError(err)
		}
		info.LimitsError = "Unavailable: check your network/service."
		if isAuthFailure(err) {
			info.LimitsError = "Unavailable: log in again."
		}
	}
	return info, nil
}

func (a application) selectAccount(home, shared string, force bool) (statusInfo, error) {
	lock, err := loginLock(home)
	if err != nil {
		return statusInfo{}, err
	}
	defer lock.Close()
	if err := privateAuth(home); err != nil {
		return statusInfo{}, err
	}
	binary, err := runtimeBinary()
	if err != nil {
		return statusInfo{}, err
	}
	fmt.Fprintln(a.err, "Checking Codex account…")
	var info statusInfo
	if !force && savedLogin(home) {
		info, err = lookup(a.ctx, binary, home, shared, true, true, true)
		if err == nil && (info.Account == nil || info.Account.Type != "chatgpt") {
			err = errLoginRequired
		}
	} else {
		err = errLoginRequired
	}
	if err != nil && !errors.Is(err, errLoginRequired) {
		return statusInfo{}, err
	}
	if errors.Is(err, errLoginRequired) {
		fmt.Fprintln(a.err, "Sign in to this nickname using the device code shown below.")
		args := append([]string{"login", "--device-auth"}, configArguments(shared, true)...)
		cmd := exec.CommandContext(a.ctx, binary, args...)
		cmd.Env = accountEnvironment(runtimeEnvironment(binary, os.Environ()), home, shared, true)
		cmd.Dir = home
		cmd.Stdin, cmd.Stdout, cmd.Stderr = a.in, a.err, a.err
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		if err := cmd.Run(); err != nil {
			return statusInfo{}, errors.New("login did not complete; your terminal account was not changed")
		}
		if err := privateAuth(home); err != nil {
			return statusInfo{}, err
		}
		info, err = lookup(a.ctx, binary, home, shared, true, false, true)
		if err != nil {
			return statusInfo{}, err
		}
		if info.Account == nil || info.Account.Type != "chatgpt" {
			return statusInfo{}, errors.New("login completed without a usable ChatGPT account")
		}
	}
	return info, nil
}

func (a application) showStatus(w io.Writer, shared string) error {
	home := os.Getenv("CODEX_HOME")
	name := os.Getenv("CODEX_ACCOUNT")
	if home == "" {
		home = shared
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	home = abs
	printEnvironment(w, name, home, shared)
	// A brand-new installation is valid even before its first login.
	if _, err := os.Stat(home); os.IsNotExist(err) {
		fmt.Fprintln(w, "Login: not signed in")
		return nil
	}
	var lock *os.File
	if name != "" {
		_, accounts, err := locations()
		if err != nil {
			return err
		}
		if !nicknamePattern.MatchString(name) || filepath.Join(accounts, name) != home {
			return errors.New("CODEX_HOME does not match the selected account")
		}
		if err := privateAuth(home); err != nil {
			return err
		}
		lock, err = loginLock(home)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	binary, err := runtimeBinary()
	if err != nil {
		return err
	}
	info, err := lookup(a.ctx, binary, home, shared, name != "", false, false)
	if err != nil {
		fmt.Fprintln(w, "Login/limits: lookup unavailable; existing credentials were kept.")
		return nil
	}
	printAccount(w, info)
	return nil
}

func printEnvironment(w io.Writer, name, home, shared string) {
	if name == "" {
		name = "default"
	}
	fmt.Fprintf(w, "Environment: %s\nCODEX_HOME: %s\nShared conversations: %s\n", cleanText(name), cleanText(home), cleanText(shared))
}
func printStatus(w io.Writer, name, home, shared string, info statusInfo) {
	printEnvironment(w, name, home, shared)
	printAccount(w, info)
}

func printAccount(w io.Writer, info statusInfo) {
	if info.Account == nil {
		fmt.Fprintln(w, "Login: not signed in")
		return
	}
	if info.Account.Type != "chatgpt" {
		fmt.Fprintf(w, "Login: %s (ChatGPT limits unavailable)\n", cleanText(info.Account.Type))
		return
	}
	email := info.Account.Email
	if email == "" {
		email = "not provided"
	}
	fmt.Fprintln(w, "Email:", cleanText(email))
	if info.Account.PlanType != "" {
		fmt.Fprintln(w, "Plan:", cleanText(info.Account.PlanType))
	}
	if info.Workspace != "" {
		fmt.Fprintln(w, "Workspace:", cleanText(info.Workspace))
	}
	if info.LimitsError != "" {
		fmt.Fprintln(w, "Limits:", info.LimitsError)
		return
	}
	printLimits(w, info.Limits)
}

func printLimits(w io.Writer, limits limitsReply) {
	buckets := limits.Buckets
	if len(buckets) == 0 && limits.Single != nil {
		buckets = map[string]*limitBucket{"codex": limits.Single}
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := 0
	for _, key := range keys {
		bucket := buckets[key]
		if bucket == nil {
			continue
		}
		for i, window := range []*limitWindow{bucket.Primary, bucket.Secondary} {
			if window == nil || window.Used == nil || math.IsNaN(*window.Used) || math.IsInf(*window.Used, 0) {
				continue
			}
			used := math.Min(100, math.Max(0, *window.Used))
			label := "primary"
			if i == 1 {
				label = "secondary"
			}
			if window.Minutes != nil {
				switch *window.Minutes {
				case 300:
					label = "5h"
				case 10080:
					label = "week"
				default:
					label = strconv.Itoa(*window.Minutes) + "min"
				}
			}
			prefix := ""
			if len(buckets) > 1 {
				name := bucket.Name
				if name == "" {
					name = key
				}
				prefix = cleanText(name) + " "
			}
			reset := ""
			if window.Reset != nil {
				reset = " / resets " + time.Unix(*window.Reset, 0).Local().Format("01-02 15:04 MST")
			}
			fmt.Fprintf(w, "%s%s: %s%% remaining (%s%% used)%s\n", prefix, label, formatPercent(100-used), formatPercent(used), reset)
			lines++
		}
	}
	if lines == 0 {
		fmt.Fprintln(w, "Limits: no information provided")
	}
}

func formatPercent(n float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", n), "0"), ".")
}

func cleanText(s string) string {
	runes := make([]rune, 0, 200)
	for _, r := range s {
		if unicode.IsPrint(r) {
			runes = append(runes, r)
			if len(runes) == 200 {
				break
			}
		}
	}
	return string(runes)
}

func workspaceName(home string) string {
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil || len(data) > 8*1024*1024 {
		return ""
	}
	var login struct {
		Tokens struct {
			IDToken   string `json:"id_token"`
			AccountID string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &login) != nil {
		return ""
	}
	parts := strings.Split(login.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return ""
	}
	claims, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var payload struct {
		Auth struct {
			ID            string `json:"chatgpt_account_id"`
			Organizations []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Name  string `json:"name"`
			} `json:"organizations"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(claims, &payload) != nil {
		return ""
	}
	id := login.Tokens.AccountID
	if id == "" {
		id = payload.Auth.ID
	}
	if id == "" {
		return ""
	}
	for _, org := range payload.Auth.Organizations {
		if org.ID == id {
			if org.Title != "" {
				return org.Title
			}
			return org.Name
		}
	}
	return ""
}
