package ytdlp

import "testing"

// YouTube often omits filesize on adaptive formats but reports tbr; distil must
// still produce a size for every option (estimated from tbr × duration), and a
// video-only stream's total must include the audio it gets muxed with.
func TestDistilEstimatesSizes(t *testing.T) {
	in := &info{
		Title:    "Clip",
		Duration: 600, // 10 minutes
		Formats: []Format{
			{Height: 1080, VCodec: "vp9", ACodec: "none", TBR: 2000},                // video-only, no filesize
			{Height: 720, VCodec: "avc1", ACodec: "none", FileSizeApprox: 50 << 20}, // video-only, reported size
			{Height: 0, VCodec: "none", ACodec: "opus", TBR: 128},                   // audio-only
		},
	}
	res := distil(in)
	if len(res.Options) == 0 {
		t.Fatal("no options")
	}
	for _, o := range res.Options {
		if o.Size <= 0 {
			t.Errorf("option %q has no size", o.Label)
		}
	}
	// 1080 video-only (~143MB from tbr) must include the audio (~9MB), so > 150MB.
	var got1080 int64
	for _, o := range res.Options {
		if o.Selector == "bv*[height<=1080][vcodec^=avc1]+ba/bv*[height<=1080]+ba/b[height<=1080]" {
			got1080 = o.Size
		}
	}
	if got1080 < 150<<20 {
		t.Errorf("1080p size %d should include audio (>150MB)", got1080)
	}
}
