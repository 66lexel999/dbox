//go:build !windows

package gui

import "errors"

// FileIconPNG is Windows-only; other platforms have no shell-icon extraction.
func FileIconPNG(path string) ([]byte, error) { return nil, errors.New("file icons are windows-only") }

// FileIconPNGEx matches the Windows signature (png, degraded, err).
func FileIconPNGEx(path string) ([]byte, bool, error) {
	return nil, false, errors.New("file icons are windows-only")
}

// DefaultAppIsGeneric is conservatively true off Windows (use our own icons).
func DefaultAppIsGeneric(ext string) bool { return true }
