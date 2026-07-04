package engine

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// probeResult describes what the server told us about the resource.
type probeResult struct {
	Size         int64 // -1 unknown
	Ranged       bool
	FileName     string // from Content-Disposition or URL path; may be ""
	ETag         string
	LastModified string
	ContentType  string
}

// probe issues GET Range: bytes=0-0 and inspects the answer.
//   - 206 -> server honors ranges; total size from Content-Range.
//   - 200 -> no range support; size from Content-Length (may be -1/chunked).
//
// A 0-0 GET is more truthful than HEAD: many servers answer HEAD with
// Accept-Ranges they don't actually honor, or omit Content-Length.
func (e *Engine) probe(ctx context.Context, rawURL string) (probeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("User-Agent", e.cfg.UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")

	resp, err := e.client.Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer resp.Body.Close()

	pr := probeResult{
		Size:         -1,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		FileName:     fileNameFromResponse(resp, rawURL),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		pr.Ranged = true
		if total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range")); ok {
			pr.Size = total
		}
	case http.StatusOK:
		pr.Ranged = false
		if resp.ContentLength >= 0 {
			pr.Size = resp.ContentLength
		}
	default:
		return probeResult{}, fmt.Errorf("server returned %s", resp.Status)
	}
	return pr, nil
}

// parseContentRangeTotal extracts N from "bytes 0-0/N".
func parseContentRangeTotal(h string) (int64, bool) {
	idx := strings.LastIndexByte(h, '/')
	if idx < 0 {
		return 0, false
	}
	totalStr := strings.TrimSpace(h[idx+1:])
	if totalStr == "*" {
		return 0, false
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil || total < 0 {
		return 0, false
	}
	return total, true
}

func fileNameFromResponse(resp *http.Response, rawURL string) string {
	// Content-Disposition wins; mime.ParseMediaType handles filename* (RFC 5987).
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := params["filename"]; name != "" {
				return SanitizeFileName(name)
			}
		}
	}
	// Use the post-redirect URL's path, falling back to the original.
	u := resp.Request.URL
	if u == nil {
		if parsed, err := url.Parse(rawURL); err == nil {
			u = parsed
		}
	}
	if u != nil {
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			if dec, err := url.PathUnescape(base); err == nil {
				base = dec
			}
			return SanitizeFileName(base)
		}
	}
	return ""
}

// SanitizeFileName strips characters NTFS rejects and guards against
// path traversal in server-supplied names.
func SanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name) // drop any directory components
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, strings.ContainsRune(`<>:"/\|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		out = "download"
	}
	return out
}
