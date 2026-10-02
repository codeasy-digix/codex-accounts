//go:build darwin || linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type continueOptions struct {
	category, output, selection string
	help                        bool
}

const continueUsage = "usage: codex continue [--quota|--other] [--list|--json|--all|UUID|numbers]"

func parseContinueOptions(args []string) (continueOptions, error) {
	var options continueOptions
	for _, arg := range args {
		switch arg {
		case "--help", "-h":
			options.help = true
		case "--quota", "--other":
			if options.category != "" {
				return options, errors.New(continueUsage)
			}
			options.category = strings.TrimPrefix(arg, "--")
		case "--list", "--json":
			if options.output != "" || options.selection != "" {
				return options, errors.New(continueUsage)
			}
			options.output = arg
		default:
			if options.selection != "" || options.output != "" || (strings.HasPrefix(arg, "-") && arg != "--all") {
				return options, errors.New(continueUsage)
			}
			options.selection = arg
		}
	}
	return options, nil
}

func (a application) continueAccounts(args []string) error {
	options, err := parseContinueOptions(args)
	if err != nil {
		return err
	}
	if options.help {
		fmt.Fprintln(a.out, `codex continue: external CLI controller for stopped conversations.
  --list / --json      List only, without login or model requests
  --quota             Show account usage/rate-limit interruptions
  --other             Show errors, user interruptions and unfinished turns
  --quota --all       Continue all idle quota interruptions
  --other --all       Continue all idle other interruptions
  --all               Continue both groups
With no flags, select numbers, quota, other, all, or q in the CLI menu.
Jobs use this terminal's account and the original working directory in separate
tmux sessions. Finished sessions close; private logs/status remain.`)
		return nil
	}
	shared, accounts, err := locations()
	if err != nil {
		return err
	}
	threads, skipped, err := scanInterrupted(a.ctx.Done(), shared, accounts)
	if err != nil {
		return err
	}
	ready := make([]interruptedThread, 0, len(threads))
	counts := map[string]int{quotaCategory: 0, otherCategory: 0}
	activeSkipped := 0
	for _, thread := range threads {
		if options.category != "" && thread.Category != options.category {
			continue
		}
		if thread.Active {
			activeSkipped++
			continue
		}
		ready = append(ready, thread)
		counts[thread.Category]++
	}
	// Keep one numbering scheme while grouping quota first, then other causes.
	sort.SliceStable(ready, func(i, j int) bool {
		return ready[i].Category == quotaCategory && ready[j].Category != quotaCategory
	})
	if options.output == "--json" {
		return json.NewEncoder(a.out).Encode(map[string]any{"conversations": ready, "counts": counts, "skippedFiles": skipped, "activeSkipped": activeSkipped})
	}
	if activeSkipped > 0 {
		fmt.Fprintf(a.err, "Excluded %d active conversation(s) from the stopped-work list.\n", activeSkipped)
	}
	if skipped > 0 {
		fmt.Fprintf(a.err, "Skipped %d unreadable rollout files.\n", skipped)
	}
	for _, group := range []struct{ category, label string }{{quotaCategory, "Quota interruptions"}, {otherCategory, "Other interruptions"}} {
		if options.category != "" && options.category != group.category {
			continue
		}
		fmt.Fprintf(a.out, "%s (%d):\n", group.label, counts[group.category])
		for i, thread := range ready {
			if thread.Category == group.category {
				fmt.Fprintf(a.out, "%d. %s\n   %s | %s | %s\n   %s\n", i+1, thread.Title, thread.ID, thread.Reason, thread.Stopped, cleanText(thread.Cwd))
			}
		}
	}
	if len(ready) == 0 {
		fmt.Fprintln(a.out, "No interrupted conversations found.")
		return nil
	}
	if options.output == "--list" {
		return nil
	}
	selection := options.selection
	if selection == "" {
		groups := "quota, other"
		if options.category != "" {
			groups = options.category
		}
		fmt.Fprintf(a.out, "Continue [number(s), %s, all, q]: ", groups)
		selection, err = bufio.NewReader(a.in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	selected, err := chooseThreads(ready, strings.TrimSpace(selection))
	if err != nil || len(selected) == 0 {
		return err
	}
	return a.launchContinuations(shared, accounts, selected)
}

func chooseThreads(threads []interruptedThread, selection string) ([]interruptedThread, error) {
	if selection == "" || selection == "q" || selection == "quit" {
		return nil, nil
	}
	if selection == "all" || selection == "a" || selection == "--all" {
		return threads, nil
	}
	if selection == quotaCategory || selection == otherCategory {
		selected := make([]interruptedThread, 0)
		for _, thread := range threads {
			if thread.Category == selection {
				selected = append(selected, thread)
			}
		}
		return selected, nil
	}
	selected := make([]interruptedThread, 0)
	seen := make(map[string]bool)
	for _, part := range strings.Split(selection, ",") {
		part = strings.TrimSpace(part)
		var found *interruptedThread
		if n, err := strconv.Atoi(part); err == nil && n > 0 && n <= len(threads) {
			found = &threads[n-1]
		} else {
			for i := range threads {
				if threads[i].ID == part {
					found = &threads[i]
					break
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("unknown conversation selection: %s", cleanText(part))
		}
		if !seen[found.ID] {
			selected = append(selected, *found)
			seen[found.ID] = true
		}
	}
	return selected, nil
}
