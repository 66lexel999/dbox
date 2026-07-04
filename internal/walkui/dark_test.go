//go:build windows

package walkui

import "testing"

func TestDarkOrdinals(t *testing.T) {
	enableDarkModeApp()
	t.Logf("SetPreferredAppMode=%#x FlushMenuThemes=%#x AllowDarkModeForWindow=%#x",
		uxSetPreferredAppModePtr, uxFlushMenuThemesPtr, uxAllowDarkModeForWindowPtr)
	if uxSetPreferredAppModePtr == 0 {
		t.Error("uxtheme!SetPreferredAppMode (ordinal 135) did not resolve")
	}
}
