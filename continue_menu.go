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
With no flags, choose a numbered conversation or numbered batch action.
Comma-separated numbers select several conversations; 0 cancels.
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
	actions := continueMenuActions(ready, options.category)
	if selection == "" {
		fmt.Fprintln(a.out, "\nActions:")
		for i, action := range actions {
			fmt.Fprintf(a.out, "%d. %s\n", len(ready)+i+1, action.label)
		}
		fmt.Fprintln(a.out, "0. Cancel")
		if a.in == nil {
			return nil
		}
		reader := bufio.NewReader(a.in)
		for {
			fmt.Fprint(a.out, "Select a number (or several: 1,3), 0 to cancel: ")
			line, readErr := reader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			selected, selectionErr := chooseContinueSelection(ready, actions, strings.TrimSpace(line))
			if selectionErr != nil {
				if errors.Is(readErr, io.EOF) {
					return selectionErr
				}
				fmt.Fprintln(a.err, "Invalid selection. Enter a listed number, several numbers separated by commas, or 0.")
				continue
			}
			if len(selected) == 0 {
				return nil
			}
			return a.launchContinuations(shared, accounts, selected)
		}
	}
	selected, err := chooseContinueSelection(ready, actions, strings.TrimSpace(selection))
	if err != nil || len(selected) == 0 {
		return err
	}
	return a.launchContinuations(shared, accounts, selected)
}

type continueMenuAction struct{ label, selection string }

func continueMenuActions(threads []interruptedThread, category string) []continueMenuAction {
	actions := make([]continueMenuAction, 0, 3)
	if category == "" {
		counts := map[string]int{}
		for _, thread := range threads {
			counts[thread.Category]++
		}
		if counts[quotaCategory] > 0 {
			actions = append(actions, continueMenuAction{fmt.Sprintf("Continue all quota interruptions (%d)", counts[quotaCategory]), quotaCategory})
		}
		if counts[otherCategory] > 0 {
			actions = append(actions, continueMenuAction{fmt.Sprintf("Continue all other interruptions (%d)", counts[otherCategory]), otherCategory})
		}
	}
	return append(actions, continueMenuAction{fmt.Sprintf("Continue all listed conversations (%d)", len(threads)), "all"})
}

func chooseContinueSelection(threads []interruptedThread, actions []continueMenuAction, selection string) ([]interruptedThread, error) {
	if selection == "" || selection == "0" || selection == "q" || selection == "quit" {
		return nil, nil
	}
	selected := make([]interruptedThread, 0)
	seen := make(map[string]bool)
	for _, part := range strings.Split(selection, ",") {
		part = strings.TrimSpace(part)
		if number, err := strconv.Atoi(part); err == nil && number > len(threads) && number <= len(threads)+len(actions) {
			part = actions[number-len(threads)-1].selection
		}
		if part == "" || part == "0" || part == "q" || part == "quit" {
			return nil, errors.New("cancel cannot be combined with another selection")
		}
		batch, err := chooseThreads(threads, part)
		if err != nil {
			return nil, err
		}
		for _, thread := range batch {
			if !seen[thread.ID] {
				selected = append(selected, thread)
				seen[thread.ID] = true
			}
		}
	}
	return selected, nil
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
