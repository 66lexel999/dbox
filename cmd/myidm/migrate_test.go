package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSwapPrefix(t *testing.T) {
	cases := []struct{ in, oldP, newP, want string }{
		{`C:\Users\a\Downloads\MyIDM\Video\x.mp4`, `C:\Users\a\Downloads\MyIDM`, `C:\Users\a\Downloads\flowerX`, `C:\Users\a\Downloads\flowerX\Video\x.mp4`},
		{`C:\Users\a\Downloads\myidm\v.mp4`, `C:\Users\a\Downloads\MyIDM`, `D:\dl`, `D:\dl\v.mp4`}, // case-insensitive match
		{`C:\Other\v.mp4`, `C:\Users\a\Downloads\MyIDM`, `D:\dl`, `C:\Other\v.mp4`},                 // no match -> unchanged
	}
	for _, c := range cases {
		if got := swapPrefix(c.in, c.oldP, c.newP); got != c.want {
			t.Errorf("swapPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRewriteTaskPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	oldRoot := `C:\Users\a\Downloads\MyIDM`
	newRoot := `D:\Media\flowerX`
	tasks := []map[string]any{
		{"id": "1", "finalPath": oldRoot + `\Video\a.mp4`, "dir": oldRoot + `\Video`},
		{"id": "2", "finalPath": `C:\elsewhere\b.zip`, "dir": `C:\elsewhere`}, // untouched
	}
	b, _ := json.Marshal(tasks)
	os.WriteFile(path, b, 0o644)

	if n := rewriteTaskPaths(path, oldRoot, newRoot); n != 2 {
		t.Fatalf("rewrote %d fields, want 2 (finalPath+dir of task 1)", n)
	}
	out, _ := os.ReadFile(path)
	var got []map[string]any
	json.Unmarshal(out, &got)
	if got[0]["finalPath"] != newRoot+`\Video\a.mp4` {
		t.Errorf("task1 finalPath = %v", got[0]["finalPath"])
	}
	if got[0]["dir"] != newRoot+`\Video` {
		t.Errorf("task1 dir = %v", got[0]["dir"])
	}
	if got[1]["finalPath"] != `C:\elsewhere\b.zip` {
		t.Errorf("task2 should be untouched, got %v", got[1]["finalPath"])
	}
}
