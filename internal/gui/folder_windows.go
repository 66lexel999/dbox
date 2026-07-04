//go:build windows

package gui

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Modern "Select Folder" picker via the Vista+ Common Item Dialog
// (IFileOpenDialog with FOS_PICKFOLDERS). Replaces the dated WinForms
// FolderBrowserDialog (the XP-style tree) the PowerShell helper used to show.
// Implemented with raw COM vtable calls so there's no extra dependency and no
// console-spawning helper process. COM handles are kept as unsafe.Pointer (not
// uintptr) so the GC and `go vet` are happy; the targets are native COM memory.

var (
	modole32             = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = modole32.NewProc("CoInitializeEx")
	procCoUninitialize   = modole32.NewProc("CoUninitialize")
	procCoCreateInstance = modole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = modole32.NewProc("CoTaskMemFree")

	modshell32                      = syscall.NewLazyDLL("shell32.dll")
	procSHCreateItemFromParsingName = modshell32.NewProc("SHCreateItemFromParsingName")
)

type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = comGUID{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog   = comGUID{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidShellItem        = comGUID{0x43826D1E, 0xE718, 0x42EE, [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

const (
	coinitApartmentThreaded = 0x2
	clsctxInprocServer      = 0x1
	fosPickFolders          = 0x20
	fosForceFileSystem      = 0x40
	sigdnFileSysPath        = 0x80058000

	// IFileOpenDialog vtable indices (IUnknown 0-2, IModalWindow 3, IFileDialog 4-26).
	miRelease    = 2
	miShow       = 3
	miSetOptions = 9
	miGetOptions = 10
	miSetFolder  = 12
	miGetResult  = 20
	// IShellItem.
	miGetDisplayName = 5
)

// comCall invokes COM method idx on obj (the this-pointer is passed first).
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj) // object's first field points at its vtable
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	ret, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return ret
}

// lpwstr copies a NUL-terminated UTF-16 string from native memory.
func lpwstr(p unsafe.Pointer) string {
	if p == nil {
		return ""
	}
	var u []uint16
	for i := 0; ; i++ {
		c := *(*uint16)(unsafe.Add(p, uintptr(i)*2))
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return syscall.UTF16ToString(u)
}

// pickFolderModern shows the modern folder dialog and returns the chosen path.
// A non-nil error means the dialog couldn't be shown (caller may fall back); a
// nil error with an empty path means the user cancelled.
func pickFolderModern(initial string, owner uintptr) (string, error) {
	hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	switch uint32(hr) {
	case 0: // S_OK — we initialized COM; balance it
		defer procCoUninitialize.Call()
	case 1: // S_FALSE — already initialized on this (STA) thread
	default: // RPC_E_CHANGED_MODE etc. — thread isn't STA, can't host the dialog
		return "", fmt.Errorf("COM not STA on this thread (hr=%#x)", uint32(hr))
	}

	var dlg unsafe.Pointer
	if r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dlg))); r != 0 || dlg == nil {
		return "", fmt.Errorf("CoCreateInstance FileOpenDialog failed (hr=%#x)", uint32(r))
	}
	defer comCall(dlg, miRelease)

	var opts uint32
	comCall(dlg, miGetOptions, uintptr(unsafe.Pointer(&opts)))
	comCall(dlg, miSetOptions, uintptr(opts|fosPickFolders|fosForceFileSystem))

	if initial != "" {
		if p, err := syscall.UTF16PtrFromString(initial); err == nil {
			var item unsafe.Pointer
			r, _, _ := procSHCreateItemFromParsingName.Call(
				uintptr(unsafe.Pointer(p)), 0,
				uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
			if r == 0 && item != nil {
				comCall(dlg, miSetFolder, uintptr(item))
				comCall(item, miRelease)
			}
		}
	}

	if comCall(dlg, miShow, owner) != 0 { // non-S_OK: cancelled (ERROR_CANCELLED) or error
		return "", nil
	}

	var item unsafe.Pointer
	if comCall(dlg, miGetResult, uintptr(unsafe.Pointer(&item))) != 0 || item == nil {
		return "", nil
	}
	defer comCall(item, miRelease)

	var psz unsafe.Pointer
	if comCall(item, miGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&psz))) != 0 || psz == nil {
		return "", nil
	}
	path := lpwstr(psz)
	procCoTaskMemFree.Call(uintptr(psz))
	return path, nil
}
