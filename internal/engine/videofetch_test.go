package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"myidm/internal/ytdlp"
)

// These hit the network and download tens to hundreds of MB; gated behind
// MYIDM_NET_TEST=1 so the normal suite stays offline and fast.
func netTest(t *testing.T) {
	t.Helper()
	if os.Getenv("MYIDM_NET_TEST") == "" {
		t.Skip("set MYIDM_NET_TEST=1 to run network integration tests")
	}
}

// testEngine builds an Engine with the same HTTP/1.1-forced, big-buffer client
// the production New() uses, which is what gives one socket per range request.
func testEngine() *Engine {
	return &Engine{client: &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   false,
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
			ReadBufferSize:      256 << 10,
			WriteBufferSize:     128 << 10,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
		},
	}}
}

func sha256File(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestBurstFetchIntegrity proves the parallel chunked assembly is byte-exact:
// burstFetch's output must hash-match a plain single-stream download of the same
// URL. Uses a fast, un-throttled Google CDN file so any size mismatch is a bug
// in our offset/WriteAt logic, not server throttling.
func TestBurstFetchIntegrity(t *testing.T) {
	netTest(t)
	const url = "https://dl.google.com/go/go1.23.0.windows-amd64.zip"
	e := testEngine()
	dir := t.TempDir()

	// reference: plain sequential download
	ref := filepath.Join(dir, "ref.zip")
	resp, err := e.client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := os.Create(ref)
	io.Copy(rf, resp.Body)
	rf.Close()
	resp.Body.Close()
	refSum := sha256File(t, ref)

	// burst download
	out := filepath.Join(dir, "burst.zip")
	var got int64
	if err := e.burstFetch(context.Background(), url, out, "", func(n int64) { atomic.AddInt64(&got, n) }); err != nil {
		t.Fatalf("burstFetch: %v", err)
	}
	if s := sha256File(t, out); s != refSum {
		t.Fatalf("sha mismatch: burst=%s ref=%s", s, refSum)
	}
	fi, _ := os.Stat(out)
	if got != fi.Size() {
		t.Fatalf("progress accounting off: onBytes total=%d file=%d", got, fi.Size())
	}
	t.Logf("integrity OK: %d bytes, sha %s", fi.Size(), refSum)
}

// TestBurstFetchYouTubeSpeed resolves a real YouTube format and downloads it via
// burstFetch, asserting the burst technique clears googlevideo's throttle
// (baseline yt-dlp+aria2c is ~12 MB/s; we expect well above that).
func TestBurstFetchYouTubeSpeed(t *testing.T) {
	netTest(t)
	ytdlp.SetSearchDirs(`C:\Users\alfat\Downloads\MyIDM\Programs`)
	if !ytdlp.Available() {
		t.Skip("yt-dlp not found")
	}
	ctx := context.Background()
	urls, err := ytdlp.ResolveURLs(ctx, "https://www.youtube.com/watch?v=aqz-KE-bpKQ", "299", "")
	if err != nil || len(urls) == 0 {
		t.Fatalf("resolve: %v", err)
	}
	e := testEngine()
	out := filepath.Join(t.TempDir(), "v.mp4")
	var got int64
	start := time.Now()
	if err := e.burstFetch(ctx, urls[0], out, "", func(n int64) { atomic.AddInt64(&got, n) }); err != nil {
		t.Fatalf("burstFetch: %v", err)
	}
	mbps := float64(got) / (1 << 20) / time.Since(start).Seconds()
	t.Logf("burst YouTube: %.0f MB in %s = %.1f MiB/s", float64(got)/(1<<20), time.Since(start).Round(time.Millisecond), mbps)
	if mbps < 25 {
		t.Fatalf("burst no faster than throttle: %.1f MiB/s", mbps)
	}
}

// TestBurstFetchResume proves a paused burst download CONTINUES instead of
// restarting: it pre-seeds one finished chunk + its .prog sidecar, then asserts
// the resumed fetch skips that chunk, fetches only the missing ones, seeds
// progress with the already-done bytes, and assembles the byte-exact file.
// Offline — a local range server stands in for googlevideo.
func TestBurstFetchResume(t *testing.T) {
	size := burstChunkSize*2 + burstChunkSize/2 // 2.5 chunks -> 3 chunks (last is partial)
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*7 + 1)
	}

	var served sync.Map // start offset -> true, for real range serves (not the 0-0 probe)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		if !(start == 0 && end == 0) {
			served.Store(start, true)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[start : end+1])
	}))
	defer srv.Close()

	e := testEngine()
	dest := filepath.Join(t.TempDir(), "v.bin")

	// Simulate a previous session that preallocated the file and finished chunk 0.
	if err := os.WriteFile(dest, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(dest, os.O_RDWR, 0o644)
	f.WriteAt(body[:burstChunkSize], 0)
	f.Close()
	prog := loadBurstProgress(dest, int64(size))
	prog.markDone(0)
	prog.flush()

	var got int64
	if err := e.burstFetch(context.Background(), srv.URL, dest, "", func(n int64) { atomic.AddInt64(&got, n) }); err != nil {
		t.Fatalf("burstFetch resume: %v", err)
	}

	if _, hit := served.Load(int64(0)); hit {
		t.Fatal("resume re-downloaded chunk 0 instead of skipping it")
	}
	if _, hit := served.Load(int64(burstChunkSize)); !hit {
		t.Fatalf("resume did not fetch the missing chunk at offset %d", burstChunkSize)
	}
	out, _ := os.ReadFile(dest)
	if !bytes.Equal(out, body) {
		t.Fatalf("resumed file mismatch (len got=%d want=%d)", len(out), len(body))
	}
	if got != int64(size) {
		t.Fatalf("progress total=%d, want %d (seed for chunk 0 + fetched chunks)", got, size)
	}
}
