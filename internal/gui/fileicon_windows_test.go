//go:build windows

package gui

import (
	"bytes"
	"image/png"
	"sync"
	"testing"
)

// TestFileIconPNGConcurrent extracts a real .exe's icon from many goroutines at
// once. Goroutines land on arbitrary OS threads — exactly the condition that
// made SHGetFileInfo fail intermittently before COM was pinned+initialized per
// call. Every attempt must now succeed and decode as a PNG.
func TestFileIconPNGConcurrent(t *testing.T) {
	const target = `C:\Windows\System32\notepad.exe`
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := FileIconPNG(target)
			if err != nil {
				errs <- err
				return
			}
			if _, err := png.Decode(bytes.NewReader(b)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	n := 0
	for err := range errs {
		n++
		t.Errorf("extraction failed: %v", err)
	}
	if n > 0 {
		t.Fatalf("%d of 32 concurrent extractions failed", n)
	}
}

// TestFileIconPNGMissingFileFallsBack resolves a nonexistent path purely by
// extension — the fallback that keeps a row on the associated-app icon when the
// real file is briefly unreadable (AV scan) or gone.
func TestFileIconPNGMissingFileFallsBack(t *testing.T) {
	b, err := FileIconPNG(`X:\definitely\not\there\file.txt`)
	if err != nil {
		t.Fatalf("extension fallback failed: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(b)); err != nil {
		t.Fatalf("fallback icon is not a PNG: %v", err)
	}
}

// TestDefaultAppIsGeneric pins the machine-stable cases: .exe files draw their
// own icon (%1 → not generic) and a made-up extension has nothing registered
// (→ generic).
func TestDefaultAppIsGeneric(t *testing.T) {
	if DefaultAppIsGeneric(".exe") {
		t.Error(".exe should not be generic — %1 means the file draws its own icon")
	}
	if !DefaultAppIsGeneric(".zz9xq") {
		t.Error("an unregistered extension should be generic")
	}
}
