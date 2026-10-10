// codex-history-sync is deliberately separate from the packaged codex command.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/codeasy-digix/codex-accounts/internal/historysync"
)

func main() {
	configureMemoryLimit()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "codex-history-sync:", err)
		os.Exit(1)
	}
}

const defaultMemoryLimit int64 = 256 << 20

// Each daemon and SSH serve process applies its own soft Go runtime limit.
// Large live bundles may exceed it; an operator can select GOMEMLIMIT instead.
func configureMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemoryLimit)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".codex-history-sync", "v2", "config.json")
	storeOverride := ""
	args, err = globalFlags(args, &configPath, &storeOverride)
	if err != nil {
		return err
	}
	args, jsonOutput, verbose := displayFlags(args)
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(output, "Usage: codex-history-sync [--config PATH] [--store PATH] sync [--dry-run] | plan | status | conflicts | daemon | serve\n\nInteractive commands show a compact summary. Use --json or --verbose for the complete report.\nThis optional relay only synchronizes conversation history. Configure enabled:true to run sync or daemon.")
		return nil
	}
	command := args[0]
	if (command == "serve" || command == "daemon") && (jsonOutput || verbose) {
		return fmt.Errorf("%s already uses the JSON protocol; --json/--verbose are for interactive commands", command)
	}
	if command == "serve" {
		if len(args) != 1 {
			return errors.New("serve accepts only --store PATH")
		}
		if storeOverride == "" {
			storeOverride = filepath.Join(home, ".codex-history-sync", "v2", "hub")
		}
		storeOverride = localPath(storeOverride, home)
		if !filepath.IsAbs(storeOverride) {
			return errors.New("serve store must be an absolute path")
		}
		return historysync.Serve(ctx, storeOverride, input, output)
	}
	file, err := os.Open(localPath(configPath, home))
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	var cfg historysync.Config
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&cfg)
	if err == nil {
		var trailing any
		if extraErr := decoder.Decode(&trailing); !errors.Is(extraErr, io.EOF) {
			err = errors.New("extra JSON after configuration")
		}
	}
	file.Close()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if storeOverride != "" {
		cfg.Store = storeOverride
	}
	cfg, err = historysync.NormalizeConfig(cfg)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if command == "conflicts" {
		if len(args) != 1 {
			return errors.New("conflicts accepts no positional arguments")
		}
		state, err := historysync.ReadHubState(ctx, cfg)
		if err != nil {
			return err
		}
		if jsonOutput || verbose {
			return encoder.Encode(state)
		}
		return printConflicts(output, state)
	}
	if command == "status" {
		if len(args) != 1 {
			return errors.New("status accepts no positional arguments")
		}
		report, err := historysync.ReadStatus(cfg.Store)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		state, stateErr := historysync.ReadHubState(ctx, cfg)
		hubError := ""
		if stateErr != nil {
			hubError = stateErr.Error()
		}
		pendingCount, err := historysync.ReadPendingCount(cfg.Store)
		if err != nil {
			return fmt.Errorf("read pending status: %w", err)
		}
		if report != nil {
			report.HubStateCurrent = stateErr == nil
			if stateErr == nil {
				report.ConflictsTotal = state.ConflictsTotal
			}
			report.Pending = pendingCount
		}
		status := statusReport{cfg.Node, cfg.Enabled, report, func() *historysync.HubState {
			if stateErr != nil {
				return nil
			}
			return &state
		}(), hubError, pendingCount}
		if jsonOutput || verbose {
			return encoder.Encode(status)
		}
		return printStatus(output, status)
	}
	if command != "sync" && command != "plan" && command != "daemon" {
		return fmt.Errorf("unknown command %q", command)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dryRun := flags.Bool("dry-run", false, "show planned uploads and downloads")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "plan" {
		*dryRun = true
	}
	if command == "daemon" && *dryRun {
		return errors.New("daemon does not accept --dry-run")
	}
	if !*dryRun && !cfg.Enabled {
		return errors.New("history synchronization is disabled in configuration")
	}
	native, err := historysync.NewNative(cfg)
	if err != nil {
		return err
	}
	relay, err := historysync.NewRelay(cfg, native)
	if err != nil {
		return err
	}
	if command != "daemon" {
		report, err := relay.Sync(ctx, *dryRun)
		var outputErr error
		if jsonOutput || verbose {
			outputErr = encoder.Encode(report)
		} else {
			outputErr = printCycle(output, &report, *dryRun)
		}
		if outputErr != nil {
			return outputErr
		}
		return err
	}
	// The supervisor (launchd, systemd, or tmux) owns the process lifecycle.
	encoder.SetIndent("", "")
	for {
		report, cycleErr := relay.Sync(ctx, false)
		if err := encoder.Encode(report); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if cycleErr != nil {
			fmt.Fprintln(os.Stderr, "codex-history-sync: cycle failed; see last-report.json")
		}
		timer := time.NewTimer(time.Duration(cfg.IntervalSeconds) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Global path options work before or after the command, including the fixed
// remotely executed "serve --store PATH" form.
func globalFlags(args []string, config, store *string) ([]string, error) {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var target *string
		switch {
		case arg == "--config":
			target = config
		case arg == "--store":
			target = store
		case strings.HasPrefix(arg, "--config="):
			*config = strings.TrimPrefix(arg, "--config=")
			continue
		case strings.HasPrefix(arg, "--store="):
			*store = strings.TrimPrefix(arg, "--store=")
			continue
		default:
			rest = append(rest, arg)
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s requires a path", arg)
		}
		i++
		*target = args[i]
		if *target == "" {
			return nil, fmt.Errorf("%s requires a nonempty path", arg)
		}
	}
	return rest, nil
}

func localPath(value, home string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	return value
}
