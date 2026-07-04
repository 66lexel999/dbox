//go:build windows

package gui

import (
	"path/filepath"
	"testing"
)

func TestGeomRoundTrip(t *testing.T) {
	dir := t.TempDir()
	g := winGeom{X: 100, Y: 120, W: 1200, H: 800, Max: true}
	saveGeom(dir, g)
	got, ok := loadGeom(dir)
	if !ok {
		t.Fatal("loadGeom returned not-ok after save")
	}
	if got != g {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, g)
	}
}

func TestLoadGeomMissing(t *testing.T) {
	if _, ok := loadGeom(filepath.Join(t.TempDir(), "does-not-exist")); ok {
		t.Fatal("expected not-ok when window.json is absent")
	}
}

func TestSanitizeClampsMinSize(t *testing.T) {
	g := sanitizeGeom(winGeom{X: 0, Y: 0, W: 10, H: 10})
	if g.W < 480 || g.H < 360 {
		t.Fatalf("min-size clamp failed: %+v", g)
	}
}
