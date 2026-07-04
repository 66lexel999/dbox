//go:build windows

package wailsui

import (
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 "force a window to the foreground from a background thread". Windows
// blocks SetForegroundWindow from a process that isn't the active one; the
// standard workaround is to AttachThreadInput to the current foreground thread
// first, which lets us steal focus. Used so a captured download surfaces over
// the browser instead of only flashing in the taskbar.

var (
	u32     = windows.NewLazySystemDLL("user32.dll")
	k32     = windows.NewLazySystemDLL("kernel32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	pEnumWindows              = u32.NewProc("EnumWindows")
	pGetWindowThreadProcessId = u32.NewProc("GetWindowThreadProcessId")
	pIsWindowVisible          = u32.NewProc("IsWindowVisible")
	pGetWindow                = u32.NewProc("GetWindow")
	pGetForegroundWindow      = u32.NewProc("GetForegroundWindow")
	pAttachThreadInput        = u32.NewProc("AttachThreadInput")
	pSetForegroundWindow      = u32.NewProc("SetForegroundWindow")
	pBringWindowToTop         = u32.NewProc("BringWindowToTop")
	pShowWindow               = u32.NewProc("ShowWindow")
	pSetWindowPos             = u32.NewProc("SetWindowPos")
	pIsIconic                 = u32.NewProc("IsIconic")
	pSendMessage              = u32.NewProc("SendMessageW")
	pGetCurrentThreadId       = k32.NewProc("GetCurrentThreadId")
	pExtractIconEx            = shell32.NewProc("ExtractIconExW")
)

const (
	wmSetIcon = 0x0080
	iconSmall = 0
	iconBig   = 1
)

const (
	swRestore     = 9
	swShow        = 5
	gwOwner       = 4
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040

	hwndTopmost   = ^uintptr(0)     // (HWND)-1
	hwndNotopmost = ^uintptr(0) - 1 // (HWND)-2
)

var (
	findMu    sync.Mutex
	foundHWND uintptr
)

// enumCB picks this process's main top-level (visible, un-owned) window.
var enumCB = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
	var wpid uint32
	pGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
	if wpid != uint32(windows.GetCurrentProcessId()) {
		return 1
	}
	if vis, _, _ := pIsWindowVisible.Call(hwnd); vis == 0 {
		return 1
	}
	if owner, _, _ := pGetWindow.Call(hwnd, gwOwner); owner != 0 {
		return 1
	}
	foundHWND = hwnd
	return 0 // found it; stop enumerating
})

func ourWindow() uintptr {
	findMu.Lock()
	defer findMu.Unlock()
	foundHWND = 0
	pEnumWindows.Call(enumCB, 0)
	return foundHWND
}

// applyWindowIcon puts the app's OWN icon on the title bar and Alt-Tab. Wails
// doesn't set a window icon, so Windows draws the generic program icon there
// (even though the taskbar/exe icon is correct). The icon is pulled from THIS
// exe's embedded resource via ExtractIconEx — the same source the tray uses —
// so it always matches. Retries briefly because the HWND may not exist the very
// instant OnDomReady fires.
func applyWindowIcon() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	p, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return
	}
	var hwnd uintptr
	for i := 0; i < 40 && hwnd == 0; i++ { // up to ~2s
		if hwnd = ourWindow(); hwnd != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hwnd == 0 {
		return
	}
	var big, small uintptr
	if n, _, _ := pExtractIconEx.Call(uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&big)), uintptr(unsafe.Pointer(&small)), 1); n == 0 {
		return
	}
	if small != 0 {
		pSendMessage.Call(hwnd, wmSetIcon, iconSmall, small) // title bar
	}
	if big != 0 {
		pSendMessage.Call(hwnd, wmSetIcon, iconBig, big) // Alt-Tab / task switcher
	}
}

// forceForeground restores (if minimized), raises and focuses the app window.
func forceForeground() {
	hwnd := ourWindow()
	if hwnd == 0 {
		return
	}
	runtime.LockOSThread() // keep AttachThreadInput attach/detach on one OS thread
	defer runtime.UnlockOSThread()

	if ic, _, _ := pIsIconic.Call(hwnd); ic != 0 {
		pShowWindow.Call(hwnd, swRestore)
	} else {
		pShowWindow.Call(hwnd, swShow)
	}

	fg, _, _ := pGetForegroundWindow.Call()
	var fgThread uintptr
	if fg != 0 {
		fgThread, _, _ = pGetWindowThreadProcessId.Call(fg, 0)
	}
	cur, _, _ := pGetCurrentThreadId.Call()
	attached := fgThread != 0 && fgThread != cur
	if attached {
		pAttachThreadInput.Call(cur, fgThread, 1)
	}
	// Briefly flip topmost to jump the z-order above the (focused) browser,
	// then drop the pin so it doesn't stay always-on-top.
	pSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	pSetWindowPos.Call(hwnd, hwndNotopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	pBringWindowToTop.Call(hwnd)
	pSetForegroundWindow.Call(hwnd)
	if attached {
		pAttachThreadInput.Call(cur, fgThread, 0)
	}
}
