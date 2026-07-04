//go:build windows

package walkui

// Dark theming for native Win32 controls. lxn/walk renders real ComCtl32
// widgets, which are light-themed by default. We force them dark three ways:
//
//  1. App-wide:   uxtheme!SetPreferredAppMode(ForceDark) + FlushMenuThemes —
//     makes context menus, scrollbars and tooltips dark.
//  2. Per window: DwmSetWindowAttribute(USE_IMMERSIVE_DARK_MODE) for the dark
//     title bar, plus uxtheme!AllowDarkModeForWindow.
//  3. Per control: SetWindowTheme(h, "DarkMode_Explorer") so list/tree headers
//     and scrollbars paint dark.
//
// Cell/background colors come from the walk.Color palette below (CellStyler on
// the table model, declarative SolidColorBrush backgrounds elsewhere).

import (
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// walkClr converts a 0xRRGGBB literal to a walk.Color (COLORREF 0x00BBGGRR).
func walkClr(hex uint32) walk.Color {
	return walk.RGB(byte(hex>>16), byte(hex>>8), byte(hex))
}

// Palette — identical hex values to the previous (Gio / WebView2) dark theme.
var (
	cBg     = walkClr(0x3c4043) // window chrome
	cBg2    = walkClr(0x34383b) // darker chrome / headers
	cPanel  = walkClr(0x303336) // list / panels
	cPanel2 = walkClr(0x44484b) // raised surfaces, inputs
	cBorder = walkClr(0x4e5256)
	cGrid   = walkClr(0x474b4e)
	cText   = walkClr(0xe6e6e6)
	cMuted  = walkClr(0x9aa0a4)
	cSel    = walkClr(0x3f5e80) // selection (steel blue)
	cAccent = walkClr(0x6aa6e6)
	cGreen  = walkClr(0x5cba6a)
	cRed    = walkClr(0xe07a7a)
	cYellow = walkClr(0xd8b24a)
)

// ---- Win32 dark-mode plumbing -------------------------------------------

var (
	modUxtheme  = windows.NewLazySystemDLL("uxtheme.dll")
	modDwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	modKernel   = windows.NewLazySystemDLL("kernel32.dll")
	modComctl32 = windows.NewLazySystemDLL("comctl32.dll")

	procGetProcAddress          = modKernel.NewProc("GetProcAddress")
	procDwmSetWindowAttribute   = modDwmapi.NewProc("DwmSetWindowAttribute")
	procSetWindowSubclass       = modComctl32.NewProc("SetWindowSubclass")
	procDefSubclassProc         = modComctl32.NewProc("DefSubclassProc")
	uxAllowDarkModeForWindowPtr uintptr // ordinal 133
	uxSetPreferredAppModePtr    uintptr // ordinal 135 (1903+)
	uxFlushMenuThemesPtr        uintptr // ordinal 136
)

const (
	wmEnterSizeMove = 0x0231
	wmExitSizeMove  = 0x0232
)

// installResizeGuard subclasses a top-level window to call onEnter when the user
// begins a modal move/resize drag and onExit when it ends. We use it to suppress
// the per-frame column re-fit during a resize drag — changing a column width
// forces a full owner-draw repaint of every (Arabic-shaped) row, ~200ms each, so
// doing it on every WM_SIZE drops to ~2fps. We skip it mid-drag and fit once at
// the end. The subclass callback is created once and never freed (as required by
// syscall.NewCallback), and chains to DefSubclassProc for normal handling.
func installResizeGuard(hwnd win.HWND, onEnter, onExit func()) {
	cb := syscall.NewCallback(func(h, msg, wParam, lParam, idSubclass, refData uintptr) uintptr {
		switch msg {
		case wmEnterSizeMove:
			onEnter()
		case wmExitSizeMove:
			onExit()
		}
		r, _, _ := procDefSubclassProc.Call(h, msg, wParam, lParam)
		return r
	})
	procSetWindowSubclass.Call(uintptr(hwnd), cb, 1, 0)
}

// uxProc resolves an undocumented uxtheme.dll export by ordinal.
func uxProc(ordinal uintptr) uintptr {
	if err := modUxtheme.Load(); err != nil {
		return 0
	}
	r, _, _ := procGetProcAddress.Call(modUxtheme.Handle(), ordinal)
	return r
}

const (
	dwmwaUseImmersiveDarkMode    = 20 // Win10 2004+
	dwmwaUseImmersiveDarkModeOld = 19 // Win10 1809..1909
	preferredAppModeForceDark    = 2
)

// enableDarkModeApp flips the process into dark mode so menus, scrollbars,
// headers and tooltips render dark. Safe to call once at startup; no-ops on
// older Windows. RefreshImmersiveColorPolicyState (104) must follow
// SetPreferredAppMode for the change to actually take effect for controls.
func enableDarkModeApp() {
	uxSetPreferredAppModePtr = uxProc(135)
	uxFlushMenuThemesPtr = uxProc(136)
	uxAllowDarkModeForWindowPtr = uxProc(133)
	refresh := uxProc(104) // RefreshImmersiveColorPolicyState
	if uxSetPreferredAppModePtr != 0 {
		syscall.SyscallN(uxSetPreferredAppModePtr, uintptr(preferredAppModeForceDark))
	}
	if refresh != 0 {
		syscall.SyscallN(refresh)
	}
	if uxFlushMenuThemesPtr != 0 {
		syscall.SyscallN(uxFlushMenuThemesPtr)
	}
}

// applyDarkFrame gives a top-level window a dark title bar + non-client frame.
func applyDarkFrame(hwnd win.HWND) {
	if uxAllowDarkModeForWindowPtr != 0 {
		syscall.SyscallN(uxAllowDarkModeForWindowPtr, uintptr(hwnd), 1)
	}
	on := int32(1)
	r, _, _ := procDwmSetWindowAttribute.Call(uintptr(hwnd),
		uintptr(dwmwaUseImmersiveDarkMode), uintptr(unsafe.Pointer(&on)), 4)
	if r != 0 { // pre-2004 build: attribute index was 19
		procDwmSetWindowAttribute.Call(uintptr(hwnd),
			uintptr(dwmwaUseImmersiveDarkModeOld), uintptr(unsafe.Pointer(&on)), 4)
	}
}

func setTheme(h win.HWND, name string) {
	sub, _ := syscall.UTF16PtrFromString(name)
	win.SetWindowTheme(h, sub, nil)
}

func allowDarkModeForWindow(h win.HWND) {
	if uxAllowDarkModeForWindowPtr != 0 {
		syscall.SyscallN(uxAllowDarkModeForWindowPtr, uintptr(h), 1)
	}
}

// themeByClass opts a control into dark mode (per-control AllowDarkModeForWindow
// is required, not just the top-level window) and applies the matching dark
// visual style. List/tree/header/toolbar/buttons use the Explorer dark style;
// edits and combo boxes use the "Common File Dialog" dark style (the only one
// that darkens their client area).
func themeByClass(h win.HWND) {
	allowDarkModeForWindow(h)
	var buf [80]uint16
	win.GetClassName(h, &buf[0], len(buf))
	switch syscall.UTF16ToString(buf[:]) {
	case "SysListView32", "SysHeader32", "SysTreeView32", "ToolbarWindow32", "Button", "ScrollBar", "Static", "msctls_progress32":
		setTheme(h, "DarkMode_Explorer")
	case "Edit", "ComboBox", "ComboLBox":
		setTheme(h, "DarkMode_CFD")
	}
}

var darkEnumCB = syscall.NewCallback(func(h win.HWND, _ uintptr) uintptr {
	themeByClass(h)
	return 1
})

// darkThemeTree dark-themes a top-level window and every descendant control.
func darkThemeTree(root win.HWND) {
	themeByClass(root)
	win.EnumChildWindows(root, darkEnumCB, 0)
}

// darkenListViewBodies sets the background of every SysListView32 inside a
// walk.TableView (it nests a frozen + a normal list view) to bg, so the area
// below the rows paints dark. walk derives the bg from the system/theme — never
// dark — and resets it on ApplySysColors, so this must be re-applied afterwards.
// A single shared callback is reused (syscall.NewCallback leaks); GUI-thread only.
var lvBkColor walk.Color

var setLVBkCB = syscall.NewCallback(func(h win.HWND, _ uintptr) uintptr {
	var buf [80]uint16
	win.GetClassName(h, &buf[0], len(buf))
	if syscall.UTF16ToString(buf[:]) == "SysListView32" {
		win.SendMessage(h, win.LVM_SETBKCOLOR, 0, uintptr(lvBkColor))
		win.SendMessage(h, win.LVM_SETTEXTBKCOLOR, 0, uintptr(lvBkColor))
	}
	return 1
})

func darkenListViewBodies(container win.HWND, bg walk.Color) {
	lvBkColor = bg
	win.EnumChildWindows(container, setLVBkCB, 0)
}
