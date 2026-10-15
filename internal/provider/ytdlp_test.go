package provider

import (
	"testing"
	"time"
)

// The lines below are what `yt-dlp --newline --progress` writes to stdout, run
// with the flags the download path uses. The first three were taken verbatim
// from yt-dlp 2026.08.19 fetching a 1.91 MiB file; the rest are the other shapes
// its progress templates produce, including the `~` that marks an estimated
// total and the no-percentage form a stream with no total falls back to.
func TestParseYtdlpProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want Progress
		ok   bool
	}{
		{
			name: "captured, part way through",
			line: "[download]  52.4% of    1.91MiB at  388.13MiB/s ETA 00:00",
			want: Progress{Stage: StageDownload, Done: 1049456, Total: 2002780},
			ok:   true,
		},
		{
			name: "captured, a tenth of a percent in",
			line: "[download]   0.1% of    1.91MiB at  Unknown B/s ETA Unknown",
			want: Progress{Stage: StageDownload, Done: 2002, Total: 2002780},
			ok:   true,
		},
		{
			name: "captured, the hundred per cent line",
			line: "[download] 100% of    1.91MiB in 00:00:00 at 471.25MiB/s",
			want: Progress{Stage: StageDownload, Done: 2002780, Total: 2002780},
			ok:   true,
		},
		{
			name: "an estimated total is still a total",
			line: "[download]  50.0% of ~   3.84MiB at   2.10MiB/s ETA 00:02",
			want: Progress{Stage: StageDownload, Done: 2013265, Total: 4026531},
			ok:   true,
		},
		{
			name: "an estimate whose size fills its padding",
			line: "[download]  25.0% of ~1023.99KiB at   2.10MiB/s ETA 00:03",
			want: Progress{Stage: StageDownload, Done: 262141, Total: 1048565},
			ok:   true,
		},
		{
			name: "an estimate, wrapped up",
			line: "[download] 100% of ~   4.00MiB in 00:00:02 at   2.10MiB/s",
			want: Progress{Stage: StageDownload, Done: 4194304, Total: 4194304},
			ok:   true,
		},
		{
			name: "a fragment download says how many pieces it has",
			line: "[download]  50.0% of    4.00MiB at   2.10MiB/s ETA 00:02 (frag 3/8)",
			want: Progress{Stage: StageDownload, Done: 2097152, Total: 4194304},
			ok:   true,
		},
		{
			name: "no total at all: the bytes taken so far",
			line: "[download]    1.23MiB at   2.10MiB/s ETA Unknown",
			want: Progress{Stage: StageDownload, Done: 1289748},
			ok:   true,
		},
		{
			name: "no total at all, with the elapsed time",
			line: "[download]    1.23MiB at   2.10MiB/s (00:01)",
			want: Progress{Stage: StageDownload, Done: 1289748},
			ok:   true,
		},
		{
			name: "a stream with nothing counted",
			line: "[download]   N/A% at  Unknown B/s ETA Unknown",
			ok:   false,
		},
		{
			name: "the destination line is not progress",
			line: "[download] Destination: /tmp/abc.webm",
			ok:   false,
		},
		{
			name: "a resume is not progress",
			line: "[download] Resuming download at byte 4096",
			ok:   false,
		},
		{
			name: "another extractor's line is not ours",
			line: "[youtube] abc: Downloading webpage",
			ok:   false,
		},
		{
			name: "an empty line",
			line: "",
			ok:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseYtdlpProgress(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (line %q)", ok, tc.ok, tc.line)
			}
			if ok && got != tc.want {
				t.Errorf("progress = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The reporter is what a download actually uses: it drops a reading that
// repeats the last one, holds the rest back to progressInterval, and never
// holds back the reading that has arrived at its total.
func TestYtdlpProgressReporter(t *testing.T) {
	defer func(interval time.Duration) { progressInterval = interval }(progressInterval)

	var got []Progress
	reporter := NewYtdlpProgressReporter(func(p Progress) { got = append(got, p) })

	progressInterval = time.Hour
	reporter.Line("[download]  10.0% of    4.00MiB at   1.00MiB/s ETA 00:03")
	reporter.Line("[download]  10.0% of    4.00MiB at   1.00MiB/s ETA 00:03") // a repeat
	reporter.Line("[download]  40.0% of    4.00MiB at   1.00MiB/s ETA 00:01") // held back
	reporter.Line("[download]  70.0% of    4.00MiB at   1.00MiB/s ETA 00:00") // held back
	reporter.Line("[download] 100% of    4.00MiB in 00:00:04 at   1.00MiB/s") // the end

	if len(got) != 2 {
		t.Fatalf("reports = %d (%+v), want 2: one opening reading and the end", len(got), got)
	}
	if got[0].Done != 419430 || got[0].Total != 4194304 {
		t.Errorf("first report = %+v, want 419430/4194304", got[0])
	}
	if got[1].Done != got[1].Total {
		t.Errorf("last report = %+v, want the whole file", got[1])
	}

	// With the gate lifted the readings in between arrive too, so a client on a
	// slow line still sees the bar move.
	progressInterval = 0
	reporter.Line("[download]  40.0% of    4.00MiB at   1.00MiB/s ETA 00:01")
	reporter.Line("[download]  70.0% of    4.00MiB at   1.00MiB/s ETA 00:00")
	if len(got) != 4 || got[2].Done != 1677721 || got[3].Done != 2936012 {
		t.Errorf("reports = %+v, want the two held-back readings to follow", got)
	}
}

// A nil callback is a caller with nobody to tell, not a crash.
func TestYtdlpProgressReporterWithoutACallback(t *testing.T) {
	reporter := NewYtdlpProgressReporter(nil)
	reporter.Line("[download] 100% of    1.00MiB in 00:00:01 at   1.00MiB/s")
}

// A position inside a track, said the way yt-dlp's --download-sections wants it.
func TestSectionTimestamp(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{0, "0:00:00"},
		{1000, "0:00:01"},
		{90_500, "0:01:30.500"},
		{214_000, "0:03:34"},
		{3_600_000, "1:00:00"},
		{3_661_234, "1:01:01.234"},
		{-5, "0:00:00"},
	}
	for _, test := range tests {
		if got := SectionTimestamp(test.ms); got != test.want {
			t.Errorf("SectionTimestamp(%d) = %q, want %q", test.ms, got, test.want)
		}
	}
}
