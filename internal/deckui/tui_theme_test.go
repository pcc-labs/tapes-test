package deckui

import "testing"

// TestIsDarkThemeNeverQueriesTerminalAtRuntime pins the fix for the chat-pane
// freeze: termenv.HasDarkBackground writes OSC 11 + CSI 6n to the tty and
// blocks up to 5s reading the replies. Once bubbletea owns the terminal its
// input reader races termenv for those reply bytes — the UI freezes for the
// OSC timeout and the mangled replies ("11;rgb:0000/0000/0000[32;12R") are
// typed into whichever input has focus. So the query may run only during
// package init, before the TUI starts; isDarkTheme must read the cached
// result, never re-query.
func TestIsDarkThemeNeverQueriesTerminalAtRuntime(t *testing.T) {
	savedDetect := detectDarkBackground
	savedOverride := themeOverride
	defer func() {
		detectDarkBackground = savedDetect
		themeOverride = savedOverride
	}()

	queried := false
	detectDarkBackground = func() bool {
		queried = true
		return true
	}

	for _, override := range []string{"", "dark", "light"} {
		themeOverride = override
		isDarkTheme()
	}
	if queried {
		t.Fatal("isDarkTheme queried the terminal; it must use the value cached at package init")
	}
}
