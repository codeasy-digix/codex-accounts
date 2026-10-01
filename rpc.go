//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type rpcMessage struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type rpcFailure struct{ payload json.RawMessage }

func (e *rpcFailure) Error() string { return "Codex account service rejected the request" }

type appServer struct {
	cmd       *exec.Cmd
	in        io.WriteCloser
	responses chan rpcMessage
	done      chan struct{}
	readDone  chan struct{}
	id        int
	cancel    context.CancelFunc
}

func configArguments(shared string, isolated bool) []string {
	data, _ := json.Marshal(shared)
	args := []string{"-c", "sqlite_home=" + string(data)}
	if isolated {
		args = append(args, "-c", `cli_auth_credentials_store="file"`, "-c", `forced_login_method="chatgpt"`)
	}
	return args
}

func newAppServer(ctx context.Context, binary, home, shared string, isolated bool) (*appServer, error) {
	childCtx, cancel := context.WithCancel(ctx)
	args := append([]string{"app-server", "--listen", "stdio://"}, configArguments(shared, isolated)...)
	args = append(args, "-c", `model_provider="openai"`)
	cmd := exec.CommandContext(childCtx, binary, args...)
	cmd.Env = accountEnvironment(runtimeEnvironment(binary, os.Environ()), home, shared, isolated)
	cmd.Dir = home
	cmd.Stderr = io.Discard // Never expose RPC error bodies or token-bearing logs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		in.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		in.Close()
		out.Close()
		return nil, errors.New("could not start the bundled Codex runtime")
	}
	s := &appServer{cmd: cmd, in: in, responses: make(chan rpcMessage, 16), done: make(chan struct{}), readDone: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.responses)
		defer close(s.readDone)
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 65536), 8*1024*1024)
		for scanner.Scan() {
			var message rpcMessage
			if json.Unmarshal(scanner.Bytes(), &message) != nil || message.ID == nil {
				continue
			}
			select {
			case s.responses <- message:
			case <-childCtx.Done():
				return
			}
		}
	}()
	go func() { <-s.readDone; cmd.Wait(); close(s.done) }()
	if err := s.request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "codex-accounts", "version": version}}, nil); err != nil {
		s.close()
		return nil, err
	}
	if err := s.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *appServer) send(value any) error { return json.NewEncoder(s.in).Encode(value) }

func (s *appServer) request(parent context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(parent, 40*time.Second)
	defer cancel()
	s.id++
	id := s.id
	if err := s.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return errors.New("Codex account service closed its connection")
	}
	for {
		select {
		case <-ctx.Done():
			return errors.New("account lookup timed out or was cancelled; existing credentials were kept")
		case message, ok := <-s.responses:
			if !ok {
				return errors.New("Codex account service exited before replying")
			}
			if message.ID == nil || *message.ID != id {
				continue
			}
			if len(message.Error) > 0 && string(message.Error) != "null" {
				return &rpcFailure{message.Error}
			}
			if result != nil && json.Unmarshal(message.Result, result) != nil {
				return errors.New("invalid reply from the Codex account service")
			}
			return nil
		}
	}
}

func (s *appServer) close() {
	s.in.Close()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.cancel()
		<-s.done
	}
	s.cancel()
}

func isAuthFailure(err error) bool {
	var failure *rpcFailure
	if !errors.As(err, &failure) {
		return false
	}
	detail := strings.ToLower(string(failure.payload))
	for _, phrase := range []string{"timed out", "timeout", "connection", "network", "dns", "tls", "certificate", "too many requests", "429", "502", "503", "504"} {
		if strings.Contains(detail, phrase) {
			return false
		}
	}
	for _, phrase := range []string{"invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated", "refresh token has expired", "refresh token has already been used", "refresh token was revoked", "invalid refresh token", "invalid access token", "token_expired", "token has expired", "not logged in", "not authenticated", "authentication required", "please sign in again", "please log in again", "unauthorized", "\"401\"", ":401", " 401 "} {
		if strings.Contains(detail, phrase) {
			return true
		}
	}
	return false
}

func safeLookupError(err error) error {
	if isAuthFailure(err) {
		return fmt.Errorf("credentials need another login; run codex account NAME --login")
	}
	return errors.New("account lookup failed; credentials were kept. Check your network/service and retry")
}
