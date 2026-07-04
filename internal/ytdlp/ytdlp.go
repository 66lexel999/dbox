// Package ytdlp wraps the external yt-dlp binary so MyIDM can download
// streaming-site media (YouTube, etc.) that its single-file segmented engine
// can't handle. yt-dlp (and ffmpeg, for muxing) are optional: if absent, only
// direct-file downloads work and video probes return ErrNotInstalled.
package ytdlp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"myidm/internal/procutil"
)

var ErrNotInstalled = errors.New(
	"yt-dlp not found - put yt-dlp.exe next to DBox.exe or on PATH")

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// searchDirs are extra roots to look in (set by the engine to the download dir),
// so dropping yt-dlp.exe / an extracted ffmpeg build into MyIDM's Programs
// folder just works.
var searchDirs []string

// SetSearchDirs registers extra directories to scan for the binaries.
func SetSearchDirs(dirs ...string) { searchDirs = append([]string(nil), dirs...) }

func statFile(p string) string {
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

func firstGlob(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		if p := statFile(m); p != "" {
			return p
		}
	}
	return ""
}

// find looks next to the MyIDM executable, then the configured download dir
// (incl. its Programs category and one level of subfolders, e.g. an extracted
// ffmpeg-*/bin/ffmpeg.exe), then PATH.
func find(name string) string {
	n := exeName(name)
	var roots []string
	if self, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(self))
	}
	roots = append(roots, searchDirs...)
	for _, root := range roots {
		if root == "" {
			continue
		}
		if p := statFile(filepath.Join(root, n)); p != "" {
			return p
		}
		for _, pat := range []string{
			filepath.Join(root, "*", n),
			filepath.Join(root, "*", "bin", n),
			filepath.Join(root, "Programs", n),
			filepath.Join(root, "Programs", "*", n),
			filepath.Join(root, "Programs", "*", "bin", n),
		} {
			if p := firstGlob(pat); p != "" {
				return p
			}
		}
	}
	if p, err := exec.LookPath(n); err == nil {
		return p
	}
	return ""
}

func Locate() string     { return find("yt-dlp") }
func ffmpegPath() string { return find("ffmpeg") }
func Available() bool    { return Locate() != "" }

// ---- browser cookies (for login-gated sites: Instagram stories/reels, etc.) --
//
// yt-dlp's own --cookies-from-browser can't read a Chromium browser's cookie DB
// while that browser is RUNNING (the file is exclusively locked on Windows —
// yt-dlp #7271), which is the common case. So instead the browser EXTENSION,
// which already holds the live session, reads the cookies via chrome.cookies and
// posts them here; we write a Netscape cookies.txt per registrable domain and
// hand yt-dlp --cookies <file> for matching URLs. No browser cooperation needed.

var (
	cookiesMu  sync.RWMutex
	cookiesDir string
)

// SetCookiesDir sets where per-domain cookies.txt files live (created on demand).
func SetCookiesDir(dir string) {
	cookiesMu.Lock()
	cookiesDir = dir
	cookiesMu.Unlock()
}

// Cookie mirrors the fields chrome.cookies.getAll returns that matter for a
// Netscape cookie jar.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	HostOnly bool    `json:"hostOnly"`
	Expiry   float64 `json:"expiry"` // epoch seconds; <=0 means a session cookie
}

// multipartTLDs are second-level labels that are effectively part of the TLD, so
// the registrable domain is the last THREE labels (e.g. bbc.co.uk), not two.
var multipartTLDs = map[string]bool{
	"co": true, "com": true, "org": true, "net": true, "gov": true, "edu": true, "ac": true,
}

// baseDomain reduces a host to its registrable domain (instagram.com,
// bbc.co.uk) so one cookie jar covers www./m./i. subdomains alike. Leading dots
// (from cookie domains like ".instagram.com") are trimmed first.
func baseDomain(host string) string {
	host = strings.Trim(strings.ToLower(host), ".")
	labels := strings.Split(host, ".")
	if len(labels) <= 2 {
		return host
	}
	n := 2
	if multipartTLDs[labels[len(labels)-2]] {
		n = 3
	}
	return strings.Join(labels[len(labels)-n:], ".")
}

func cookieFilePath(domain string) string {
	cookiesMu.RLock()
	dir := cookiesDir
	cookiesMu.RUnlock()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, baseDomain(domain)+".txt")
}

// WriteCookies persists a Netscape cookie jar for the URL's registrable domain,
// replacing any prior jar for it. A nil/empty list removes the jar.
func WriteCookies(rawURL string, cookies []Cookie) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid url")
	}
	path := cookieFilePath(u.Hostname())
	if path == "" {
		return fmt.Errorf("cookies dir not configured")
	}
	if len(cookies) == 0 {
		os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n# Written by D BOX from the browser extension.\n\n")
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		dom := c.Domain
		if dom == "" {
			dom = u.Hostname()
		}
		flag := "TRUE" // include-subdomains: true for a .domain cookie
		if c.HostOnly {
			flag = "FALSE"
		}
		// httpOnly cookies (e.g. Instagram's sessionid) are marked with yt-dlp's
		// #HttpOnly_ prefix — without it the parser drops the auth cookie.
		if c.HTTPOnly {
			dom = "#HttpOnly_" + dom
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		exp := int64(0)
		if c.Expiry > 0 {
			exp = int64(c.Expiry)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", dom, flag, path, secure, exp, c.Name, c.Value)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cookieArgsFor returns --cookies <file> when a cookie jar exists for the URL's
// domain, else nil (yt-dlp runs anonymously, as before).
func cookieArgsFor(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil
	}
	path := cookieFilePath(u.Hostname())
	if path == "" || statFile(path) == "" {
		return nil
	}
	return []string{"--cookies", path}
}

// HasCookiesFor reports whether a cookie jar is on disk for the URL's domain.
func HasCookiesFor(rawURL string) bool { return len(cookieArgsFor(rawURL)) > 0 }

// RegistrableDomain returns a URL's cookie-jar domain (instagram.com for
// www.instagram.com), or "" if it can't be parsed. Exported so callers can match
// cookie scope — e.g. invalidating cached probes for the same site.
func RegistrableDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return baseDomain(u.Hostname())
}

// aria2Win64URL is aria2's official Windows 64-bit release. aria2 releases
// rarely (1.37.0 is the current stable), so a pinned URL is stable enough; a
// 404 later just logs and leaves yt-dlp on its single-connection downloader.
const aria2Win64URL = "https://github.com/aria2/aria2/releases/download/release-1.37.0/aria2-1.37.0-win-64bit-build1.zip"

// EnsureAria2c makes aria2c available so yt-dlp can use it as a multi-connection
// external downloader (YouTube throttles each connection, so a single stream
// crawls; aria2c opens many and multiplies the throughput). If aria2c isn't
// already found, it downloads the official Windows build into dir and extracts
// aria2c.exe there. Returns the path, or "" if it can't be provided.
func EnsureAria2c(ctx context.Context, dir string) (string, error) {
	if p := find("aria2c"); p != "" {
		return p, nil // already installed somewhere we look
	}
	if runtime.GOOS != "windows" {
		return "", nil // only the Windows build is auto-provisioned
	}
	dest := filepath.Join(dir, "aria2c.exe")
	if statFile(dest) != "" {
		return dest, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aria2Win64URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download aria2: HTTP %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return "", fmt.Errorf("open aria2 archive: %w", err)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(filepath.Base(f.Name), "aria2c.exe") {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		tmp := dest + ".tmp"
		out, err := os.Create(tmp)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, copyErr := io.Copy(out, rc) // archive/zip verifies the CRC as it reads
		rc.Close()
		out.Close()
		if copyErr != nil {
			os.Remove(tmp)
			return "", copyErr
		}
		if err := os.Rename(tmp, dest); err != nil {
			os.Remove(tmp)
			return "", err
		}
		return dest, nil
	}
	return "", fmt.Errorf("aria2c.exe not found in archive")
}

// SelfUpdate upgrades yt-dlp to the latest release (`yt-dlp -U`). YouTube changes
// its player/signature ("n"/nsig) scheme constantly; an out-of-date yt-dlp can't
// solve it and is handed a THROTTLED media URL (~hundreds of KB/s per connection
// — the reason even aria2c tops out at a few MB/s), while a current one gets the
// full-speed URL. Keeping yt-dlp current is the single biggest speed factor.
// Returns yt-dlp's output; a package-managed install just prints a notice.
func SelfUpdate(ctx context.Context) (string, error) {
	bin := Locate()
	if bin == "" {
		return "", ErrNotInstalled
	}
	cmd := exec.CommandContext(ctx, bin, "-U")
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Format is the subset of yt-dlp's -J output we care about.
type Format struct {
	ID             string  `json:"format_id"`
	Ext            string  `json:"ext"`
	Height         int     `json:"height"`
	FPS            float64 `json:"fps"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	FileSize       int64   `json:"filesize"`
	FileSizeApprox int64   `json:"filesize_approx"`
	TBR            float64 `json:"tbr"` // total bitrate (kbps), for size estimation
}

type info struct {
	Title    string   `json:"title"`
	Duration float64  `json:"duration"` // seconds, for size estimation
	Formats  []Format `json:"formats"`
}

// formatSize returns a format's byte size: its reported filesize, else an
// estimate from total bitrate × duration (YouTube often omits filesize on
// adaptive formats but reports tbr).
func formatSize(f Format, durationSec float64) int64 {
	if f.FileSize > 0 {
		return f.FileSize
	}
	if f.FileSizeApprox > 0 {
		return f.FileSizeApprox
	}
	if f.TBR > 0 && durationSec > 0 {
		return int64(f.TBR * 1000 / 8 * durationSec) // kbps -> bytes
	}
	return 0
}

func isAudioOnly(f Format) bool {
	return (f.ACodec != "" && f.ACodec != "none") && (f.VCodec == "" || f.VCodec == "none")
}

// DownOption is one entry in the overlay's quality dropdown.
type DownOption struct {
	Label    string `json:"label"`    // "MP4 - 1080p 60fps (~335 MB)"
	Selector string `json:"selector"` // yt-dlp -f selector
	Ext      string `json:"ext"`      // expected container
	Audio    bool   `json:"audio"`    // audio-only (extract to mp3)
	Size     int64  `json:"size"`
}

// ProbeResult summarises what's downloadable at a URL for the UI.
type ProbeResult struct {
	Title   string       `json:"title"`
	Options []DownOption `json:"options"`
}

// Probe runs `yt-dlp -J` and distils the formats into quality options.
func Probe(ctx context.Context, url, userAgent string) (*ProbeResult, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{"-J", "--no-warnings", "--no-playlist"}
	if userAgent != "" {
		args = append(args, "--user-agent", userAgent)
	}
	args = append(args, cookieArgsFor(url)...) // login cookies for IG/etc. when the extension supplied them
	args = append(args, url)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd) // no flashing cmd window under a -H windowsgui parent
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s", ytdlpErr(err))
	}
	var in info
	if err := json.Unmarshal(out, &in); err != nil {
		return nil, fmt.Errorf("parse yt-dlp output: %w", err)
	}
	return distil(&in), nil
}

// ytdlpErr turns a failed `cmd.Output()` error into the useful message: yt-dlp
// writes the real reason (e.g. Instagram "login required", "private", "rate
// limit") to STDERR, which cmd.Output() stashes on *ExitError.Stderr — otherwise
// all the caller sees is "exit status 1".
func ytdlpErr(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		lines := strings.Split(strings.TrimSpace(string(ee.Stderr)), "\n")
		for i := len(lines) - 1; i >= 0; i-- { // the last ERROR line is the actionable one
			l := strings.TrimSpace(lines[i])
			if l == "" {
				continue
			}
			l = strings.TrimPrefix(l, "ERROR: ")
			if len(l) > 240 {
				l = l[:240]
			}
			return l
		}
	}
	return err.Error()
}

// PlaylistEntry is one video in a playlist (resolved lazily at download time).
type PlaylistEntry struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// PlaylistInfo is a playlist's title + its videos, enumerated without download.
type PlaylistInfo struct {
	Title   string          `json:"title"`
	Entries []PlaylistEntry `json:"entries"`
	Mix     bool            `json:"mix"` // a YouTube Mix/radio (endless) — the count was capped
}

// mixCap limits how many videos we pull from a YouTube Mix. Mixes (list=RD…) are
// endless auto-radios — yt-dlp will otherwise follow them for hundreds of
// videos (a different count every run) — so we take only the leading queue,
// which is about the size YouTube shows in the mix panel (~25).
const mixCap = 25

// isMixURL reports whether a URL is a YouTube Mix/radio (its list= id starts
// with RD), which has no fixed size and must be capped.
func isMixURL(u string) bool {
	return strings.Contains(u, "?list=RD") || strings.Contains(u, "&list=RD")
}

type flatEntry struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
}
type flatPlaylist struct {
	Type    string      `json:"_type"`
	Title   string      `json:"title"`
	Entries []flatEntry `json:"entries"`
}

// ProbePlaylist lists a playlist's videos WITHOUT downloading them, using
// `yt-dlp --flat-playlist -J` (fast — it doesn't fetch each video's formats).
// Returns nil (not an error) when the URL isn't a playlist, so callers can fall
// back to the single-video path.
func ProbePlaylist(ctx context.Context, url, userAgent string) (*PlaylistInfo, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{"--flat-playlist", "-J", "--no-warnings"}
	mix := isMixURL(url)
	if mix {
		args = append(args, "--playlist-end", fmt.Sprint(mixCap)) // endless radio — take the leading queue only
	}
	if userAgent != "" {
		args = append(args, "--user-agent", userAgent)
	}
	args = append(args, url)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp playlist probe failed: %v", err)
	}
	var fp flatPlaylist
	if err := json.Unmarshal(out, &fp); err != nil {
		return nil, fmt.Errorf("parse yt-dlp playlist: %w", err)
	}
	if fp.Type != "playlist" || len(fp.Entries) == 0 {
		return nil, nil // a single video, not a playlist
	}
	pl := &PlaylistInfo{Title: strings.TrimSpace(fp.Title), Mix: mix}
	seen := map[string]bool{}
	for _, e := range fp.Entries {
		u := strings.TrimSpace(e.URL)
		if !strings.HasPrefix(u, "http") { // flat entries often give a bare video ID
			id := e.ID
			if id == "" {
				id = u
			}
			if id == "" {
				continue
			}
			u = "https://www.youtube.com/watch?v=" + id
		}
		key := u
		if e.ID != "" { // dedup on the video ID (radio/mix playlists can repeat videos)
			key = e.ID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		title := strings.TrimSpace(e.Title)
		if title == "" {
			title = "video"
		}
		pl.Entries = append(pl.Entries, PlaylistEntry{URL: u, Title: title})
	}
	if len(pl.Entries) == 0 {
		return nil, nil
	}
	return pl, nil
}

func distil(in *info) *ProbeResult {
	dur := in.Duration
	// Largest audio stream — muxed into video-only (adaptive) formats, so its
	// size has to be added to theirs for an accurate total.
	var audioSize int64
	for _, f := range in.Formats {
		if isAudioOnly(f) {
			if s := formatSize(f, dur); s > audioSize {
				audioSize = s
			}
		}
	}

	heightSize := map[int]int64{}
	heightFps := map[int]float64{}
	for _, f := range in.Formats {
		if f.Height <= 0 || f.VCodec == "" || f.VCodec == "none" {
			continue
		}
		sz := formatSize(f, dur)
		if sz > 0 && (f.ACodec == "" || f.ACodec == "none") {
			sz += audioSize // video-only stream: add the audio we'll mux in
		}
		if sz > heightSize[f.Height] {
			heightSize[f.Height] = sz
		}
		if f.FPS > heightFps[f.Height] {
			heightFps[f.Height] = f.FPS
		}
	}
	var bestSize int64
	for _, s := range heightSize {
		if s > bestSize {
			bestSize = s
		}
	}

	res := &ProbeResult{Title: in.Title}
	best := DownOption{Label: "Best quality (video + audio)", Selector: "bv*+ba/b", Ext: "mp4", Size: bestSize}
	if bestSize > 0 {
		best.Label += fmt.Sprintf(" (~%s)", human(bestSize))
	}
	res.Options = append(res.Options, best)

	heights := make([]int, 0, len(heightSize))
	for h := range heightSize {
		heights = append(heights, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(heights)))
	for _, h := range heights {
		label := fmt.Sprintf("MP4 - %dp", h)
		if heightFps[h] >= 50 {
			label += fmt.Sprintf(" %dfps", int(heightFps[h]+0.5))
		}
		if sz := heightSize[h]; sz > 0 {
			label += fmt.Sprintf(" (~%s)", human(sz))
		}
		res.Options = append(res.Options, DownOption{
			Label: label,
			// Prefer H.264 (avc1) at the chosen height: it's the largest format (so
			// the downloaded size matches the "~size" shown, which is the max at that
			// height) and the most compatible for an MP4. Fall back to any codec, then
			// to a progressive combined stream, if no avc1 exists (e.g. >1080p).
			Selector: fmt.Sprintf("bv*[height<=%d][vcodec^=avc1]+ba/bv*[height<=%d]+ba/b[height<=%d]", h, h, h),
			Ext:      "mp4",
			Size:     heightSize[h],
		})
	}
	audio := DownOption{Label: "Audio only (MP3)", Selector: "ba/bestaudio", Ext: "mp3", Audio: true, Size: audioSize}
	if audioSize > 0 {
		audio.Label += fmt.Sprintf(" (~%s)", human(audioSize))
	}
	res.Options = append(res.Options, audio)
	return res
}

// DownloadCmd builds the yt-dlp command for a task. outTemplate should be an
// -o template like "<dir>/<title>.%(ext)s". Resumable via --continue.
func DownloadCmd(ctx context.Context, url, selector, outTemplate, userAgent string, audio bool, concurrency int) *exec.Cmd {
	if concurrency < 1 {
		concurrency = 4
	}
	args := []string{
		"--no-warnings", "--no-playlist", "--newline", "--continue",
		// Default player-client set (matches Probe/ResolveURLs) so the chosen format
		// — e.g. 1080p — is honored; the android-only override capped quality at 360p.
		"-f", selector, "-o", outTemplate,
	}
	if ff := ffmpegPath(); ff != "" {
		args = append(args, "--ffmpeg-location", ff)
	}
	// YouTube throttles each media connection to roughly playback speed, so a
	// single stream crawls at a few hundred KB/s no matter how fast the link is.
	// Pull many pieces at once to multiply throughput: aria2c (multi-connection
	// per fragment) when it's available, else yt-dlp's own concurrent-fragment
	// downloader plus chunked HTTP (which also resets the throttle per chunk).
	if find("aria2c") != "" {
		// YouTube throttles each connection, so use aria2c's max (16 connections
		// per server, 16 splits) to multiply throughput past the throttle.
		args = append(args,
			"--downloader", "aria2c",
			// -x16/-s16: aria2c's max connections+splits per server. -k1M: small
			// pieces so all 16 connections stay busy. --file-allocation=none skips
			// the pre-allocation stall on big files; --disk-cache=64M smooths
			// high-speed writes. --summary-interval=1 emits a progress line every
			// second (default 60s) so the row shows live speed.
			"--downloader-args",
			"aria2c:-x16 -s16 -k1M --file-allocation=none --disk-cache=64M --summary-interval=1")
	} else {
		args = append(args,
			"--concurrent-fragments", strconv.Itoa(concurrency),
			"--http-chunk-size", "10M")
	}
	if audio {
		args = append(args, "-x", "--audio-format", "mp3")
	} else {
		args = append(args, "--merge-output-format", "mp4")
	}
	if userAgent != "" {
		args = append(args, "--user-agent", userAgent)
	}
	args = append(args, cookieArgsFor(url)...) // login cookies (IG stories/reels, etc.)
	args = append(args, url)
	cmd := exec.CommandContext(ctx, Locate(), args...)
	procutil.Hidden(cmd) // no flashing cmd window under a -H windowsgui parent
	// yt-dlp is a Python program: by default its stdout uses the Windows console
	// codepage, which silently drops characters it can't represent (ş, Arabic,
	// …) from the "[download] Destination:" / "[Merger]" lines we parse — even
	// though the file lands on disk with the correct Unicode name. Force Python
	// UTF-8 mode so the paths we capture match what's actually written.
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	return cmd
}

// HasFFmpeg reports whether ffmpeg is available (needed to mux the separate
// video+audio streams the burst downloader fetches).
func HasFFmpeg() bool { return ffmpegPath() != "" }

// ResolveURLs runs `yt-dlp -g` to turn a page URL + format selector into the
// direct media URL(s): one element for a progressive/combined format, two
// (video then audio) for an adaptive selection that needs muxing. These
// googlevideo URLs are what MyIDM's burst downloader then fetches itself, far
// faster than letting yt-dlp+aria2c stream them. The URLs are short-lived.
func ResolveURLs(ctx context.Context, pageURL, selector, userAgent string) ([]string, error) {
	bin := Locate()
	if bin == "" {
		return nil, ErrNotInstalled
	}
	args := []string{
		"--no-warnings", "--no-playlist", "--no-plugin-dirs",
		// Use yt-dlp's DEFAULT player-client set — the SAME one Probe uses — so the
		// format the user picked actually resolves. Forcing player_client=android was
		// ~2x faster but the android client only exposes a low progressive ladder
		// (360p max), so a "1080p" pick silently fell back to 360p (size 800MB→83MB).
		// The default auto-selects whichever client currently works as YouTube shifts.
		"-g", "-f", selector,
	}
	if userAgent != "" {
		args = append(args, "--user-agent", userAgent)
	}
	args = append(args, cookieArgsFor(pageURL)...) // login cookies (IG stories/reels, etc.)
	args = append(args, pageURL)
	cmd := exec.CommandContext(ctx, bin, args...)
	procutil.Hidden(cmd)
	cmd.Env = append(os.Environ(), "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("yt-dlp -g failed: %v", err)
	}
	var urls []string
	for _, ln := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if ln = strings.TrimSpace(ln); strings.HasPrefix(ln, "http") {
			urls = append(urls, ln)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("yt-dlp -g returned no URLs")
	}
	return urls, nil
}

// MuxCmd builds an ffmpeg command that remuxes a separate video and audio file
// into one container (stream copy, no re-encode) with the moov atom up front.
func MuxCmd(ctx context.Context, videoPath, audioPath, outPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ffmpegPath(),
		"-y", "-loglevel", "error",
		"-i", videoPath, "-i", audioPath,
		"-c", "copy", "-map", "0:v:0", "-map", "1:a:0",
		"-movflags", "+faststart", outPath)
	procutil.Hidden(cmd)
	return cmd
}

var progressRE = regexp.MustCompile(
	`\[download\]\s+([\d.]+)% of\s+~?\s*([\d.]+)(K|M|G)iB(?:\s+at\s+([\d.]+)(K|M|G)iB/s)?`)
var destRE = regexp.MustCompile(
	`(?:\[download\] Destination:|\[Merger\] Merging formats into|\[ExtractAudio\] Destination:)\s+"?(.+?)"?\s*$`)

// Progress is a parsed yt-dlp progress line.
type Progress struct {
	Percent  float64
	Total    int64 // bytes, 0 if unknown
	SpeedBPS float64
}

func unitMul(u string) float64 {
	switch u {
	case "K":
		return 1 << 10
	case "M":
		return 1 << 20
	case "G":
		return 1 << 30
	}
	return 1
}

// ParseProgress extracts percent/total/speed from a `[download] ... %` line.
func ParseProgress(line string) (Progress, bool) {
	m := progressRE.FindStringSubmatch(line)
	if m == nil {
		return Progress{}, false
	}
	var p Progress
	p.Percent, _ = strconv.ParseFloat(m[1], 64)
	if v, err := strconv.ParseFloat(m[2], 64); err == nil {
		p.Total = int64(v * unitMul(m[3]))
	}
	if m[4] != "" {
		if v, err := strconv.ParseFloat(m[4], 64); err == nil {
			p.SpeedBPS = v * unitMul(m[5])
		}
	}
	return p, true
}

// ParseDestination returns the output path yt-dlp reports for a stream/merge.
func ParseDestination(line string) (string, bool) {
	m := destRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// aria2RE matches aria2c's periodic summary line (emitted when yt-dlp uses it as
// the external downloader), e.g. "[#a1b2c3 4.5MiB/100MiB(4%) CN:16 DL:5.2MiB ETA:18s]".
var aria2RE = regexp.MustCompile(
	`([\d.]+)((?:Ki|Mi|Gi)?)B/([\d.]+)((?:Ki|Mi|Gi)?)B\((\d+)%\).*?DL:\s*([\d.]+)((?:Ki|Mi|Gi)?)B`)

// ParseAria2cProgress reads aria2c's progress so video downloads still show a
// live percentage and speed when aria2c (not yt-dlp's own downloader) is doing
// the transfer.
func ParseAria2cProgress(line string) (Progress, bool) {
	m := aria2RE.FindStringSubmatch(line)
	if m == nil {
		return Progress{}, false
	}
	pct, _ := strconv.ParseFloat(m[5], 64)
	return Progress{
		Percent:  pct,
		Total:    int64(aria2Bytes(m[3], m[4])),
		SpeedBPS: aria2Bytes(m[6], m[7]),
	}, true
}

func aria2Bytes(val, unit string) float64 {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0
	}
	switch unit {
	case "Ki":
		f *= 1 << 10
	case "Mi":
		f *= 1 << 20
	case "Gi":
		f *= 1 << 30
	}
	return f
}

func human(n int64) string {
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
	return fmt.Sprintf("%.1f %s", f, u[i])
}
