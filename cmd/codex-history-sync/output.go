package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/codeasy-digix/codex-accounts/internal/historysync"
)

type statusReport struct {
	Node     string                   `json:"node"`
	Enabled  bool                     `json:"enabled"`
	Report   *historysync.CycleReport `json:"last_report,omitempty"`
	Hub      *historysync.HubState    `json:"hub,omitempty"`
	HubError string                   `json:"hub_error,omitempty"`
	Pending  int                      `json:"pending"`
}

// Keep machine-readable reports opt-in for interactive commands. The daemon
// log and SSH serve protocol always retain their complete JSON messages.
func displayFlags(args []string) (rest []string, jsonOutput, verbose bool) {
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonOutput = true
		case "--verbose":
			verbose = true
		default:
			rest = append(rest, arg)
		}
	}
	return
}

func compactLine(value string) string {
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return string(runes)
}

func cycleText(report *historysync.CycleReport, dryRun bool) string {
	var out strings.Builder
	label := "마지막 동기화"
	if dryRun {
		label = "동기화 계획 (변경 없음)"
	}
	when := report.Finished
	if when.IsZero() {
		when = report.Started
	}
	if !when.IsZero() {
		fmt.Fprintf(&out, "%s: %s", label, when.Local().Format("2006-01-02 15:04:05 MST"))
	} else {
		fmt.Fprintf(&out, "%s: 시각 기록 없음", label)
	}
	if report.Interrupted {
		out.WriteString(" · 중단됨")
	}
	out.WriteByte('\n')
	fmt.Fprintf(&out, "전송: 업로드 %d · 다운로드 %d · 적용 %d · 동일 %d\n", report.Uploaded, report.Downloaded, report.Installed, report.Unchanged)
	fmt.Fprintf(&out, "대기: %d · 사용 중 %d · 보류 %d\n", report.Pending, report.Busy, report.Deferred)
	if dryRun {
		out.WriteString("전송 건수는 예정 수량입니다.\n")
	}
	// Group repeated deferrals instead of dumping each conversation ID/digest.
	reasons := make(map[string]int)
	for _, result := range report.Results {
		if result.Status == "deferred" && result.Reason != "" {
			reasons[compactLine(result.Reason)]++
		}
	}
	keys := make([]string, 0, len(reasons))
	for reason := range reasons {
		keys = append(keys, reason)
	}
	sort.Slice(keys, func(i, j int) bool {
		if reasons[keys[i]] != reasons[keys[j]] {
			return reasons[keys[i]] > reasons[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, reason := range keys[:min(3, len(keys))] {
		fmt.Fprintf(&out, "보류 사유: %s (%d건)\n", reason, reasons[reason])
	}
	if len(keys) > 3 {
		fmt.Fprintf(&out, "그 외 보류 사유 %d개 (--verbose로 확인)\n", len(keys)-3)
	}
	if len(report.Errors) > 0 {
		fmt.Fprintf(&out, "오류: %d건\n", len(report.Errors))
		for _, err := range report.Errors[:min(3, len(report.Errors))] {
			fmt.Fprintf(&out, "  %s\n", compactLine(err))
		}
		if len(report.Errors) > 3 {
			fmt.Fprintf(&out, "  외 %d건 (--verbose로 확인)\n", len(report.Errors)-3)
		}
	}
	return out.String()
}

func printCycle(output io.Writer, report *historysync.CycleReport, dryRun bool) error {
	_, err := fmt.Fprintf(output, "동기화 [%s]\n%s충돌: 이번 %d · 누적 %d\n", report.Node, cycleText(report, dryRun), report.Conflicts, report.ConflictsTotal)
	return err
}

func unresolved(state *historysync.HubState) int {
	count := 0
	for _, conflict := range state.Conflicts {
		if !conflict.Revision && !conflict.Resolved {
			count++
		}
	}
	return count
}

func printStatus(output io.Writer, status statusReport) error {
	var out strings.Builder
	enabled := "off"
	if status.Enabled {
		enabled = "on"
	}
	fmt.Fprintf(&out, "동기화 [%s]: %s\n", status.Node, enabled)
	if status.Report != nil {
		out.WriteString(cycleText(status.Report, false))
	} else {
		fmt.Fprintf(&out, "동기화 기록 없음 · 대기 %d\n", status.Pending)
	}
	if status.Hub != nil {
		fmt.Fprintf(&out, "허브: 대화 %d · 충돌 누적 %d (미해결 %d)\n", status.Hub.Heads, status.Hub.ConflictsTotal, unresolved(status.Hub))
	} else if status.HubError != "" {
		fmt.Fprintf(&out, "허브 조회 실패: %s\n", compactLine(status.HubError))
		if status.Report != nil {
			fmt.Fprintf(&out, "충돌 누적 %d (이전 기록; 현재 상태 확인 불가)\n", status.Report.ConflictsTotal)
		}
	}
	_, err := io.WriteString(output, out.String())
	return err
}

func printConflicts(output io.Writer, state historysync.HubState) error {
	var out strings.Builder
	fmt.Fprintf(&out, "충돌: 누적 %d · 미해결 %d · 정상 이력 갱신 %d\n", state.ConflictsTotal, unresolved(&state), state.RevisionsTotal)
	conflicts := make([]historysync.Conflict, 0, state.ConflictsTotal)
	for _, conflict := range state.Conflicts {
		if !conflict.Revision {
			conflicts = append(conflicts, conflict)
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Resolved != conflicts[j].Resolved {
			return !conflicts[i].Resolved
		}
		if !conflicts[i].Observed.Equal(conflicts[j].Observed) {
			return conflicts[i].Observed.After(conflicts[j].Observed)
		}
		return conflicts[i].Key < conflicts[j].Key
	})
	for _, conflict := range conflicts[:min(10, len(conflicts))] {
		resolution := "미해결"
		if conflict.Resolved {
			resolution = "해결됨"
		}
		observed := ""
		if !conflict.Observed.IsZero() {
			observed = conflict.Observed.Local().Format(time.DateTime) + " "
		}
		fmt.Fprintf(&out, "  %s%s [%s] %s (%s → %s)\n", observed, compactLine(conflict.ID), resolution, compactLine(conflict.Reason), compactLine(conflict.IncomingNode), compactLine(conflict.CurrentNode))
	}
	if len(conflicts) > 10 {
		fmt.Fprintf(&out, "  외 %d건. 전체: codex-history-sync conflicts --verbose\n", len(conflicts)-10)
	}
	_, err := io.WriteString(output, out.String())
	return err
}
