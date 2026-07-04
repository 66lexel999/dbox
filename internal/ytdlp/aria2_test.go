package ytdlp

import "testing"

func TestParseAria2cProgress(t *testing.T) {
	cases := []struct {
		line   string
		pct    float64
		total  int64 // 0 = skip
		spdMin float64
	}{
		{"[#a1b2c3 4.5MiB/100MiB(4%) CN:16 DL:5.2MiB ETA:18s]", 4, 100 << 20, 5 << 20},
		{"[#7d3c5e 649.8MiB/1.2GiB(49%) CN:16 DL:98MiB ETA:8s]", 49, 0, 90 << 20},
	}
	for _, c := range cases {
		p, ok := ParseAria2cProgress(c.line)
		if !ok {
			t.Fatalf("no match: %s", c.line)
		}
		if p.Percent != c.pct {
			t.Errorf("%s: pct=%v want %v", c.line, p.Percent, c.pct)
		}
		if c.total > 0 && p.Total != c.total {
			t.Errorf("%s: total=%d want %d", c.line, p.Total, c.total)
		}
		if p.SpeedBPS < c.spdMin {
			t.Errorf("%s: speed=%v want >=%v", c.line, p.SpeedBPS, c.spdMin)
		}
	}
	if _, ok := ParseAria2cProgress("[download]  50% of  100.0MiB at 5.0MiB/s"); ok {
		t.Error("aria2 parser wrongly matched a yt-dlp native line")
	}
}
