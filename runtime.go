//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type codexRuntime struct {
	binary string
	source string
}

func selectRuntime() (codexRuntime, error) {
	if override := os.Getenv("CODEX_ACCOUNTS_RUNTIME"); override != "" {
		p, err := filepath.Abs(override)
		if err != nil {
			return codexRuntime{}, err
		}
		if !executable(p) {
			return codexRuntime{}, errors.New("CODEX_ACCOUNTS_RUNTIME is not an executable file")
		}
		return codexRuntime{p, "override"}, nil
	}
	shared, _, err := locations()
	if err != nil {
		return codexRuntime{}, err
	}
	if binary, err := managedRuntime(shared); err != nil {
		return codexRuntime{}, err
	} else if binary != "" {
		return codexRuntime{binary, "daemon"}, nil
	}
	self, err := os.Executable()
	if err != nil {
		return codexRuntime{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return codexRuntime{}, err
	}
	if binary := installedRuntime(self); binary != "" {
		return codexRuntime{binary, "path"}, nil
	}
	binary := filepath.Join(filepath.Dir(self), "..", "libexec", "codex", "bin", "codex")
	if executable(binary) {
		return codexRuntime{filepath.Clean(binary), "bundled"}, nil
	}
	return codexRuntime{}, errors.New("bundled Codex is missing; reinstall with brew reinstall codex-accounts")
}

var releaseVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

func managedRuntime(shared string) (string, error) {
	root := filepath.Join(shared, "packages", "app-server-daemon")
	current := filepath.Join(root, "current")
	if _, err := os.Lstat(current); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	binary, err := filepath.EvalSymlinks(filepath.Join(current, "bin", "codex"))
	if err != nil || !executable(binary) {
		return "", errors.New("managed Codex package is incomplete; repair the daemon installation or set CODEX_ACCOUNTS_RUNTIME explicitly")
	}
	if _, err := os.Stat(filepath.Join(shared, "app-server-daemon", "daemon.pid")); os.IsNotExist(err) {
		return binary, nil
	} else if err != nil {
		return "", err
	}

	// A scheduled download may advance current while busy agents still use an
	// older daemon. Ask the native read-only command which release is running;
	// never start, update, restart or authenticate a server during selection.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "app-server", "daemon", "version")
	cmd.Env = accountEnvironment(runtimeEnvironment(binary, os.Environ()), shared, shared, false)
	cmd.Env = setEnvironment(cmd.Env, "CODEX_ACCOUNT", "")
	cmd.Dir = shared
	data, err := cmd.Output()
	if err != nil {
		return "", errors.New("could not inspect the Codex background server; no runtime or server was changed")
	}
	var status struct {
		Status              string `json:"status"`
		ManagedCodexVersion string `json:"managedCodexVersion"`
		AppServerVersion    string `json:"appServerVersion"`
	}
	if err := json.Unmarshal(data, &status); err != nil || status.Status == "" {
		return "", errors.New("invalid Codex background server version response")
	}
	if status.Status != "running" {
		return binary, nil
	}
	if !releaseVersionPattern.MatchString(status.AppServerVersion) || !releaseVersionPattern.MatchString(status.ManagedCodexVersion) {
		return "", errors.New("running Codex background server did not report a valid release version")
	}
	if status.AppServerVersion == status.ManagedCodexVersion {
		return binary, nil
	}
	release := filepath.Base(filepath.Dir(filepath.Dir(binary)))
	prefix := status.ManagedCodexVersion + "-"
	if !strings.HasPrefix(release, prefix) {
		return "", errors.New("managed Codex package path does not match its reported release")
	}
	running := filepath.Join(root, "releases", status.AppServerVersion+"-"+strings.TrimPrefix(release, prefix), "bin", "codex")
	if !executable(running) {
		return "", fmt.Errorf("running Codex %s package is missing; finish active work before updating the background server", status.AppServerVersion)
	}
	return filepath.EvalSymlinks(running)
}
