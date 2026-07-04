// Package updater gives D BOX a serverless self-update: on start (and on demand)
// it reads a small static latest.json over HTTPS, compares versions, and — one
// click — downloads the new executable and swaps it in place, then relaunches.
//
// No backend is required. latest.json and the binary live on any static host
// (GitHub Releases + Pages, Netlify, R2…). The "replace a running exe" problem
// is solved the Windows way: a running .exe can be RENAMED (just not deleted or
// overwritten), so we move the current exe aside to <exe>.old, drop the new one
// in its place, then spawn a tiny detached relauncher that waits for this
// process to exit (freeing the port) and starts the new build.
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Release is one entry of latest.json.
type Release struct {
	Version      string `json:"version"`                // "1.2.0"
	URL          string `json:"url"`                    // direct download of the new executable
	InstallerURL string `json:"installerUrl,omitempty"` // full installer (website download; optional here)
	SHA256       string `json:"sha256,omitempty"`       // hex digest of the executable at URL (verified)
	Notes        string `json:"notes,omitempty"`        // short "what's new"
	Date         string `json:"date,omitempty"`         // release date, display-only
	Mandatory    bool   `json:"mandatory,omitempty"`    // UI can hide "Later" for a forced update
}

// Fetch retrieves and decodes latest.json.
func Fetch(ctx context.Context, manifestURL, userAgent string) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest HTTP %d", resp.StatusCode)
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if strings.TrimSpace(rel.Version) == "" || strings.TrimSpace(rel.URL) == "" {
		return nil, fmt.Errorf("manifest missing version/url")
	}
	return &rel, nil
}

// Newer reports whether remote is a strictly higher version than current.
// Both are dotted numeric ("1.10.2"); a leading "v" and any "-prerelease"
// suffix are ignored. An unparseable current (e.g. "dev") is treated as oldest.
func Newer(remote, current string) bool {
	return compare(remote, current) > 0
}

func compare(a, b string) int {
	pa, pb := parseVer(a), parseVer(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseVer(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 { // drop pre-release / build metadata
		v = v[:i]
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return out // "dev" / garbage -> whatever parsed so far (usually empty = oldest)
		}
		out = append(out, n)
	}
	return out
}

// Progress is a snapshot of an in-flight Apply, polled by the UI.
type Progress struct {
	Phase      string `json:"phase"` // downloading | verifying | swapping | restarting | error | done
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Error      string `json:"error,omitempty"`
}

// progressWriter counts bytes and reports (downloaded, total) through cb.
type progressWriter struct {
	n     int64
	total int64
	cb    func(downloaded, total int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	if w.cb != nil {
		w.cb(w.n, w.total)
	}
	return len(p), nil
}

// Apply downloads rel's executable next to the current one, verifies it,
// atomically swaps it in for the running exe, then spawns a detached relauncher
// and returns. The caller must then exit the process (the relauncher waits for
// that, frees the port, and starts the new build). progress is called with each
// phase/byte-count update; it may be nil.
//
// On any failure BEFORE the swap, the current exe is untouched. If the byte
// swap itself half-completes, the original is restored from <exe>.old.
func Apply(ctx context.Context, rel *Release, userAgent string, progress func(Progress)) error {
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}
	self, err := currentExe()
	if err != nil {
		return err
	}
	dir := filepath.Dir(self)

	// Same-directory temp file so the final rename is a cheap same-volume move.
	tmp := filepath.Join(dir, ".dbox-update.download")
	_ = os.Remove(tmp)

	report(Progress{Phase: "downloading"})
	sum, _, err := download(ctx, rel.URL, tmp, userAgent, func(n, total int64) {
		report(Progress{Phase: "downloading", Downloaded: n, Total: total})
	})
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("download: %w", err)
	}

	// Verify integrity when the manifest pins a digest — never run an executable
	// that didn't match what the release advertised.
	if want := strings.ToLower(strings.TrimSpace(rel.SHA256)); want != "" {
		report(Progress{Phase: "verifying"})
		if got := hex.EncodeToString(sum); !strings.EqualFold(got, want) {
			os.Remove(tmp)
			return fmt.Errorf("checksum mismatch: got %s want %s", got, want)
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		// non-fatal on Windows; keep going
		_ = err
	}

	report(Progress{Phase: "swapping"})
	old := self + ".old"
	_ = os.Remove(old) // clear a leftover from a prior update
	if err := os.Rename(self, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("set aside running exe (need write access to %s): %w", dir, err)
	}
	if err := os.Rename(tmp, self); err != nil {
		// Roll back so the app still launches next time.
		if rbErr := os.Rename(old, self); rbErr != nil {
			return fmt.Errorf("install new exe failed (%v) AND rollback failed (%v) — reinstall from the website", err, rbErr)
		}
		os.Remove(tmp)
		return fmt.Errorf("install new exe: %w", err)
	}

	report(Progress{Phase: "restarting"})
	// Relaunch with the SAME args this process was started with, so an installed
	// app (launched with none) restarts clean and a custom launch is preserved.
	if err := relaunch(self, os.Args[1:]); err != nil {
		return fmt.Errorf("schedule relaunch: %w", err)
	}
	report(Progress{Phase: "done"})
	return nil
}

// download streams url to dst, returning the sha256 of the bytes written and the
// total size. Reports (downloaded, total) via cb.
func download(ctx context.Context, url, dst, userAgent string, cb func(downloaded, total int64)) ([]byte, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return nil, 0, err
	}
	h := sha256.New()
	pw := &progressWriter{total: resp.ContentLength, cb: cb}
	n, err := io.Copy(io.MultiWriter(f, h, pw), resp.Body)
	closeErr := f.Close()
	if err != nil {
		return nil, 0, err
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return nil, 0, fmt.Errorf("short read: %d of %d bytes", n, resp.ContentLength)
	}
	return h.Sum(nil), n, nil
}

// CleanupOld removes the <exe>.old left by a previous successful update. Safe to
// call on every startup; a still-locked .old (rare) just lingers to next launch.
func CleanupOld() {
	self, err := currentExe()
	if err != nil {
		return
	}
	old := self + ".old"
	if _, err := os.Stat(old); err == nil {
		// A brief retry: the just-replaced predecessor may hold the handle for a
		// moment right after relaunch.
		for i := 0; i < 5; i++ {
			if os.Remove(old) == nil {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func currentExe() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	return self, nil
}
