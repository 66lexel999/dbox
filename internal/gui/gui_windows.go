//go:build windows

// Package gui hosts D BOX's web UI inside native WebView2 windows (the runtime
// ships with Windows 11), so DBox.exe is a real desktop app. It provides a
// native folder picker and separate top-level windows for the New Download
// dialog, the download-complete popup, and the per-connection progress view.
package gui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"

	"myidm/internal/config"
	"myidm/internal/procutil"
)

// ---- Win32 helpers (DPI awareness + topmost) -----------------------------
var (
	user32                            = syscall.NewLazyDLL("user32.dll")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDPIAware            = user32.NewProc("SetProcessDPIAware")
	procSetWindowPos                  = user32.NewProc("SetWindowPos")
	procGetWindowRect                 = user32.NewProc("GetWindowRect")
	procSetForegroundWindow           = user32.NewProc("SetForegroundWindow")
	procGetWindowPlacement            = user32.NewProc("GetWindowPlacement")
	procSetWindowPlacement            = user32.NewProc("SetWindowPlacement")
	procGetSystemMetrics              = user32.NewProc("GetSystemMetrics")
	procGetForegroundWindow           = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessId      = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput             = user32.NewProc("AttachThreadInput")
	procBringWindowToTop              = user32.NewProc("BringWindowToTop")
	procShowWindow                    = user32.NewProc("ShowWindow")
	procSetFocus                      = user32.NewProc("SetFocus")
	procAllowSetForeground            = user32.NewProc("AllowSetForegroundWindow")
	procGetDpiForWindow               = user32.NewProc("GetDpiForWindow")
	procSetWindowsHookEx              = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx           = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx                = user32.NewProc("CallNextHookEx")

	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")

	comctl32              = syscall.NewLazyDLL("comctl32.dll")
	procSetWindowSubclass = comctl32.NewProc("SetWindowSubclass")
	procDefSubclassProc   = comctl32.NewProc("DefSubclassProc")
)

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
)

const swRestore = 9 // SW_RESTORE
const swHide = 0    // SW_HIDE

const (
	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040
)

// EnableDPIAwareness makes the process per-monitor DPI aware so WebView2 renders
// at native resolution instead of being bitmap-stretched by Windows (the blurry
// "low-res" look). Must run before any window is created — call it first in main.
func EnableDPIAwareness() {
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 == (HANDLE)-4 (Win10 1703+/Win11).
	if procSetProcessDpiAwarenessContext.Find() == nil {
		if r, _, _ := procSetProcessDpiAwarenessContext.Call(^uintptr(3)); r != 0 {
			return
		}
	}
	procSetProcessDPIAware.Call() // legacy system-DPI fallback
}

// AllowForeground lets the next process this one spawns pull itself to the
// foreground. Called by the parent (which currently holds the foreground) right
// before spawning a popup process, so the child's SetForegroundWindow isn't
// blocked by Windows' foreground lock (which otherwise just flashes the taskbar
// button). ASFW_ANY == (DWORD)-1.
func AllowForeground() {
	procAllowSetForeground.Call(uintptr(0xFFFFFFFF))
}

// setTopmost pins the window above other windows and brings it to the
// foreground with focus, so popups appear on top of everything (browser, other
// apps) instead of flashing in the taskbar.
func setTopmost(w webview.WebView) {
	forceForeground(uintptr(w.Window()))
}

// forceForeground reliably brings hwnd to the front *with focus*, even from a
// background/child process. Windows denies a plain SetForegroundWindow from a
// process that isn't already foreground (it just flashes the taskbar button)
// unless the calling thread shares input state with the current foreground
// thread — so we briefly AttachThreadInput to it, then steal focus.
func forceForeground(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	fg, _, _ := procGetForegroundWindow.Call()
	cur, _, _ := procGetCurrentThreadId.Call()
	fgThread, _, _ := procGetWindowThreadProcessId.Call(fg, 0)
	attached := false
	if fg != 0 && fgThread != 0 && fgThread != cur {
		if r, _, _ := procAttachThreadInput.Call(cur, fgThread, 1); r != 0 {
			attached = true
		}
	}
	procShowWindow.Call(hwnd, swRestore) // in case it came up minimized
	// Jump to the top of the z-order, then drop the always-on-top flag, so the
	// window leads but a modal child (the folder browser) can still open above it.
	const hwndTopmost = ^uintptr(0)   // (HWND)-1
	const hwndNoTopmost = ^uintptr(1) // (HWND)-2
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	procSetWindowPos.Call(hwnd, hwndNoTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	procSetFocus.Call(hwnd)
	if attached {
		procAttachThreadInput.Call(cur, fgThread, 0)
	}
}

// ---- window geometry persistence (main window only) ----------------------
// The main window remembers its size, position, and maximized state across
// launches via Win32 GetWindowPlacement/SetWindowPlacement, stored as
// <dataDir>/window.json. Using WINDOWPLACEMENT (not GetWindowRect) means a
// maximized window restores maximized while still tracking the underlying
// "normal" size to un-maximize back to.

type winPoint struct{ X, Y int32 }
type winRect struct{ Left, Top, Right, Bottom int32 }

type windowPlacement struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    winPoint
	PtMaxPosition    winPoint
	RcNormalPosition winRect
}

const (
	swShowNormal    = 1
	swShowMinimized = 2
	swShowMaximized = 3

	wpfRestoreToMaximized = 0x0002

	smCXScreen        = 0
	smCYScreen        = 1
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// winGeom is the persisted shape of the window (JSON in window.json).
type winGeom struct {
	X, Y, W, H int32
	Max        bool
}

func geomPath(dataDir string) string { return filepath.Join(dataDir, "window.json") }

func loadGeom(dataDir string) (winGeom, bool) {
	b, err := os.ReadFile(geomPath(dataDir))
	if err != nil {
		return winGeom{}, false
	}
	var g winGeom
	if json.Unmarshal(b, &g) != nil || g.W <= 0 || g.H <= 0 {
		return winGeom{}, false
	}
	return g, true
}

func saveGeom(dataDir string, g winGeom) {
	if b, err := json.Marshal(g); err == nil {
		_ = os.WriteFile(geomPath(dataDir), b, 0o644)
	}
}

func sysMetric(i int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(i))
	return int32(r) // truncates correctly for negative virtual-screen origins
}

// sanitizeGeom clamps a saved geometry to a sane size and keeps the title bar
// reachable on the current virtual desktop (guards against a monitor that was
// unplugged since the size was saved).
func sanitizeGeom(g winGeom) winGeom { return sanitizeGeomMin(g, 480, 360) }

// sanitizeGeomMin is sanitizeGeom with a caller-chosen minimum size — the small
// secondary popups use a tiny floor so a remembered compact size isn't forced up
// to the main window's 480×360 minimum.
func sanitizeGeomMin(g winGeom, minW, minH int32) winGeom {
	vx, vy := sysMetric(smXVirtualScreen), sysMetric(smYVirtualScreen)
	vw, vh := sysMetric(smCXVirtualScreen), sysMetric(smCYVirtualScreen)
	if vw <= 0 || vh <= 0 {
		return g // metrics unavailable; trust the saved values
	}
	if g.W < minW {
		g.W = minW
	}
	if g.H < minH {
		g.H = minH
	}
	if g.W > vw {
		g.W = vw
	}
	if g.H > vh {
		g.H = vh
	}
	// Keep at least ~120px of the window's top edge on-screen.
	if g.X+g.W < vx+120 {
		g.X = vx
	}
	if g.X > vx+vw-120 {
		g.X = vx + vw - g.W
	}
	if g.Y < vy {
		g.Y = vy
	}
	if g.Y > vy+vh-80 {
		g.Y = vy + vh - g.H
	}
	return g
}

func getGeom(h uintptr) (winGeom, bool) {
	if h == 0 {
		return winGeom{}, false
	}
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	if r, _, _ := procGetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&wp))); r == 0 {
		return winGeom{}, false
	}
	rc := wp.RcNormalPosition
	g := winGeom{X: rc.Left, Y: rc.Top, W: rc.Right - rc.Left, H: rc.Bottom - rc.Top}
	g.Max = wp.ShowCmd == swShowMaximized ||
		(wp.ShowCmd == swShowMinimized && wp.Flags&wpfRestoreToMaximized != 0)
	return g, true
}

func applyGeom(h uintptr, g winGeom) { applyGeomMin(h, g, 480, 360) }

func applyGeomMin(h uintptr, g winGeom, minW, minH int32) {
	if h == 0 {
		return
	}
	g = sanitizeGeomMin(g, minW, minH)
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	procGetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&wp))) // seed PtMin/PtMax
	wp.Length = uint32(unsafe.Sizeof(wp))
	wp.Flags = 0
	if g.Max {
		wp.ShowCmd = swShowMaximized
	} else {
		wp.ShowCmd = swShowNormal
	}
	wp.RcNormalPosition = winRect{Left: g.X, Top: g.Y, Right: g.X + g.W, Bottom: g.Y + g.H}
	procSetWindowPlacement.Call(h, uintptr(unsafe.Pointer(&wp)))
}

// watchGeom polls the window every 500ms and persists its geometry whenever it
// changes, so the latest size/position survives even an abrupt close. Stops
// when stop is closed (on window destroy). GetWindowPlacement is a thread-safe
// query, so polling off the UI thread is fine.
func watchGeom(w webview.WebView, dataDir string, stop <-chan struct{}) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last winGeom
	have := false
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			g, ok := getGeom(uintptr(w.Window()))
			if !ok || g.W <= 0 || g.H <= 0 {
				continue
			}
			if !have || g != last {
				saveGeom(dataDir, g)
				last, have = g, true
			}
		}
	}
}

// ---- flash-free reveal ---------------------------------------------------
// go-webview2 creates the window and ShowWindows it immediately, THEN cold-starts
// the WebView2 runtime (~1-2s for the FIRST window in the session). For that gap
// an empty window sits on screen — the Windows open-animation in white, then
// WebView2's black surface — before the dark HTML paints (the user's "white →
// black → UI"). The library exposes no background-color hook.
//
// Fix: DWM-cloak the window the instant it's created. A cloaked window is still
// SW_SHOWN and at its real position (so WebView2 keeps compositing — unlike
// SW_HIDE, which blanks the surface, and unlike off-screen parking, which makes
// the window visibly vanish and reappear), but DWM renders nothing for it. We
// position it (centered / restored) while cloaked, and uncloak only once the
// page reports first paint (window.myidmReady) or a safety timeout fires — so it
// appears, fully rendered, exactly once and in place.

var (
	dwmapi                    = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

const dwmwaCloak = 13                 // DWMWA_CLOAK
const dwmwaUseImmersiveDarkMode = 20  // Win10 2004+/Win11 (older 1809-1903 used 19)

// readyTimeout reveals the window even if the page never calls myidmReady (an
// older page, a load error). Worst case matches the old flash duration.
const readyTimeout = 2500 * time.Millisecond

func toCoord(v int) uintptr { return uintptr(uint32(int32(v))) } // signed -> SetWindowPos arg

// cloakWindow hides (cloak=true) or shows (cloak=false) a window via DWM without
// changing its SW_SHOWN visibility, so WebView2 keeps rendering throughout.
func cloakWindow(hwnd uintptr, cloak bool) {
	var v int32
	if cloak {
		v = 1
	}
	procDwmSetWindowAttribute.Call(hwnd, dwmwaCloak, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
}

// setDarkTitleBar paints a window's native title bar (caption + frame) in dark
// mode via DWM, so the popup chrome matches the app's dark theme instead of the
// bright white default. Tries the Win10-2004+/Win11 attribute first, then the
// older 1809-1903 one; a no-op on builds that support neither.
func setDarkTitleBar(hwnd uintptr) {
	var on int32 = 1
	if r, _, _ := procDwmSetWindowAttribute.Call(hwnd, dwmwaUseImmersiveDarkMode,
		uintptr(unsafe.Pointer(&on)), unsafe.Sizeof(on)); r != 0 {
		procDwmSetWindowAttribute.Call(hwnd, 19, uintptr(unsafe.Pointer(&on)), unsafe.Sizeof(on))
	}
}

// hideForClose makes a popup vanish instantly and cleanly the moment a close is
// initiated — BEFORE go-webview2 tears the WebView2 surface down. SW_HIDE removes
// the window from the screen synchronously (unlike a DWM cloak, which only takes
// effect on the next compositor frame, so a dying frame can still flash); the
// cloak is kept as a backstop in case a stray paint slips in. Without this the
// contentless window flashes the desktop / the window behind it / a black
// surface as it closes. The main window doesn't need it — it's Wails-hosted and
// manages its own teardown, which is why only the popups flickered.
func hideForClose(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	cloakWindow(hwnd, true)
	procShowWindow.Call(hwnd, swHide)
}

const (
	whCBT         = 5 // WH_CBT
	hcbtCreatewnd = 3 // HCBT_CREATEWND
)

// cbtCloakCallback cloaks every top-level window created on a hooked thread, at
// HCBT_CREATEWND — i.e. BEFORE go-webview2's internal ShowWindow runs, so the
// empty window frame never flashes on screen. It's a single stateless callback
// (syscall.NewCallback handles are never freed, so we must not make one per
// call); concurrency is fine because the hook itself is thread-local. DWM cloak
// is a no-op on the child windows WebView2 creates later, so only the top-level
// window is affected. CallNextHookEx's first arg is documented as ignored.
var cbtCloakCallback = syscall.NewCallback(func(nCode, wParam, lParam uintptr) uintptr {
	if nCode == hcbtCreatewnd {
		cloakWindow(wParam, true) // wParam = HWND of the window being created
	}
	ret, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return ret
})

// closeCloakSubclass DWM-cloaks a popup the instant a close begins (WM_CLOSE or
// WM_DESTROY) — BEFORE go-webview2's wndproc runs DestroyWindow and tears the
// WebView2 surface down — so the bare, null-background-brush native client area
// never flashes white as the window closes. It is the symmetric counterpart to
// the create-time cloak (cbtCloakCallback). A window subclass (not the JS close
// binds) is required so the native title-bar X is covered too. Single stateless
// package-level callback (syscall.NewCallback handles are never freed). We only
// cloak and ALWAYS chain to DefSubclassProc so the library still closes the
// window; no RemoveWindowSubclass needed (comctl32 detaches on destroy).
var closeCloakSubclass = syscall.NewCallback(func(hwnd, msg, wParam, lParam, idSubclass, refData uintptr) uintptr {
	if msg == wmClose || msg == wmDestroy {
		cloakWindow(hwnd, true)
		procShowWindow.Call(hwnd, swHide) // synchronous removal — beats DWM's next-frame cloak
	}
	ret, _, _ := procDefSubclassProc.Call(hwnd, msg, wParam, lParam)
	return ret
})

// makeWindowCloaked creates a webview window that is already DWM-cloaked, so the
// library's ShowWindow shows nothing until the caller uncloaks it on first paint.
func makeWindowCloaked(title string, width, height uint, center bool, dataPath string) webview.WebView {
	tid, _, _ := procGetCurrentThreadId.Call()
	hook, _, _ := procSetWindowsHookEx.Call(whCBT, cbtCloakCallback, 0, tid)
	w := makeWindow(title, width, height, center, dataPath)
	if hook != 0 {
		procUnhookWindowsHookEx.Call(hook)
	}
	return w
}

// setWebView2Env keeps WebView2 painting while a window is briefly cloaked:
// without it Chromium's native-window occlusion check can pause compositing and
// the window uncloaks blank. Set once, before the first WebView2 is created.
func setWebView2Env() {
	const key = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
	if os.Getenv(key) == "" {
		os.Setenv(key, "--disable-features=CalculateNativeWinOcclusion")
	}
}

// centerWindow positions a window of w×h centered on the primary screen.
func centerWindow(hwnd uintptr, w, h int) {
	sw, sh := int(sysMetric(smCXScreen)), int(sysMetric(smCYScreen))
	x, y := (sw-w)/2, (sh-h)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	procSetWindowPos.Call(hwnd, 0, toCoord(x), toCoord(y), uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate)
}

func makeWindow(title string, width, height uint, center bool, dataPath string) webview.WebView {
	return webview.NewWithOptions(webview.WebViewOptions{
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview.WindowOptions{
			Title: title, Width: width, Height: height, Center: center,
		},
	})
}

// popupDataPath returns a STABLE WebView2 user-data folder for a popup-window
// type, reused across launches. A brand-new (empty) WebView2 profile drops its
// first navigation and renders blank — which is why the completion / New
// Download / status popups were showing up empty: the old code handed each one
// a fresh MkdirTemp folder every launch, so the profile was always cold. The
// main window never hit this because it reuses dataDir/webview. One warm folder
// per type fixes it; WebView2 permits a user-data folder shared across
// processes, so two same-type popups at once still work.
func popupDataPath(tag string) string {
	p := filepath.Join(os.TempDir(), "dbox-webview-"+tag)
	_ = os.MkdirAll(p, 0o755)
	return p
}

// popupGeomDir is the STABLE per-popup folder where a window's remembered
// size/position lives (in the data dir, so it survives a %TEMP% clear, unlike
// the WebView2 user-data folder). Each popup type gets its own window.json.
func popupGeomDir(tag string) string {
	p := filepath.Join(config.DefaultDataDir(), "wingeom", tag)
	_ = os.MkdirAll(p, 0o755)
	return p
}

// SaveWinSize / LoadWinSize persist a single window's width/height for the
// "remember size" feature (used by the Wails main window, which manages its own
// frame so it can't use the Win32 placement helpers above).
func SaveWinSize(key string, w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	dir := filepath.Join(config.DefaultDataDir(), "wingeom")
	_ = os.MkdirAll(dir, 0o755)
	if b, err := json.Marshal(map[string]int{"w": w, "h": h}); err == nil {
		_ = os.WriteFile(filepath.Join(dir, key+".json"), b, 0o644)
	}
}

func LoadWinSize(key string) (w, h int, ok bool) {
	b, err := os.ReadFile(filepath.Join(config.DefaultDataDir(), "wingeom", key+".json"))
	if err != nil {
		return 0, 0, false
	}
	var m map[string]int
	if json.Unmarshal(b, &m) != nil || m["w"] <= 0 || m["h"] <= 0 {
		return 0, 0, false
	}
	return m["w"], m["h"], true
}

// RunMain opens the main MyIDM window pointed at the running server and blocks
// until it is closed; onClose (if set) then runs to trigger shutdown. Must be
// called on the main goroutine.
func RunMain(serverURL, dataDir string, onClose func()) error {
	runtime.LockOSThread()
	EnableDPIAwareness()
	setWebView2Env()

	// Reopen at the last-used size/position (falls back to a centered default).
	width, height := uint(1080), uint(760)
	saved, hasSaved := loadGeom(dataDir)
	if hasSaved {
		saved = sanitizeGeom(saved)
		width, height = uint(saved.W), uint(saved.H)
	}

	wd := filepath.Join(dataDir, "webview")
	SetSharedWebviewData(wd) // in-process popups reuse this warm browser process
	w := makeWindowCloaked("D BOX — Download Manager", width, height, false, wd)
	if w == nil {
		return fmt.Errorf("could not create a WebView2 window (is the Microsoft Edge WebView2 Runtime installed?)")
	}
	defer w.Destroy()
	bindPickers(w)

	// Hide the WebView2 cold-start flashes: cloak the window (DWM renders nothing,
	// but WebView2 keeps painting), position it where it will live, and uncloak
	// only once the page has painted — so it appears once, in place, fully drawn.
	hwnd := uintptr(w.Window())
	setDarkTitleBar(hwnd) // dark caption to match the app's dark chrome
	// Re-cloak on close (every path, incl. the native X) so the window never
	// flashes white as it tears down — mirror of the create-time cloak.
	procSetWindowSubclass.Call(hwnd, closeCloakSubclass, 1, 0)
	cloakWindow(hwnd, true)
	if hasSaved {
		applyGeom(hwnd, saved) // restore exact position + maximized state
	} else {
		centerWindow(hwnd, int(width), int(height))
	}

	stop := make(chan struct{})
	var once sync.Once
	reveal := func() {
		once.Do(func() {
			cloakWindow(hwnd, false) // appears in place, already rendered
			forceForeground(hwnd)
			go watchGeom(w, dataDir, stop) // only track geometry once on-screen
		})
	}
	w.Bind("myidmReady", func() { w.Dispatch(reveal) })
	go func() { time.Sleep(readyTimeout); w.Dispatch(reveal) }()

	w.Navigate(serverURL)
	w.Run()
	close(stop)

	if onClose != nil {
		onClose()
	}
	return nil
}

// RunDialog opens the topmost "New Download" window for target, hosting the
// server's /add page with native folder browsing and a native close.
// resizeLogical sizes a window to logW×logH device-independent pixels (scaled by
// its monitor DPI) and centers it on the primary screen, so a popup fits its
// content at any DPI instead of being created in raw physical pixels.
func resizeLogical(hwnd uintptr, logW, logH int) {
	dpi := 96
	if procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(hwnd); d > 0 {
			dpi = int(d)
		}
	}
	w := logW * dpi / 96
	h := logH * dpi / 96
	sw, sh := int(sysMetric(smCXScreen)), int(sysMetric(smCYScreen))
	x, y := (sw-w)/2, (sh-h)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate)
}

// centerPhysical sizes a window to w×h PHYSICAL pixels and centers it on the
// primary screen. Used to reopen a popup at its remembered SIZE while resetting
// its POSITION to center every time (so it never reappears off in a corner).
func centerPhysical(hwnd uintptr, w, h int) {
	sw, sh := int(sysMetric(smCXScreen)), int(sysMetric(smCYScreen))
	x, y := (sw-w)/2, (sh-h)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate)
}

// recenter moves a window to the center of the primary screen using its TRUE
// physical size (GetWindowRect, unlike GetWindowPlacement, returns real pixels),
// so it works regardless of the coordinate space the size was restored from.
func recenter(hwnd uintptr) {
	var r winRect
	if ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		return
	}
	centerPhysical(hwnd, int(r.Right-r.Left), int(r.Bottom-r.Top))
}

// runPopup opens one of the secondary WebView2 windows (New Download /
// completion / status). On first open it sizes to logW×logH DIPs (DPI-correct,
// centered); after that it reopens at — and lives at — whatever size/position
// the user last left it (persisted to window.json in the window's data folder).
// runPopup hosts one secondary window. webviewData is the WebView2 user-data
// folder (in-process popups pass the MAIN window's folder so they reuse its warm
// browser process — opening in ~70ms instead of ~400ms+ for a cold folder);
// geomDir is where this popup type's size/position is remembered (kept separate
// so sharing the data folder doesn't clobber the main window's geometry).
func runPopup(title string, logW, logH int, webviewData, geomDir, nav, trackID string, extraBind func(webview.WebView)) error {
	runtime.LockOSThread()
	EnableDPIAwareness()
	setWebView2Env()

	w := makeWindowCloaked(title, uint(logW), uint(logH), false, webviewData)
	if w == nil {
		return fmt.Errorf("could not create a WebView2 window")
	}
	defer w.Destroy()
	w.Bind("myidmCloseDialog", func() { hideForClose(uintptr(w.Window())); w.Terminate() })
	w.Bind("myidmShowInFolder", func(path string) { RevealInExplorer(path) }) // foreground-correct "Open folder"
	if extraBind != nil {
		extraBind(w)
	}
	// When running in-process, register the status window by task id so the
	// completion popup's Close can dismiss it too.
	if trackID != "" {
		popupMu.Lock()
		detailByID[trackID] = w
		popupMu.Unlock()
		defer func() {
			popupMu.Lock()
			if detailByID[trackID] == w {
				delete(detailByID, trackID)
			}
			popupMu.Unlock()
		}()
	}

	// Cloak during the WebView2 cold start; position it, then uncloak once the
	// page paints (or the safety timeout fires) so it appears once, fully drawn.
	hwnd := uintptr(w.Window())
	setDarkTitleBar(hwnd) // dark caption to match the app's dark chrome
	// Re-cloak on close (every path, incl. the native X) so the window never
	// flashes white as it tears down — mirror of the create-time cloak.
	procSetWindowSubclass.Call(hwnd, closeCloakSubclass, 1, 0)
	cloakWindow(hwnd, true)
	if g, ok := loadGeom(geomDir); ok {
		applyGeomMin(hwnd, g, 220, 150) // restore the remembered SIZE (correct in placement space)
		recenter(hwnd)                  // then reset POSITION to center every open
	} else {
		resizeLogical(hwnd, logW, logH) // first ever open: centered at the default size
	}

	stop := make(chan struct{})
	go watchGeom(w, geomDir, stop) // remember size/position — saved whenever it changes
	var once sync.Once
	reveal := func() {
		once.Do(func() {
			cloakWindow(hwnd, false)
			setTopmost(w)
		})
	}
	w.Bind("myidmReady", func() { w.Dispatch(reveal) })
	go func() { time.Sleep(readyTimeout); w.Dispatch(reveal) }()

	// Navigate from inside the message loop (not before Run): a fresh WebView2
	// can otherwise drop the first navigation and render blank.
	w.Dispatch(func() { w.Navigate(nav) })
	w.Run()
	close(stop)
	return nil
}

// In-process popup registry: the per-download status (detail) window keyed by
// task id, so the completion popup can close it ("Close" dismisses both windows).
var (
	popupMu    sync.Mutex
	detailByID = map[string]webview.WebView{}

	// sharedWebviewData is the MAIN window's WebView2 user-data folder. In-process
	// popups reuse it so they connect to the already-running browser process and
	// open in ~70ms instead of cold-starting their own (~400ms+ per folder).
	sharedWebviewData string
)

// SetSharedWebviewData records the main window's WebView2 data folder for the
// in-process popups to reuse. Call before any popup opens.
func SetSharedWebviewData(p string) { sharedWebviewData = p }

// popupWebviewData picks a popup's WebView2 folder: the shared (warm) main folder
// when set, else a per-type folder (the separate-process / Gio fallback).
func popupWebviewData(tag string) string {
	if sharedWebviewData != "" {
		return sharedWebviewData
	}
	return popupDataPath(tag)
}

// CloseDetail terminates the in-process status window for a task, if one is open.
// WebView.Terminate is safe to call from any thread.
func CloseDetail(id string) {
	popupMu.Lock()
	w := detailByID[id]
	popupMu.Unlock()
	if w != nil {
		hideForClose(uintptr(w.Window()))
		w.Terminate()
	}
}

func dialogNav(serverURL, query string) string {
	nav := strings.TrimRight(serverURL, "/") + "/add?dialog=1"
	if query != "" {
		nav += "&" + query
	}
	return nav
}
func doneNav(serverURL, id string) string {
	return strings.TrimRight(serverURL, "/") + "/done?id=" + url.QueryEscape(id)
}
func detailNav(serverURL, id string) string {
	return strings.TrimRight(serverURL, "/") + "/detail?id=" + url.QueryEscape(id)
}

// doneBind lets the completion popup's Close (myidmCloseDownload) also dismiss
// the matching status window when both run in-process.
func doneBind(id string) func(webview.WebView) {
	return func(w webview.WebView) {
		w.Bind("myidmCloseDownload", func() { hideForClose(uintptr(w.Window())); CloseDetail(id); w.Terminate() })
	}
}

// RunDialog/RunDone/RunDetail BLOCK (used by the separate-process -dialog/-done/
// -detail modes the Gio main path spawns; each gets its own data folder).
// OpenDialog/OpenDone/OpenDetail are the IN-PROCESS, non-blocking equivalents the
// WebView2 main path uses: they reuse the main window's warm browser process
// (fast) and let the main process coordinate the windows.

func RunDialog(serverURL, query string) error {
	return runPopup("Download File Info — D BOX", 429, 263, popupDataPath("dialog"), popupGeomDir("dialog"), dialogNav(serverURL, query), "", bindPickers)
}
func RunDone(serverURL, id string) error {
	return runPopup("Download complete — D BOX", 327, 165, popupDataPath("done"), popupGeomDir("done"), doneNav(serverURL, id), "", doneBind(id))
}
func RunDetail(serverURL, id string) error {
	return runPopup("Download status — D BOX", 464, 350, popupDataPath("detail"), popupGeomDir("detail"), detailNav(serverURL, id), id, nil)
}

// OpenDialog opens the New Download window in-process (non-blocking).
func OpenDialog(serverURL, query string) {
	go runPopup("Download File Info — D BOX", 429, 263, popupWebviewData("dialog"), popupGeomDir("dialog"), dialogNav(serverURL, query), "", bindPickers)
}

// OpenDone opens the completion window in-process (non-blocking).
func OpenDone(serverURL, id string) {
	go runPopup("Download complete — D BOX", 327, 165, popupWebviewData("done"), popupGeomDir("done"), doneNav(serverURL, id), "", doneBind(id))
}

// OpenDetail opens (or, if already open, focuses) the status window for a task
// in-process (non-blocking).
func OpenDetail(serverURL, id string) {
	popupMu.Lock()
	existing := detailByID[id]
	popupMu.Unlock()
	if existing != nil { // already open — just bring it to the front
		forceForeground(uintptr(existing.Window()))
		return
	}
	go runPopup("Download status — D BOX", 464, 350, popupWebviewData("detail"), popupGeomDir("detail"), detailNav(serverURL, id), id, nil)
}

// RevealInExplorer opens Explorer with the file selected and brings it to the
// FRONT. Meant to run from a popup window (which is foreground when the user
// clicks "Open folder"), so the spawned Explorer is allowed to take focus —
// AllowForeground grants the right. Fixes Explorer opening behind the app (the
// user previously had to click the taskbar Explorer icon to see the window).
func RevealInExplorer(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	AllowForeground()
	cmd := exec.Command("explorer.exe", "/select,", filepath.Clean(path))
	procutil.Hidden(cmd)
	cmd.Start()
}

// bindPickers exposes the native folder picker to the page as
// window.myidmPickFolder(initialDir) -> Promise<string> ("" if cancelled).
func bindPickers(w webview.WebView) {
	owner := uintptr(w.Window())
	w.Bind("myidmPickFolder", func(initial string) (string, error) {
		return pickFolder(initial, owner)
	})
}

// PickFolder shows the native "Select Folder" dialog and returns the chosen path
// ("" if cancelled). It runs on a dedicated, freshly-locked OS thread so the
// modern COM dialog's STA requirement is met regardless of the caller's thread
// (e.g. a Gio event-loop goroutine). Blocks until the user chooses or cancels.
func PickFolder(initial string) string {
	ch := make(chan string, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		p, err := pickFolderModern(initial, 0)
		if err != nil {
			p, _ = pickFolderLegacy(initial)
		}
		ch <- p
	}()
	return <-ch
}

// pickFolder shows the modern Vista+ "Select Folder" dialog, owned by the
// calling window. It falls back to the legacy PowerShell dialog only if the
// modern one can't be created (e.g. the thread isn't an STA apartment).
func pickFolder(initial string, owner uintptr) (string, error) {
	if p, err := pickFolderModern(initial, owner); err == nil {
		return p, nil // "" => user cancelled
	}
	return pickFolderLegacy(initial)
}

// pickFolderLegacy shows the old WinForms folder-browser dialog via PowerShell
// (STA) and returns the chosen path, or "" if cancelled.
func pickFolderLegacy(initial string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Add-Type -AssemblyName System.Windows.Forms | Out-Null;")
	sb.WriteString("$f = New-Object System.Windows.Forms.FolderBrowserDialog;")
	sb.WriteString("$f.Description = 'Choose a folder to save this download';")
	sb.WriteString("$f.ShowNewFolderButton = $true;")
	if d := strings.TrimSpace(initial); d != "" {
		sb.WriteString("$f.SelectedPath = '" + strings.ReplaceAll(d, "'", "''") + "';")
	}
	sb.WriteString("if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($f.SelectedPath) }")

	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-NonInteractive", "-Command", sb.String())
	procutil.Hidden(cmd) // the FolderBrowserDialog still shows; only the cmd window is suppressed
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}
