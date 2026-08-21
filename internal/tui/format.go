package tui

import (
	"strconv"
	"time"
)

const emptyCell = "-"

func formatTime(t time.Time) string {
	if t.IsZero() {
		return emptyCell
	}
	return t.Local().Format("15:04:05")
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return emptyCell
	}
	return formatTime(*t)
}

func formatExitCode(code *int) string {
	if code == nil {
		return emptyCell
	}
	return strconv.Itoa(*code)
}

func formatBytes(n int64) string {
	if n <= 0 {
		return emptyCell
	}
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	units := "KMGTPE"
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + string(units[exp]) + "iB"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
