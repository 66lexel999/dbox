// Package config holds runtime configuration for MyIDM.
// Stdlib-only; values come from flags with sane Windows-friendly defaults.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Listen         string // host:port for the HTTP UI/API
	DownloadDir    string // where finished files land
	DataDir        string // where tasks.json lives
	Segments       int    // default parallel connections per download
	MaxConcurrent  int    // max tasks downloading at once
	MaxRetries     int    // per-connection retry budget (resets on progress)
	MinSegmentSize int64  // don't split below this many bytes per segment
	SpeedLimit     int64  // global bytes/sec cap, 0 = unlimited
	UserAgent      string
	OpenBrowser    bool
	UIKind         string            // "wails" (WebView2 via Wails) | "walk" (native Win32) | "webview" (raw WebView2) | "off" (headless)
	GUI            bool              // derived: true unless UIKind=="off"
	Categories     map[string]string // IDM-style category name -> destination folder
}

// DefaultCategories maps IDM-style category names to folders under the download
// directory. "General" is the download root itself.
func DefaultCategories(dl string) map[string]string {
	return map[string]string{
		"General":    dl,
		"Compressed": filepath.Join(dl, "Compressed"),
		"Documents":  filepath.Join(dl, "Documents"),
		"Music":      filepath.Join(dl, "Music"),
		"Video":      filepath.Join(dl, "Video"),
		"Programs":   filepath.Join(dl, "Programs"),
		"Images":     filepath.Join(dl, "Images"),
	}
}

// The on-disk data/download folder is intentionally still named "flowerX" even
// though the app is branded "D BOX": renaming it would orphan existing users'
// download history. Keep it for continuity (migrate explicitly if ever needed).

// DefaultDataDir is where app state (tasks.json, settings.json, logs) lives.
func DefaultDataDir() string {
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "flowerX")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".flowerx")
}

// DefaultDownloadDir is the out-of-the-box base download folder.
func DefaultDownloadDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "flowerX")
}

func Default() *Config {
	dataDir := DefaultDataDir()
	dl := DefaultDownloadDir()
	return &Config{
		Listen:         "127.0.0.1:8081",
		DownloadDir:    dl,
		DataDir:        dataDir,
		Segments:       8,
		MaxConcurrent:  1, // one download at a time by default (segmented engine saturates a link with one); raise it in Options / -concurrent
		MaxRetries:     5,
		MinSegmentSize: 512 << 10,
		SpeedLimit:     0,
		UserAgent:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
		OpenBrowser:    true,
		UIKind:         "wails",
		GUI:            true,
		Categories:     DefaultCategories(dl),
	}
}

// FromFlags parses command-line flags over the defaults.
func FromFlags(args []string) (*Config, error) {
	cfg := Default()
	fs := flag.NewFlagSet("myidm", flag.ContinueOnError)

	fs.StringVar(&cfg.Listen, "listen", cfg.Listen, "address to serve UI/API on")
	fs.StringVar(&cfg.DownloadDir, "dir", cfg.DownloadDir, "download directory")
	fs.StringVar(&cfg.DataDir, "data", cfg.DataDir, "state directory (tasks.json)")
	fs.IntVar(&cfg.Segments, "segments", cfg.Segments, "default connections per download (1-32)")
	fs.IntVar(&cfg.MaxConcurrent, "concurrent", cfg.MaxConcurrent, "max simultaneous downloads")
	fs.IntVar(&cfg.MaxRetries, "retries", cfg.MaxRetries, "retries per connection before failing")
	fs.StringVar(&cfg.UserAgent, "ua", cfg.UserAgent, "User-Agent header")
	fs.BoolVar(&cfg.OpenBrowser, "open", cfg.OpenBrowser, "open the UI in the browser (headless mode only)")
	guiFlag := fs.String("gui", "wails", "UI: wails (default, WebView2 HTML), walk (native Win32 widgets), webview (raw WebView2), or off (headless server)")
	limit := fs.String("limit", "0", "global speed limit, e.g. 2M, 500K (0 = unlimited)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	switch g := strings.ToLower(strings.TrimSpace(*guiFlag)); g {
	case "off", "false", "none", "0", "headless":
		cfg.UIKind, cfg.GUI = "off", false
	case "wails", "true", "1", "":
		cfg.UIKind, cfg.GUI = "wails", true
	case "walk", "native":
		cfg.UIKind, cfg.GUI = "walk", true
	case "webview", "web":
		cfg.UIKind, cfg.GUI = "webview", true
	default:
		fmt.Fprintf(os.Stderr, "warning: unknown -gui=%q; using wails (valid: wails, walk, webview, off)\n", *guiFlag)
		cfg.UIKind, cfg.GUI = "wails", true
	}

	v, err := ParseSize(*limit)
	if err != nil {
		return nil, fmt.Errorf("invalid -limit: %w", err)
	}
	cfg.SpeedLimit = v

	if cfg.Segments < 1 {
		cfg.Segments = 1
	}
	if cfg.Segments > 32 {
		cfg.Segments = 32
	}
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 1
	}
	// Keep category folders rooted under the (possibly -dir-overridden) dir.
	cfg.Categories = DefaultCategories(cfg.DownloadDir)
	return cfg, nil
}

// ParseSize parses "0", "1048576", "512K", "2M", "1.5G" into bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return int64(f * float64(mult)), nil
}
