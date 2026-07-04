//go:build windows

package gui

// Extract the Windows shell icon for a file — the file's OWN embedded icon for an
// .exe (so a downloaded VS Code setup shows the VS Code icon), or the default
// associated app's icon otherwise (WinRAR for .rar/.zip when it's the default,
// etc.). Returned as a 32-bit PNG the web UI can <img>-display. Native SHGetFileInfo
// + GDI, matching what Explorer shows.

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	gdi32            = syscall.NewLazyDLL("gdi32.dll")
	procGetObject    = gdi32.NewProc("GetObjectW")
	procGetDIBits    = gdi32.NewProc("GetDIBits")
	procDeleteObject = gdi32.NewProc("DeleteObject")

	shlwapi              = syscall.NewLazyDLL("shlwapi.dll")
	procAssocQueryString = shlwapi.NewProc("AssocQueryStringW") // shlwapi, NOT shell32

	procSHGetFileInfo = shell32.NewProc("SHGetFileInfoW")

	procGetIconInfo = user32.NewProc("GetIconInfo")
	procDestroyIcon = user32.NewProc("DestroyIcon")
	procGetDC       = user32.NewProc("GetDC")
	procReleaseDC   = user32.NewProc("ReleaseDC")
)

const (
	shgfiIcon              = 0x000000100
	shgfiLargeIcon         = 0x000000000 // 32x32
	shgfiUseFileAttributes = 0x000000010
	fileAttributeNormal    = 0x00000080
	diBRGBColors           = 0
)

type shFileInfo struct {
	hIcon         uintptr
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

type iconInfoT struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmapT struct {
	bmType       int32
	bmWidth      int32
	bmHeight     int32
	bmWidthBytes int32
	bmPlanes     uint16
	bmBitsPixel  uint16
	bmBits       uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// FileIconPNG returns path's shell icon as PNG bytes. Uses the real file when it
// exists (so an .exe yields its own icon); otherwise resolves by extension.
func FileIconPNG(path string) ([]byte, error) {
	b, _, err := FileIconPNGEx(path)
	return b, err
}

// FileIconPNGEx additionally reports degraded=true when the icon came from the
// EXTENSION fallback even though the file exists on disk — i.e. the real file's
// own icon (an .exe's embedded artwork) couldn't be read right now (AV scan
// lock on a fresh download). Callers cache degraded results briefly so the next
// attempt can upgrade to the real icon, instead of pinning the generic one.
func FileIconPNGEx(path string) (png []byte, degraded bool, err error) {
	// SHGetFileInfo requires COM initialized on the calling thread (its docs are
	// explicit). Go schedules onto arbitrary OS threads, so without pinning +
	// initializing here, extraction succeeds or fails depending on which thread
	// the call happens to land on — the "same file, sometimes the real icon,
	// sometimes the fallback" symptom.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	if uint32(hr) == 0 { // S_OK — we initialized; balance it. S_FALSE/MTA: leave the thread as-is.
		defer procCoUninitialize.Call()
	}

	hIcon := shellIcon(path, false)
	if hIcon == 0 {
		// Real-file lookup can fail transiently (AV still scanning a fresh
		// download, file moved): fall back to the extension's associated icon
		// so the row shows the default app instead of nothing.
		hIcon = shellIcon(path, true)
		if _, statErr := os.Stat(filepath.Clean(path)); statErr == nil {
			degraded = true // file IS there — its own icon should win on a retry
		}
	}
	if hIcon == 0 {
		return nil, false, fmt.Errorf("no icon for %s", path)
	}
	defer procDestroyIcon.Call(hIcon)
	b, err := iconToPNG(hIcon)
	return b, degraded, err
}

// shellIcon fetches the 32px shell icon handle. byExtOnly skips the file on
// disk and resolves purely from the extension's association.
func shellIcon(path string, byExtOnly bool) uintptr {
	path = filepath.Clean(path) // SHGetFileInfo needs backslashes, not forward slashes
	flags := uintptr(shgfiIcon | shgfiLargeIcon)
	var attrs uintptr
	if byExtOnly {
		flags |= shgfiUseFileAttributes
		attrs = fileAttributeNormal
	} else if _, err := os.Stat(path); err != nil { // file not on disk → resolve by extension
		flags |= shgfiUseFileAttributes
		attrs = fileAttributeNormal
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var sfi shFileInfo
	r, _, _ := procSHGetFileInfo.Call(uintptr(unsafe.Pointer(p)), attrs,
		uintptr(unsafe.Pointer(&sfi)), unsafe.Sizeof(sfi), flags)
	if r == 0 {
		return 0
	}
	return sfi.hIcon
}

func iconToPNG(hIcon uintptr) ([]byte, error) {
	var ii iconInfoT
	if r, _, _ := procGetIconInfo.Call(hIcon, uintptr(unsafe.Pointer(&ii))); r == 0 {
		return nil, fmt.Errorf("GetIconInfo failed")
	}
	if ii.hbmColor != 0 {
		defer procDeleteObject.Call(ii.hbmColor)
	}
	if ii.hbmMask != 0 {
		defer procDeleteObject.Call(ii.hbmMask)
	}
	if ii.hbmColor == 0 {
		return nil, fmt.Errorf("monochrome icon unsupported")
	}

	var bm bitmapT
	if r, _, _ := procGetObject.Call(ii.hbmColor, unsafe.Sizeof(bm), uintptr(unsafe.Pointer(&bm))); r == 0 {
		return nil, fmt.Errorf("GetObject failed")
	}
	w, h := int(bm.bmWidth), int(bm.bmHeight)
	if w <= 0 || h <= 0 || w > 512 || h > 512 {
		return nil, fmt.Errorf("bad icon size %dx%d", w, h)
	}

	hdc, _, _ := procGetDC.Call(0)
	defer procReleaseDC.Call(0, hdc)

	bi := bitmapInfoHeader{biPlanes: 1, biBitCount: 32}
	bi.biSize = uint32(unsafe.Sizeof(bi))
	bi.biWidth = int32(w)
	bi.biHeight = -int32(h) // negative = top-down rows

	colorBuf := make([]byte, w*h*4)
	procGetDIBits.Call(hdc, ii.hbmColor, 0, uintptr(h),
		uintptr(unsafe.Pointer(&colorBuf[0])), uintptr(unsafe.Pointer(&bi)), diBRGBColors)

	// Modern icons carry alpha in the color bitmap; legacy ones need the AND mask.
	hasAlpha := false
	for i := 3; i < len(colorBuf); i += 4 {
		if colorBuf[i] != 0 {
			hasAlpha = true
			break
		}
	}
	var maskBuf []byte
	if !hasAlpha && ii.hbmMask != 0 {
		maskBuf = make([]byte, w*h*4)
		bi2 := bi
		procGetDIBits.Call(hdc, ii.hbmMask, 0, uintptr(h),
			uintptr(unsafe.Pointer(&maskBuf[0])), uintptr(unsafe.Pointer(&bi2)), diBRGBColors)
	}

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		b, g, r, a := colorBuf[i*4], colorBuf[i*4+1], colorBuf[i*4+2], colorBuf[i*4+3]
		if !hasAlpha {
			a = 255
			if maskBuf != nil && maskBuf[i*4] != 0 { // AND mask set = transparent
				a = 0
			}
		}
		img.Pix[i*4] = r
		img.Pix[i*4+1] = g
		img.Pix[i*4+2] = b
		img.Pix[i*4+3] = a
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// DefaultAppIsGeneric reports whether the shell would draw only a generic
// system icon for ext (".zip") — i.e. no app registered its own artwork — so
// the caller can prefer its category icon over a meaningless white file.
// Judged by ASSOCSTR_DEFAULTICON (what actually gets DRAWN), not by which exe
// opens the type: an archiver like WinRAR/7-Zip registers a real icon, while an
// unassociated type resolves to a shell32/imageres stock slot.
func DefaultAppIsGeneric(ext string) bool {
	icon := strings.ToLower(assocString(ext, assocstrDefaultIcon))
	if icon == "" {
		return true // nothing registered at all
	}
	if icon == "%1" {
		return false // the file IS its own icon (.exe, .ico)
	}
	return strings.Contains(icon, "shell32.dll") || strings.Contains(icon, "imageres.dll") ||
		strings.Contains(icon, "zipfldr.dll") || strings.Contains(icon, "explorer.exe")
}

const (
	assocstrExecutable  = 2
	assocstrDefaultIcon = 15
)

// assocString wraps AssocQueryString for the "open" verb.
func assocString(ext string, what uintptr) string {
	e, err := syscall.UTF16PtrFromString(ext)
	if err != nil {
		return ""
	}
	verb, _ := syscall.UTF16PtrFromString("open")
	var size uint32 = 1024
	buf := make([]uint16, size)
	r, _, _ := procAssocQueryString.Call(0, what,
		uintptr(unsafe.Pointer(e)), uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 { // non-S_OK
		return ""
	}
	return syscall.UTF16ToString(buf)
}
