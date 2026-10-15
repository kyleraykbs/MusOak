package provider

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// yt-dlp's progress, run with `--newline --progress`, arrives on stdout one line
// per download update - even under `--quiet`, which is what the download path
// uses. The downloader builds those lines from fixed templates
// (yt_dlp/downloader/common.py), so there are four shapes:
//
//	[download]  45.2% of    3.84MiB at   2.10MiB/s ETA 00:02
//	[download]  45.2% of ~   3.84MiB at   2.10MiB/s ETA 00:02
//	[download] 100% of    3.84MiB in 00:00:02 at   2.10MiB/s
//	[download]    1.23MiB at   2.10MiB/s ETA Unknown
//
// The first counts against a total the tool knows, the second against one it can
// only estimate (the `~`), the third is the finished download, and the fourth is
// a stream whose size is unknown - a live stream - where only the bytes taken so
// far are printed. Every other line yt-dlp says, such as its "Destination:"
// line, is not a progress report.
const ytdlpProgressPrefix = "[download]"

// ParseYtdlpProgress reads one line of `yt-dlp --newline --progress` output and
// reports the bytes fetched so far and the total the tool expects. ok is false
// for any line that is not a download progress report.
//
// A total yt-dlp only estimated is taken as the total: a bar that is close is
// what this is for. A stream with no total at all - a live one - is reported
// with Total 0, which asks a client to sweep rather than fill against a
// denominator nobody can compute.
func ParseYtdlpProgress(line string) (Progress, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != ytdlpProgressPrefix {
		return Progress{}, false
	}
	fields = fields[1:]

	if percent, ok := parsePercent(fields[0]); ok {
		total, counted := totalAfterOf(fields)
		if !counted {
			return Progress{Stage: StageDownload}, true
		}
		done := int64(float64(total) * percent / 100)
		if done > total {
			done = total
		}
		return Progress{Stage: StageDownload, Done: done, Total: total}, true
	}

	// Without a percentage yt-dlp prints the bytes it has taken and nothing to
	// divide them by.
	if done, ok := parseByteCount(fields[0]); ok {
		return Progress{Stage: StageDownload, Done: done}, true
	}
	return Progress{}, false
}

// parsePercent reads the percentage a progress line opens with: "45.2%" or
// "100%". "N/A%" - the tool's answer when it has no count to give - is not one.
func parsePercent(token string) (float64, bool) {
	value, found := strings.CutSuffix(token, "%")
	if !found {
		return 0, false
	}
	percent, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return percent, true
}

// totalAfterOf reads the size a progress line carries after its "of". The "~"
// that marks yt-dlp's own estimate is dropped: it says where the number came
// from, not what it is.
func totalAfterOf(fields []string) (int64, bool) {
	for i, field := range fields {
		if field != "of" || i+1 >= len(fields) {
			continue
		}
		size := strings.TrimPrefix(fields[i+1], "~")
		if size == "" && i+2 < len(fields) {
			// The estimate's "~" and its padded size are two fields when the
			// size is short enough to be padded.
			size = fields[i+2]
		}
		return parseByteCount(size)
	}
	return 0, false
}

// byteCountPattern matches the sizes yt-dlp prints: two decimals and a binary
// unit, "3.84MiB", "512.00KiB", "0.00B".
var byteCountPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMGTPEZY]i)?B$`)

// byteCountUnits are those units in bytes.
var byteCountUnits = map[string]int64{
	"":   1,
	"Ki": 1 << 10,
	"Mi": 1 << 20,
	"Gi": 1 << 30,
	"Ti": 1 << 40,
	"Pi": 1 << 50,
	"Ei": 1 << 60,
}

// parseByteCount reads one of those sizes into a byte count.
func parseByteCount(token string) (int64, bool) {
	match := byteCountPattern.FindStringSubmatch(token)
	if match == nil {
		return 0, false
	}
	unit, known := byteCountUnits[match[2]]
	if !known {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	return int64(value * float64(unit)), true
}

// progressInterval is the shortest gap between two progress reports. A status
// reader polls every 250 ms, so a line per chunk - dozens a second on a fast
// connection - is detail nobody can see. It is a var so a test can take the
// clock out of the gate.
var progressInterval = 250 * time.Millisecond

// YtdlpProgressReporter turns yt-dlp's progress lines into the reports a
// provider hands to the manager. A line that repeats the last reading is
// dropped, and one that has not reached its total is held back to
// progressInterval, so a download reports a few times a second rather than on
// every chunk.
type YtdlpProgressReporter struct {
	report ProgressFunc
	last   Progress
	at     time.Time
}

// NewYtdlpProgressReporter returns a reporter that reports to fn, which may be
// nil: a caller with nobody to tell then gets a reporter that does nothing.
func NewYtdlpProgressReporter(fn ProgressFunc) *YtdlpProgressReporter {
	return &YtdlpProgressReporter{report: fn}
}

// Line reads one line of yt-dlp output and reports the progress in it, if it
// holds any and if it is worth sending.
func (r *YtdlpProgressReporter) Line(line string) {
	if r == nil || r.report == nil {
		return
	}
	p, ok := ParseYtdlpProgress(line)
	if !ok || p == r.last {
		return
	}
	// The reading that has reached its total is the end of the download and
	// always goes out; anything short of that waits its turn.
	complete := p.Total > 0 && p.Done >= p.Total
	if !complete && !r.at.IsZero() && time.Since(r.at) < progressInterval {
		return
	}
	r.last, r.at = p, time.Now()
	r.report(p)
}

// SectionTimestamp is a time inside a track the way yt-dlp's --download-sections
// wants it: hours:minutes:seconds, with milliseconds only when there are any.
//
// A listener's position is milliseconds, and the tool reads a clock; this is the
// one place that translation happens, so a section that starts where somebody is
// is the same number everywhere.
func SectionTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	hours := ms / 3_600_000
	minutes := (ms % 3_600_000) / 60_000
	seconds := (ms % 60_000) / 1000
	millis := ms % 1000
	if millis == 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%d:%02d:%02d.%03d", hours, minutes, seconds, millis)
}
