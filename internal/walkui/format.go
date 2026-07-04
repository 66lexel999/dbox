package walkui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"myidm/internal/engine"
)

func humanBytes(n int64) string {
	if n < 0 {
		return ""
	}
	f := float64(n)
	u := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= 1024 && i < len(u)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, u[0])
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", f, u[i])
	}
	return fmt.Sprintf("%.1f %s", f, u[i])
}

func humanSpeed(bps float64) string {
	if bps <= 0 {
		return ""
	}
	return humanBytes(int64(bps)) + "/s"
}

func humanETA(s float64) string {
	if s < 0 || math.IsInf(s, 0) || math.IsNaN(s) {
		return ""
	}
	sec := int(s + 0.5)
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	}
}

func humanDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%s %02d  %02d:%02d", t.Month().String()[:3], t.Day(), t.Hour(), t.Minute())
}

func statusText(t engine.TaskView) string {
	switch t.Status {
	case engine.StatusDownloading:
		if t.Progress >= 0 {
			return fmt.Sprintf("%.1f%%", t.Progress*100)
		}
		return "Receiving…"
	case engine.StatusQueued:
		return "Queued"
	case engine.StatusPaused:
		if t.Progress >= 0 {
			return fmt.Sprintf("Paused %.0f%%", t.Progress*100)
		}
		return "Paused"
	case engine.StatusCompleted:
		if t.FileExists {
			return "Complete"
		}
		return "Complete*"
	case engine.StatusFailed:
		return "Error"
	}
	return string(t.Status)
}

func normDir(d string) string {
	return strings.ToLower(strings.TrimRight(strings.ReplaceAll(d, "\\", "/"), "/"))
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpF(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func etaVal(t engine.TaskView) float64 {
	if t.Status == engine.StatusDownloading && t.ETA >= 0 {
		return t.ETA
	}
	return math.Inf(1)
}

func spdVal(t engine.TaskView) float64 {
	if t.Status == engine.StatusDownloading {
		return t.Speed
	}
	return -1
}

func dateVal(t engine.TaskView) int64 {
	ts := t.CreatedAt
	if t.CompletedAt != nil {
		ts = *t.CompletedAt
	}
	return ts.UnixNano()
}
