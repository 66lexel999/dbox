package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"myidm/internal/config"
)

// migrateFromMyIDM performs the one-time MyIDM -> flowerX rename for upgraders:
// it moves the old data and download folders to their new (flowerX) locations and
// rewrites stored task paths to match. It is conservative — it only renames when
// the new location is ABSENT and the old one exists, and NEVER deletes user data.
// If a move can't happen (folder locked, different volume) it points the app back
// at the old folder so nothing is lost. Returns notes for the caller to log once
// the logger exists (this runs before the data dir is created).
func migrateFromMyIDM(cfg *config.Config) []string {
	var notes []string
	// Only auto-migrate the DEFAULT locations; an explicit -data/-dir means the
	// user is managing their own paths, so leave them alone.
	migrateData := cfg.DataDir == config.DefaultDataDir()
	migrateDownloads := cfg.DownloadDir == config.DefaultDownloadDir()
	home, _ := os.UserHomeDir()
	oldData := filepath.Join(os.Getenv("LOCALAPPDATA"), "MyIDM")
	if os.Getenv("LOCALAPPDATA") == "" {
		oldData = filepath.Join(home, ".myidm")
	}
	oldDownloads := filepath.Join(home, "Downloads", "MyIDM")

	// 1) Data dir (tasks.json, settings.json, logs, webview cache).
	if migrateData && !dirExists(cfg.DataDir) && dirExists(oldData) {
		if err := os.Rename(oldData, cfg.DataDir); err != nil {
			cfg.DataDir = oldData // keep using the old data dir so history survives
			notes = append(notes, fmt.Sprintf("data dir move failed (%v); keeping %s", err, oldData))
		} else {
			notes = append(notes, "moved data dir "+oldData+" -> "+cfg.DataDir)
		}
	}

	// 2) Download dir. On failure keep the OLD folder so the user's files stay
	//    visible (data is never lost), and root the categories there.
	downloadsMoved := false
	if migrateDownloads && !dirExists(cfg.DownloadDir) && dirExists(oldDownloads) {
		if err := os.Rename(oldDownloads, cfg.DownloadDir); err != nil {
			cfg.DownloadDir = oldDownloads
			cfg.Categories = config.DefaultCategories(oldDownloads)
			notes = append(notes, fmt.Sprintf("download move failed (%v); keeping %s", err, oldDownloads))
		} else {
			downloadsMoved = true
			notes = append(notes, "moved downloads "+oldDownloads+" -> "+cfg.DownloadDir)
		}
	}

	// 3) If downloads moved, repoint stored task paths at the new folder.
	if downloadsMoved {
		if n := rewriteTaskPaths(filepath.Join(cfg.DataDir, "tasks.json"), oldDownloads, cfg.DownloadDir); n > 0 {
			notes = append(notes, fmt.Sprintf("rewrote %d task paths to the new folder", n))
		}
	}
	return notes
}

func dirExists(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// rewriteTaskPaths swaps the download-folder prefix in each task's finalPath/dir.
// Decoupled from the engine's Task type — it edits the raw JSON object map so a
// future schema change can't silently drop fields. Returns the number of swaps.
func rewriteTaskPaths(path, oldPrefix, newPrefix string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var tasks []map[string]any
	if json.Unmarshal(b, &tasks) != nil {
		return 0
	}
	n := 0
	for _, t := range tasks {
		for _, k := range []string{"finalPath", "dir"} {
			if s, ok := t[k].(string); ok {
				if r := swapPrefix(s, oldPrefix, newPrefix); r != s {
					t[k] = r
					n++
				}
			}
		}
	}
	if n == 0 {
		return 0
	}
	if nb, err := json.MarshalIndent(tasks, "", "  "); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, nb, 0o644) == nil {
			os.Rename(tmp, path)
		}
	}
	return n
}

// swapPrefix replaces a leading path prefix, tolerant of \ vs / and case (Windows
// paths). Prefix chars map 1:1 under normalization, so the original tail is kept.
func swapPrefix(s, oldP, newP string) string {
	norm := func(x string) string { return strings.ToLower(strings.ReplaceAll(x, "\\", "/")) }
	if strings.HasPrefix(norm(s), norm(oldP)) {
		return newP + s[len(oldP):]
	}
	return s
}
