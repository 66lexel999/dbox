package engine

// Burst downloader for googlevideo (YouTube) media URLs.
//
// googlevideo throttles each HTTP request to ~playback speed AFTER an initial
// fast "burst" window of roughly the first few MB. A conventional downloader
// (aria2c -x16) opens a handful of connections and streams one large continuous
// range down each, so every connection bursts once and then crawls — capping a
// 100 MB/s link at ~12 MB/s. The fix, measured at ~82 MB/s on the same link, is
// to issue MANY small range requests in parallel, each a FRESH connection so it
// gets its own burst and finishes before the throttle engages. This is the
// technique IDM uses. KEY: it relies on the HTTP Range *header*, not the
// googlevideo `&range=` URL parameter (which is throttled harder).

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// burstChunkSize must stay inside googlevideo's burst window; 5 MiB measured
	// markedly faster than 10 MiB (10 starts hitting the throttle mid-chunk).
	burstChunkSize = 5 << 20
	// burstConns parallel range requests. 32 ~saturated a 70 MB/s link in
	// testing (16→51 MB/s, 32→61–82 MB/s). aria2c's per-server cap is 16; our
	// own engine has no such limit, which is the whole point.
	burstConns = 32
	// burstChunkRetries per chunk before the whole fetch fails over to yt-dlp.
	burstChunkRetries = 3
)

// isGoogleVideo reports whether a resolved media URL is a googlevideo host (the
// only place the burst trick applies; other CDNs aren't throttled this way).
func isGoogleVideo(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.Contains(u.Host, "googlevideo.com")
}

// burstProgress is a persisted bitmap of completed burst chunks for one stream,
// written next to the .part file (a ".prog" sidecar) so a paused or interrupted
// video download RESUMES instead of restarting. One bit per burstChunkSize chunk;
// the leading clen guards against the resolved stream changing size between
// sessions (a quality/format change → the stale bitmap is ignored, that stream
// restarts). Burst writes chunks out of order in parallel, so a high-water mark
// won't do — we need to know exactly which chunks already landed.
type burstProgress struct {
	path    string
	clen    int64
	nChunks int
	mu      sync.Mutex
	bits    []byte
}

func progPath(dest string) string { return dest + ".prog" }

// loadBurstProgress reads dest's sidecar bitmap. A missing, short, or
// size-mismatched file yields an all-zero (fresh) bitmap, so a corrupt or stale
// sidecar just means the download starts over rather than corrupting.
func loadBurstProgress(dest string, clen int64) *burstProgress {
	n := int((clen + burstChunkSize - 1) / burstChunkSize)
	bp := &burstProgress{path: progPath(dest), clen: clen, nChunks: n, bits: make([]byte, (n+7)/8)}
	b, err := os.ReadFile(bp.path)
	if err != nil || len(b) < 8 || int64(binary.LittleEndian.Uint64(b[:8])) != clen {
		return bp
	}
	copy(bp.bits, b[8:])
	return bp
}

func (bp *burstProgress) isDone(i int) bool {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return bp.bits[i/8]&(1<<(uint(i)%8)) != 0
}

func (bp *burstProgress) markDone(i int) {
	bp.mu.Lock()
	bp.bits[i/8] |= 1 << (uint(i) % 8)
	bp.mu.Unlock()
}

// doneBytes is how many bytes the already-completed chunks account for, used to
// seed a resumed download's progress so the row picks up where it left off.
func (bp *burstProgress) doneBytes() int64 {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	var total int64
	for i := 0; i < bp.nChunks; i++ {
		if bp.bits[i/8]&(1<<(uint(i)%8)) != 0 {
			end := int64(i+1) * burstChunkSize
			if end > bp.clen {
				end = bp.clen
			}
			total += end - int64(i)*burstChunkSize
		}
	}
	return total
}

// flush persists the bitmap via a temp file + rename so a crash mid-write can't
// leave a torn sidecar that spuriously marks a chunk done (which would leave a
// hole in the file). Best-effort: an I/O error just costs some re-downloading.
func (bp *burstProgress) flush() {
	bp.mu.Lock()
	buf := make([]byte, 8+len(bp.bits))
	binary.LittleEndian.PutUint64(buf[:8], uint64(bp.clen))
	copy(buf[8:], bp.bits)
	bp.mu.Unlock()
	tmp := bp.path + ".tmp"
	if os.WriteFile(tmp, buf, 0o644) == nil {
		if os.Rename(tmp, bp.path) != nil {
			os.Remove(tmp)
		}
	}
}

func (bp *burstProgress) remove() { os.Remove(bp.path); os.Remove(bp.path + ".tmp") }

// burstFetch downloads rawURL to dest using many small parallel range requests.
// onBytes is called with the byte count of each completed chunk (for progress),
// plus once up front with any bytes already on disk from a previous (paused)
// session. Completed chunks are tracked in a ".prog" sidecar so a re-run fetches
// only what's missing instead of starting over. Honors ctx cancellation
// (pause/delete/shutdown), leaving the .part + .prog in place for the caller.
func (e *Engine) burstFetch(ctx context.Context, rawURL, dest, ua string, onBytes func(int64)) error {
	clen, err := e.burstProbeLen(ctx, rawURL, ua)
	if err != nil {
		return fmt.Errorf("probe length: %w", err)
	}
	if clen <= 0 {
		return fmt.Errorf("server did not report content length")
	}

	f, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(clen); err != nil { // preallocate for concurrent WriteAt; keeps existing bytes
		f.Close()
		os.Remove(dest)
		return err
	}

	prog := loadBurstProgress(dest, clen)
	if resumed := prog.doneBytes(); resumed > 0 && onBytes != nil {
		onBytes(resumed) // seed the progress bar with what a previous session fetched
	}

	// Persist the chunk bitmap once a second so an abrupt kill loses ~1s at most.
	flushStop := make(chan struct{})
	var flushWg sync.WaitGroup
	flushWg.Add(1)
	go func() {
		defer flushWg.Done()
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-flushStop:
				return
			case <-tk.C:
				prog.flush()
			}
		}
	}()

	type chunk struct {
		idx        int
		start, end int64
	}
	jobs := make(chan chunk)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
		cancel() // stop the other workers and the feeder
	}

	for i := 0; i < burstConns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				n, err := e.burstChunk(wctx, rawURL, ua, j.start, j.end, f)
				if err != nil {
					if wctx.Err() == nil {
						fail(err)
					}
					return
				}
				prog.markDone(j.idx)
				if onBytes != nil {
					onBytes(n)
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		idx := 0
		for off := int64(0); off < clen; off += burstChunkSize {
			end := off + burstChunkSize - 1
			if end >= clen {
				end = clen - 1
			}
			if !prog.isDone(idx) { // skip chunks a previous session already wrote
				select {
				case jobs <- chunk{idx, off, end}:
				case <-wctx.Done():
					return
				}
			}
			idx++
		}
	}()

	wg.Wait()
	close(flushStop)
	flushWg.Wait()
	prog.flush() // capture every chunk finished this run
	syncErr := f.Sync()
	f.Close()

	errMu.Lock()
	fe := firstErr
	errMu.Unlock()
	switch {
	case ctx.Err() != nil: // paused / deleted / shutdown — keep .part + .prog for resume
		return ctx.Err()
	case fe != nil:
		os.Remove(dest)
		prog.remove()
		return fe
	case syncErr != nil:
		os.Remove(dest)
		prog.remove()
		return syncErr
	}
	// Fully fetched. Keep the .prog (all bits set) until the WHOLE task finishes:
	// for a 2-stream video this stream may complete while the other is still going
	// and the task could pause in between, so the bitmap lets a resume recognize
	// this stream as done rather than re-fetching it. The caller drops the
	// sidecars on final success / delete.
	return nil
}

// burstProbeLen learns the full size via a 1-byte ranged GET (Content-Range
// "bytes 0-0/TOTAL"). googlevideo honors this and it doubles as a liveness check
// on the (short-lived) resolved URL.
func (e *Engine) burstProbeLen(ctx context.Context, rawURL, ua string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Close = true
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range not honored (HTTP %d)", resp.StatusCode)
	}
	cr := resp.Header.Get("Content-Range")
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		if v, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	return 0, fmt.Errorf("no Content-Range total")
}

// burstChunk fetches one [start,end] range, retrying a few times on transient
// failure. Each attempt is a fresh connection (req.Close) so googlevideo serves
// it from a new burst window.
func (e *Engine) burstChunk(ctx context.Context, rawURL, ua string, start, end int64, f *os.File) (int64, error) {
	want := end - start + 1
	var lastErr error
	for attempt := 0; attempt < burstChunkRetries; attempt++ {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		n, err := e.burstChunkOnce(ctx, rawURL, ua, start, end, f)
		if err == nil && n == want {
			return n, nil
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("short chunk at %d: got %d of %d", start, n, want)
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return 0, lastErr
}

func (e *Engine) burstChunkOnce(ctx context.Context, rawURL, ua string, start, end int64, f *os.File) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Close = true // fresh connection per chunk = a fresh googlevideo burst window
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("range request at %d: HTTP %d", start, resp.StatusCode)
	}
	buf := make([]byte, end-start+1)
	n, err := io.ReadFull(resp.Body, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil // a final short read is fine; we write what arrived
	}
	if err != nil {
		return 0, err
	}
	if _, err := f.WriteAt(buf[:n], start); err != nil {
		return int64(n), err
	}
	return int64(n), nil
}
