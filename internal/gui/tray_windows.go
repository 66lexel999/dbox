//go:build windows

package gui

// System-tray (notification area) icon, IDM-style: closing the main window hides
// it to the tray instead of quitting. Native Shell_NotifyIcon — no extra deps,
// matching the rest of this package's raw Win32 usage. A single tray instance per
// process (package-level state); the icon is the snail embedded in the .exe.

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	shell32             = syscall.NewLazyDLL("shell32.dll")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procExtractIcon     = shell32.NewProc("ExtractIconW")

	procRegisterClassEx  = user32.NewProc("RegisterClassExW")
	procCreateWindowEx   = user32.NewProc("CreateWindowExW")
	procDefWindowProc    = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenu       = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procGetMessage       = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessage  = user32.NewProc("DispatchMessageW")

	procLoadIcon        = user32.NewProc("LoadIconW")
	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
)

const idiApplication = 32512 // default app icon, if the exe has none to extract

const (
	wmTrayCallback  = 0x0400 + 1 // WM_APP+1
	wmCommandMsg    = 0x0111     // WM_COMMAND
	wmLButtonUp     = 0x0202
	wmLButtonDblclk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0x0
	nimDelete = 0x2
	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04

	menuOpenID = 1
	menuExitID = 2

	mfString       = 0x0000
	tpmRightButton = 0x0002
)

type notifyIconData struct {
	cbSize            uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uVersionOrTimeout uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          [16]byte
	hBalloonIcon      uintptr
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type trayMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ X, Y int32 }
	lPrivate uint32
}

var (
	trayOnOpen func()
	trayOnExit func()
	trayNID    notifyIconData
	trayMenu   uintptr
	trayActive bool
)

var trayWndProc = syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTrayCallback:
		switch lParam {
		case wmLButtonUp, wmLButtonDblclk:
			if trayOnOpen != nil {
				trayOnOpen()
			}
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0
	case wmCommandMsg:
		switch wParam & 0xffff {
		case menuOpenID:
			if trayOnOpen != nil {
				trayOnOpen()
			}
		case menuExitID:
			if trayOnExit != nil {
				trayOnExit()
			}
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, msg, wParam, lParam)
	return ret
})

// showTrayMenu pops the right-click menu. SetForegroundWindow first so the menu
// dismisses correctly when the user clicks elsewhere (a Win32 tray-menu quirk).
func showTrayMenu(hwnd uintptr) {
	if trayMenu == 0 {
		return
	}
	procSetForegroundWindow.Call(hwnd)
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procTrackPopupMenu.Call(trayMenu, tpmRightButton, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
}

// RunTray creates the tray icon and runs its message loop (BLOCKS — call in a
// goroutine). onOpen fires on left click / "Open"; onExit on "Exit". It signals
// ready<-true once the icon is up, or ready<-false if creation failed (so the
// caller can fall back to a plain minimize). The loop ends when the process exits.
func RunTray(tooltip string, onOpen, onExit func(), ready chan<- bool) {
	runtime.LockOSThread()
	trayOnOpen, trayOnExit = onOpen, onExit

	hInst, _, _ := procGetModuleHandle.Call(0)

	// First icon embedded in the .exe = the D BOX app icon.
	var hIcon uintptr
	if exe, err := os.Executable(); err == nil {
		if p, err := syscall.UTF16PtrFromString(exe); err == nil {
			hIcon, _, _ = procExtractIcon.Call(hInst, uintptr(unsafe.Pointer(p)), 0)
		}
	}
	if hIcon == 0 || hIcon == 1 { // ExtractIcon returns 1 when the file has no icons
		hIcon, _, _ = procLoadIcon.Call(0, idiApplication)
	}

	className, _ := syscall.UTF16PtrFromString("DBoxTrayClass")
	wc := wndClassEx{lpfnWndProc: trayWndProc, hInstance: hInst, hIcon: hIcon, hIconSm: hIcon, lpszClassName: className}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if atom, _, _ := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		if ready != nil {
			ready <- false
		}
		return
	}

	winName, _ := syscall.UTF16PtrFromString("D BOX")
	hwnd, _, _ := procCreateWindowEx.Call(0,
		uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(winName)),
		0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if hwnd == 0 {
		if ready != nil {
			ready <- false
		}
		return
	}

	trayMenu, _, _ = procCreatePopupMenu.Call()
	if openTxt, err := syscall.UTF16PtrFromString("Open D BOX"); err == nil {
		procAppendMenu.Call(trayMenu, mfString, menuOpenID, uintptr(unsafe.Pointer(openTxt)))
	}
	if exitTxt, err := syscall.UTF16PtrFromString("Exit"); err == nil {
		procAppendMenu.Call(trayMenu, mfString, menuExitID, uintptr(unsafe.Pointer(exitTxt)))
	}

	trayNID = notifyIconData{hWnd: hwnd, uID: 1, uFlags: nifMessage | nifIcon | nifTip,
		uCallbackMessage: wmTrayCallback, hIcon: hIcon}
	trayNID.cbSize = uint32(unsafe.Sizeof(trayNID))
	for i, c := range syscall.StringToUTF16(tooltip) {
		if i >= len(trayNID.szTip) {
			break
		}
		trayNID.szTip[i] = c
	}
	if r, _, _ := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&trayNID))); r == 0 {
		procDestroyWindow.Call(hwnd)
		if ready != nil {
			ready <- false
		}
		return
	}
	trayActive = true
	if ready != nil {
		ready <- true
	}

	var msg trayMsg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	RemoveTray()
}

// RemoveTray deletes the tray icon. Safe to call more than once and from any
// thread (call right before the app actually exits so no ghost icon lingers).
func RemoveTray() {
	if !trayActive {
		return
	}
	trayActive = false
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&trayNID)))
}
