//go:build windows

package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestBridgeStatusText(t *testing.T) {
	for _, id := range []int{idClaude, idCodex, idCopilot} {
		for _, pending := range []bool{false, true} {
			for _, on := range []bool{false, true} {
				want := "Paused"
				if pending {
					want = "Working..."
				} else if on {
					want = "Active"
				}
				if got := bridgeStatusText(id, on, pending); got != want {
					t.Errorf("bridgeStatusText(%d, %t, %t) = %q, want %q", id, on, pending, got, want)
				}
			}
		}
	}
}

func captureClient(t *testing.T, hwnd uintptr, width, height int) *image.NRGBA {
	t.Helper()
	dc := proc("GetDC", hwnd)
	defer proc("ReleaseDC", hwnd, dc)
	mem, _, _ := gdi.NewProc("CreateCompatibleDC").Call(dc)
	defer gdi.NewProc("DeleteDC").Call(mem)
	header := struct {
		Size                   uint32
		Width, Height          int32
		Planes, Bits           uint16
		Compression, ImageSize uint32
		X, Y                   int32
		Used, Important        uint32
	}{Size: 40, Width: int32(width), Height: -int32(height), Planes: 1, Bits: 32}
	var pixels uintptr
	bitmap, _, _ := gdi.NewProc("CreateDIBSection").Call(dc, uintptr(unsafe.Pointer(&header)), 0, uintptr(unsafe.Pointer(&pixels)), 0, 0)
	if bitmap == 0 || pixels == 0 {
		t.Fatal("could not create native capture bitmap")
	}
	defer gdi.NewProc("DeleteObject").Call(bitmap)
	old, _, _ := gdi.NewProc("SelectObject").Call(mem, bitmap)
	defer gdi.NewProc("SelectObject").Call(mem, old)
	proc("SendMessageW", hwnd, 0x317, mem, 0x14)
	gdi.NewProc("GdiFlush").Call()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	kernel.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&im.Pix[0])), pixels, uintptr(len(im.Pix)))
	for i := 0; i < len(im.Pix); i += 4 {
		im.Pix[i], im.Pix[i+2], im.Pix[i+3] = im.Pix[i+2], im.Pix[i], 255
	}
	return im
}

func visibleInkBounds(im *image.NRGBA, region image.Rectangle, background color.NRGBA) image.Rectangle {
	var bounds image.Rectangle
	for y := region.Min.Y; y < region.Max.Y; y++ {
		for x := region.Min.X; x < region.Max.X; x++ {
			if im.NRGBAAt(x, y) != background {
				pixel := image.Rect(x, y, x+1, y+1)
				if bounds.Empty() {
					bounds = pixel
				} else {
					bounds = bounds.Union(pixel)
				}
			}
		}
	}
	return bounds
}

func TestNativeAppearanceMatchesExistingInterface(t *testing.T) {
	// Retire this GUI thread when the test exits so queued Win32 messages cannot affect later tests.
	runtime.LockOSThread()
	awareness := user.NewProc("SetProcessDpiAwarenessContext")
	if awareness.Find() == nil {
		awareness.Call(^uintptr(3))
	} else {
		proc("SetProcessDPIAware")
	}
	if active != nil {
		t.Fatal("another native window owns this process")
	}
	w := &Window{dpi: 96, view: "idle", settings: map[string]any{"startAtLogin": true, "closeToTray": true},
		status: map[string]any{"version": "2.1.6", "username": "example", "codexEnabled": false, "claudeEnabled": true,
			"copilotEnabled": true, "copilotURL": "http://127.0.0.1:18925/v1",
			"usage": map[string]any{"creditsUsed": float64(12), "entitlement": float64(100)}}}
	if err := w.loadArtwork(); err != nil {
		t.Fatal(err)
	}
	active = w
	defer func() { active = nil }()
	w.fonts()
	class := windowClass{Procedure: syscall.NewCallback(windowProc), Name: wide("ModelBridgeAppearanceTest")}
	class.Size = uint32(unsafe.Sizeof(class))
	if proc("RegisterClassExW", uintptr(unsafe.Pointer(&class))) == 0 {
		t.Fatal("could not register isolated UI test class")
	}
	defer proc("UnregisterClassW", uintptr(unsafe.Pointer(class.Name)), 0)
	w.Handle = proc("CreateWindowExW", windowExtendedStyle, uintptr(unsafe.Pointer(class.Name)), uintptr(unsafe.Pointer(wide("Isolated UI test"))),
		windowStyle, 0, 0, windowWidth, windowHeight, 0, 0, 0, 0)
	if w.Handle == 0 {
		t.Fatal("could not create isolated native UI")
	}
	defer func() {
		proc("DestroyWindow", w.Handle)
		var m message
		for proc("PeekMessageW", uintptr(unsafe.Pointer(&m)), 0, 0x12, 0x12, 1) != 0 {
		}
	}()
	if err := w.build(); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.controls {
		if c.id == 1 && c.title != "ModelBridge" {
			t.Fatal("native branding must use ModelBridge", c.id, c.title)
		}
	}
	// Keep native calls on the window's creating thread; t.Run uses another goroutine.
	func() {
		if w.control(idTheme) != 0 || w.control(10) != 0 || w.control(11) != 0 {
			t.Fatal("header theme control or removed descriptions are still present")
		}
		selector := w.control(idAppearance)
		if proc("SendMessageW", selector, 0x146, 0, 0) != 3 ||
			proc("GetWindowLongW", selector, ^uintptr(15))&0x10003 != 0x10003 {
			t.Fatal("Appearance must be a keyboard-accessible native dropdown list")
		}
		for index, title := range []string{"Auto", "Light", "Dark"} {
			buffer := make([]uint16, 32)
			proc("SendMessageW", selector, 0x148, uintptr(index), uintptr(unsafe.Pointer(&buffer[0])))
			if syscall.UTF16ToString(buffer) != title {
				t.Fatal("unexpected appearance choice", index, syscall.UTF16ToString(buffer))
			}
		}
		originalView, originalAppearance := w.view, w.status["appearance"]
		defer func() { w.view, w.status["appearance"] = originalView, originalAppearance; w.render() }()
		w.view = "settings"
		w.status["appearance"] = "system"
		w.render()
		if proc("SendMessageW", selector, 0x147, 0, 0) != 0 ||
			w.palette != themePalette(systemDark() && !w.highContrast, w.highContrast) {
			t.Fatal("Auto must show the system theme without an override")
		}
		w.view = "idle"
		w.render()
		if proc("IsWindowVisible", selector) != 0 || proc("IsWindowVisible", w.control(idAppearanceLabel)) != 0 {
			t.Fatal("Appearance must only appear in Settings")
		}
	}()
	func() {
		originalStatus, originalSettings, originalView := w.status, w.settings, w.view
		defer func() {
			w.status, w.settings, w.view = originalStatus, originalSettings, originalView
			w.notice, w.refreshError = "", ""
			w.render()
		}()
		label := func(id int) string {
			buffer := make([]uint16, 256)
			proc("GetWindowTextW", w.control(id), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
			return strings.ReplaceAll(syscall.UTF16ToString(buffer), "&", "")
		}
		w.view = "starting"
		w.accept(event{path: "refresh", value: map[string]any{"ready": false, "connected": false}})
		if w.view != "starting" || label(13) != "Starting..." ||
			proc("IsWindowEnabled", w.control(idConnect)) != 0 ||
			proc("GetWindowLongW", w.control(13), ^uintptr(15))&0x10000000 == 0 {
			t.Fatal("loading offers sign-in before saved login has loaded")
		}
		for _, c := range w.controls {
			if c.id != 13 && proc("GetWindowLongW", c.handle, ^uintptr(15))&0x10000000 != 0 {
				t.Fatal("startup shows more than its plain status text", c.id)
			}
		}
		w.accept(event{path: "refresh", err: errors.New("Local bridge unavailable.")})
		if label(12) != w.refreshError || w.refreshError == "" {
			t.Fatal("startup hides the actual local-bridge failure")
		}
		for _, c := range w.controls {
			visible := proc("GetWindowLongW", c.handle, ^uintptr(15))&0x10000000 != 0
			if visible != (c.id == 12) {
				t.Fatal("startup failure shows more than its error text", c.id)
			}
		}
		w.post("/api/start", map[string]any{})
		if w.busy {
			t.Fatal("sign-in raced startup")
		}
		connected := map[string]any{"ready": true, "connected": true, "copilotEnabled": false}
		w.accept(event{path: "refresh", value: connected})
		if w.view != "connected" {
			t.Fatal("restored login did not open the connected screen")
		}
		w.accept(event{path: "refresh", err: errors.New("Local bridge unavailable.")})
		if w.notice != "" || w.refreshError != "Local bridge unavailable." || label(12) != w.refreshError {
			t.Fatal("polling error was lost or stored as an action failure")
		}
		w.accept(event{path: "refresh", value: connected})
		if w.refreshError != "" || label(12) != "" {
			t.Fatal("recovered polling left a stale connection error")
		}
		w.notice = "Could not enable bridge."
		w.accept(event{path: "refresh", value: connected})
		if w.notice != "Could not enable bridge." || label(12) != w.notice {
			t.Fatal("successful polling cleared a user action error")
		}
		w.notice = ""
		w.accept(event{path: "refresh", value: map[string]any{"ready": true, "connected": false}})
		if w.view != "idle" || label(idConnect) != "Not Connected" || proc("IsWindowEnabled", w.control(idConnect)) == 0 {
			t.Fatal("a genuinely signed-out account cannot sign in after startup")
		}
		w.status["ready"] = false
		w.view = "settings"
		w.command(idDone)
		if w.view != "starting" {
			t.Fatal("Done offered sign-in before initialization completed")
		}
	}()
	func() {
		originalStatus, originalView := w.status, w.view
		defer func() {
			w.closeAccounts()
			w.status, w.view, w.addingAccount = originalStatus, originalView, false
			w.render()
		}()
		accounts := []any{}
		for i := range 12 {
			accounts = append(accounts, map[string]any{"username": fmt.Sprintf("account-%d", i), "selected": i == 0})
		}
		w.status = map[string]any{"ready": true, "connected": true, "username": "account-0", "accounts": accounts}
		w.view = "connected"
		w.busy = true
		w.render()
		if proc("IsWindowEnabled", w.control(idAccounts)) == 0 {
			t.Fatal("a pending bridge action must not block opening the saved account list")
		}
		count := len(w.controls)
		if err := w.showAccounts(); err != nil {
			t.Fatal(err)
		}
		if len(w.popupAccounts) != 12 || w.accountPopup == 0 || w.accountList == 0 ||
			len(w.controls) != count+26 || w.control(idAccountDisconnect) == 0 {
			t.Fatal("popup does not expose every account with separate switch/remove buttons")
		}
		var popupBounds rect
		proc("GetClientRect", w.accountPopup, uintptr(unsafe.Pointer(&popupBounds)))
		popup := captureClient(t, w.accountPopup, int(popupBounds.Right), int(popupBounds.Bottom))
		background := color.NRGBA{R: byte(w.palette.panel >> 16), G: byte(w.palette.panel >> 8), B: byte(w.palette.panel), A: 255}
		if popup.NRGBAAt(4, int(popupBounds.Bottom)-4) != background {
			t.Fatal("the account menu footer has an unpainted bottom margin")
		}
		if proc("IsWindowEnabled", w.control(accountControlBase)) != 0 ||
			proc("IsWindowEnabled", w.control(accountControlBase+1)) != 0 ||
			proc("IsWindowEnabled", w.control(idAddAccount)) != 0 {
			t.Fatal("account mutations must wait for the pending bridge action")
		}
		w.accountCommand(accountControlBase + 1)
		w.command(idAddAccount)
		if w.accountPopup == 0 || w.addingAccount {
			t.Fatal("busy account commands must leave the popup and current operation alone")
		}
		w.busy = false
		w.render()
		if proc("IsWindowEnabled", w.control(accountControlBase)) == 0 ||
			proc("IsWindowEnabled", w.control(accountControlBase+1)) == 0 ||
			proc("IsWindowEnabled", w.control(idAddAccount)) == 0 {
			t.Fatal("the open list must become actionable when the bridge finishes")
		}
		for i := range 12 {
			for _, id := range []int{accountControlBase + i*2, accountControlBase + i*2 + 1} {
				if proc("GetParent", w.control(id)) != w.accountList {
					t.Fatal("account controls are not in the scrollable list")
				}
			}
		}
		w.scrollAccounts(10000)
		if w.accountOffset != 216 {
			t.Fatal("account scrolling is not clamped to the last full row")
		}
		w.scrollAccounts(0)
		proc("SetFocus", w.control(accountControlBase))
		if !w.accountKey(message{Message: 0x100, WParam: 9}) ||
			proc("GetFocus") != w.control(accountControlBase+1) {
			t.Fatal("Tab cannot focus the row's remove action")
		}
		if !w.accountKey(message{Message: 0x100, WParam: 27}) || w.accountPopup != 0 ||
			proc("GetFocus") != w.control(idAccounts) || len(w.controls) != count {
			t.Fatal("Escape must remove popup controls and restore trigger focus")
		}
		w.command(idDisconnect)
		if w.accountPopup == 0 || w.busy {
			t.Fatal("Connected must open the account menu without disconnecting")
		}
		proc("SetFocus", w.control(idAddAccount))
		if !w.accountKey(message{Message: 0x100, WParam: 9}) ||
			proc("GetFocus") != w.control(idAccountDisconnect) {
			t.Fatal("Disconnect is not keyboard-accessible in the account menu")
		}
		w.closeAccounts()
		w.command(idAddAccount)
		w.accept(event{path: "refresh", value: w.status})
		if !w.addingAccount || w.view != "idle" || w.status["connected"] != true {
			t.Fatal("polling interrupted Add account or disconnected the current account")
		}
		buffer := make([]uint16, 32)
		proc("GetWindowTextW", w.control(idCancelAccount), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if syscall.UTF16ToString(buffer) != "Back" {
			t.Fatal("the account connection page needs an accessible Back control")
		}
		back := captureClient(t, w.control(idCancelAccount), 32, 32)
		reference := captureClient(t, w.control(idSettingsBack), 32, 32)
		if !bytes.Equal(back.Pix, reference.Pix) {
			t.Fatal("the account connection page must use the same back arrow as Settings")
		}
		if dir := os.Getenv("KOBASHI_UI_CAPTURE_DIR"); dir != "" {
			f, err := os.Create(filepath.Join(dir, "light-add-account.png"))
			if err != nil {
				t.Fatal(err)
			}
			err = png.Encode(f, captureClient(t, w.Handle, windowWidth, w.contentHeight()))
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
		}
		w.setText(idToken, "private-test-input")
		w.command(idTokenView)
		proc("GetWindowTextW", w.control(idCancelAccount), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if syscall.UTF16ToString(buffer) != "Cancel" {
			t.Fatal("the account connection page changed another authentication Cancel control")
		}
		w.view = "idle"
		w.render()
		w.command(idCancelAccount)
		if w.addingAccount || w.view != "connected" || w.tokenValue() != "" ||
			proc("GetFocus") != w.control(idAccounts) {
			t.Fatal("Back must retain the connection, clear token input, and restore focus")
		}
		w.status["connected"] = false
		w.view = "token"
		w.render()
		if proc("GetWindowLongW", w.control(idAccounts), ^uintptr(15))&0x10000000 == 0 {
			t.Fatal("saved accounts are unreachable after token-based disconnect")
		}
	}()
	for id, want := range map[int]string{idCodex: "Codex", idClaude: "Claude Code", idCopilot: "GitHub Copilot"} {
		for _, c := range w.controls {
			if c.id == id && strings.ReplaceAll(c.title, "&", "") != want {
				t.Fatal("client label should omit Bridge", id, c.title)
			}
		}
	}
	for _, c := range w.controls {
		if c.view == "client" || c.view == "server" || c.view == "network" {
			t.Fatal("disabled network control was created", c.id, c.title)
		}
		if strings.ReplaceAll(c.title, "&", "") == "Copy Copilot setup" {
			t.Fatal("Copilot setup copy action is still present")
		}
		if strings.ReplaceAll(c.title, "&", "") == "Quit ModelBridge" {
			t.Fatal("Settings quit link is still present")
		}
	}
	w.fixedSize(0, 0)
	if w.control(2) != 0 {
		t.Fatal("the route description is still present")
	}
	dc := proc("GetDC", w.Handle)
	for font, want := range map[uintptr]string{w.font: "Segoe UI", w.bold: "Segoe UI", w.heading: "Segoe UI", w.symbolFont: "Segoe MDL2 Assets"} {
		old, _, _ := gdi.NewProc("SelectObject").Call(dc, font)
		face := make([]uint16, 64)
		gdi.NewProc("GetTextFaceW").Call(dc, uintptr(len(face)), uintptr(unsafe.Pointer(&face[0])))
		gdi.NewProc("SelectObject").Call(dc, old)
		if syscall.UTF16ToString(face) != want {
			t.Fatalf("incorrect native typography: %q", syscall.UTF16ToString(face))
		}
	}
	old, _, _ := gdi.NewProc("SelectObject").Call(dc, w.symbolFont)
	symbol, glyph := uint16(0xe713), uint16(0)
	if count, _, _ := gdi.NewProc("GetGlyphIndicesW").Call(dc, uintptr(unsafe.Pointer(&symbol)), 1,
		uintptr(unsafe.Pointer(&glyph)), 1); count != 1 || glyph == 0xffff {
		t.Fatal("the system font does not provide the standard Settings gear")
	}
	gdi.NewProc("SelectObject").Call(dc, old)
	proc("ReleaseDC", w.Handle, dc)
	proc("SendMessageW", w.Handle, 0x127, 1|2<<16, 0)
	w.code = "ABCD-1234"
	w.verification = "https://github.com/login/device"
	assertStatus := func(id int, want string, rgb uint32) {
		t.Helper()
		if id == idAccounts && !w.highContrast {
			rgb = themePalette(true, false).connected
		}
		h := w.control(id)
		if proc("GetWindowLongW", h, ^uintptr(15))&0x10000 == 0 {
			t.Fatal("connection status lost keyboard focusability", id)
		}
		buffer := make([]uint16, 64)
		proc("GetWindowTextW", h, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if strings.ReplaceAll(syscall.UTF16ToString(buffer), "&", "") != want {
			t.Fatal("incorrect connection status label", id, syscall.UTF16ToString(buffer))
		}
		var c control
		for _, candidate := range w.controls {
			if candidate.id == id {
				c = candidate
				break
			}
		}
		width, height := c.w*w.dpi/96, c.h*w.dpi/96
		screenshot := captureClient(t, h, width, height)
		pixel := func(rgb uint32) color.NRGBA {
			return color.NRGBA{R: byte(rgb >> 16), G: byte(rgb >> 8), B: byte(rgb), A: 255}
		}
		for i := range w.controls {
			if w.controls[i].id != id {
				continue
			}
			w.controls[i].hover = true
			hover := captureClient(t, h, width, height)
			w.controls[i].hover = false
			want := w.palette.button
			sample := 10
			if id == idAccounts {
				want = themePalette(true, false).hover
				if w.highContrast {
					want = w.palette.hover
				}
				sample = 5
			}
			if hover.NRGBAAt(sample*w.dpi/96, sample*w.dpi/96) != pixel(want) {
				t.Fatal("connection hover must preserve the contrast-safe background and semantic text color", id, w.dpi)
			}
		}
		colored := 0
		for y := 3; y < height-3; y++ {
			for x := 3; x < width-3; x++ {
				if screenshot.NRGBAAt(x, y) == pixel(rgb) {
					colored++
				}
			}
		}
		if colored < 4 {
			t.Fatal("connection status is not rendered in its state color", id, w.dpi, rgb)
		}
		if id == idAccounts {
			return
		}
		border := pixel(w.palette.border)
		for side, at := range []func(int) color.NRGBA{
			func(n int) color.NRGBA { return screenshot.NRGBAAt(width/2, n) },
			func(n int) color.NRGBA { return screenshot.NRGBAAt(width/2, height-1-n) },
			func(n int) color.NRGBA { return screenshot.NRGBAAt(n, height/2) },
			func(n int) color.NRGBA { return screenshot.NRGBAAt(width-1-n, height/2) },
		} {
			stroke := 0
			for stroke < height/3 && at(stroke) == border {
				stroke++
			}
			if stroke != max(1, (w.dpi+48)/96) {
				t.Errorf("status border is clipped or uneven: id=%d dpi=%d side=%d thickness=%d", id, w.dpi, side, stroke)
			}
		}
	}
	assertVisibleControlsFit := func() {
		t.Helper()
		var client rect
		proc("GetClientRect", w.Handle, uintptr(unsafe.Pointer(&client)))
		if proc("GetWindowLongW", w.Handle, ^uintptr(15))&(0x100000|0x200000) != 0 {
			t.Fatal("the compact window has scrollbars")
		}
		for _, c := range w.controls {
			if proc("GetWindowLongW", c.handle, ^uintptr(15))&0x10000000 == 0 {
				continue
			}
			var bounds rect
			proc("GetWindowRect", c.handle, uintptr(unsafe.Pointer(&bounds)))
			proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&bounds)), 2)
			if bounds.Left < 0 || bounds.Top < 0 || bounds.Right > client.Right || bounds.Bottom > client.Bottom {
				t.Fatalf("%s/%d DPI clips control %d: %v outside %v", w.view, w.dpi, c.id, bounds, client)
			}
		}
	}
	assertConnectedLayout := func() {
		t.Helper()
		var client rect
		proc("GetClientRect", w.Handle, uintptr(unsafe.Pointer(&client)))
		if client.Right != int32(windowWidth*w.dpi/96) || client.Bottom != int32(312*w.dpi/96) {
			t.Fatalf("connected/%d DPI does not preserve the padded content size: %v", w.dpi, client)
		}
		for id, y := range map[int]int{7: 24, 8: 58, idAccounts: 20, idCodex: 118, idClaude: 162,
			idCopilot: 206, idWorkBuddy: 250} {
			var bounds rect
			proc("GetWindowRect", w.control(id), uintptr(unsafe.Pointer(&bounds)))
			proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&bounds)), 2)
			if bounds.Top != int32(y*w.dpi/96) {
				t.Fatalf("connected/%d DPI misaligns control %d: %v", w.dpi, id, bounds)
			}
			if w.control(idDisconnect) != 0 {
				t.Fatal("Connected and its arrow must share one account-menu button")
			}
		}
		noticeVisible := proc("GetWindowLongW", w.control(12), ^uintptr(15))&0x10000000 != 0
		if noticeVisible != (w.notice != "" || text(w.status["error"]) != "") {
			t.Fatal("the connected homepage reserves space for an empty notice or hides an active notice")
		}
		if w.control(1) != 0 || w.control(idProject) != 0 {
			t.Fatal("the content must not include a product logo, title, or version")
		}
		if noticeVisible && proc("GetWindowLongW", w.control(8), ^uintptr(15))&0x10000000 != 0 {
			t.Fatal("connected errors must replace usage details")
		}
		var noticeBounds rect
		proc("GetWindowRect", w.control(12), uintptr(unsafe.Pointer(&noticeBounds)))
		proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&noticeBounds)), 2)
		if noticeBounds != (rect{int32(28 * w.dpi / 96), int32(58 * w.dpi / 96),
			int32(372 * w.dpi / 96), int32(90 * w.dpi / 96)}) {
			t.Fatal("connected error must fit in the usage slot", noticeBounds)
		}
		screenshot := captureClient(t, w.Handle, int(client.Right), int(client.Bottom))
		gearPixels := 0
		for y := 28 * w.dpi / 96; y < 44*w.dpi/96; y++ {
			for x := 352 * w.dpi / 96; x < 368*w.dpi/96; x++ {
				if got := screenshot.NRGBAAt(x, y); got.R != 0 || got.G != 0 || got.B != 0 {
					gearPixels++
				}
			}
		}
		if !w.highContrast && gearPixels == 0 {
			t.Fatal("the Settings glyph is missing from the connected header", w.dpi)
		}
		if !w.highContrast {
			headerBottom := 100 * w.dpi / 96
			ink := visibleInkBounds(screenshot, image.Rect(0, 0, screenshot.Bounds().Max.X, headerBottom), color.NRGBA{A: 255})
			if ink.Empty() || ink.Min.Y < 24*w.dpi/96 {
				t.Fatal("visible account content is too close to the native client-area top", w.dpi, ink)
			}
			_, _, usageVisible := w.usage()
			if usageVisible && !noticeVisible {
				if difference := ink.Min.Y - (headerBottom - ink.Max.Y); difference < -3*w.dpi/96 || difference > 3*w.dpi/96 {
					t.Fatal("the account card has inconsistent visible top and bottom padding", w.dpi, ink, headerBottom)
				}
			}
		}
		pixel := func(x, y int) color.NRGBA {
			return screenshot.NRGBAAt(x*w.dpi/96, y*w.dpi/96)
		}
		rgb := func(value uint32) color.NRGBA {
			return color.NRGBA{R: byte(value >> 16), G: byte(value >> 8), B: byte(value), A: 255}
		}
		if !w.highContrast && pixel(5, 5) != rgb(0) {
			t.Fatal("connected account region must use a solid black background")
		}
		for _, y := range []int{100, 140, 162, 184, 206, 228, 250, 272, 311} {
			if pixel(0, y) != rgb(w.palette.panel) || pixel(399, y) != rgb(w.palette.panel) ||
				pixel(200, y) != rgb(w.palette.panel) {
				t.Fatalf("connected/%d DPI must use one edge-to-edge client surface at row %d", w.dpi, y)
			}
		}
		assertVisibleControlsFit()
	}
	for _, theme := range []string{"dark", "light"} {
		w.status["appearance"] = theme
		for _, view := range []string{"idle", "token", "device", "connected", "settings"} {
			w.view = view
			w.notice = ""
			w.render()
			assertVisibleControlsFit()
			if view == "idle" || view == "token" {
				assertStatus(idConnect, "Not Connected", w.palette.disconnected)
			}
			if view == "token" {
				assertStatus(idLogin, "Not Connected", w.palette.disconnected)
			}
			if view == "connected" {
				assertStatus(idAccounts, "Accounts", w.palette.connected)
				assertConnectedLayout()
				for _, enabled := range []bool{true, false} {
					for _, key := range []string{"claudeEnabled", "codexEnabled", "copilotEnabled"} {
						w.status[key] = enabled
					}
					w.render()
					dot := w.palette.disconnected
					if enabled {
						dot = w.palette.connected
					}
					want := color.NRGBA{R: byte(dot >> 16), G: byte(dot >> 8), B: byte(dot), A: 255}
					for _, id := range []int{idClaude, idCodex, idCopilot} {
						image := captureClient(t, w.control(id), 364, 44)
						if got := image.NRGBAAt(62, 27); got != want {
							t.Fatal("status dot does not use its semantic color", theme, id, enabled, got, want)
						}
					}
					reference := captureClient(t, w.control(idClaude), 364, 44)
					for _, id := range []int{idCodex, idCopilot} {
						if id == idCopilot && !enabled {
							continue
						}
						status := captureClient(t, w.control(id), 364, 44)
						for y := 24; y < 44; y++ {
							for x := 58; x < 254; x++ {
								if status.NRGBAAt(x, y) != reference.NRGBAAt(x, y) {
									t.Fatal("Bridge status text or typography differs", id, enabled, x, y)
								}
							}
						}
					}
				}
				w.status["claudeEnabled"], w.status["codexEnabled"], w.status["copilotEnabled"] = true, false, true
				w.render()
			}
			if view == "idle" || view == "connected" {
				w.busy = true
				w.render()
				if view == "idle" {
					assertStatus(idConnect, "Not Connected", w.palette.disconnected)
				} else {
					assertStatus(idAccounts, "Accounts", w.palette.connected)
				}
				w.busy = false
				w.render()
			}
			screenshot := captureClient(t, w.Handle, windowWidth, w.contentHeight())
			want := themePalette(theme == "dark", w.highContrast)
			if view == "connected" && !w.highContrast {
				want.background = 0
			}
			background := screenshot.NRGBAAt(5, 5)
			if background != (color.NRGBA{R: byte(want.background >> 16), G: byte(want.background >> 8), B: byte(want.background), A: 255}) {
				t.Fatalf("%s/%s has an incorrect plain background: %v", theme, view, background)
			}
			if w.control(1) != 0 || w.control(idProject) != 0 || w.control(idDone) != 0 {
				t.Fatal("product header, version, or Settings Done control is still present")
			}
			if dir := os.Getenv("KOBASHI_UI_CAPTURE_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, theme+"-"+view+".png")
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(f, screenshot)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
		}
	}
	for id, want := range map[int]rect{
		idSettings: {344, 8, 376, 40},
		idCodex:    {18, 118, 382, 162}, idClaude: {18, 162, 382, 206},
		idCopilot:   {18, 206, 382, 250},
		idWorkBuddy: {18, 250, 382, 294},
		idStartup:   {16, 64, 384, 96}, idTray: {16, 100, 384, 132},
		idAppearanceLabel: {24, 147, 176, 165},
		idSettingsBack:    {16, 16, 48, 48},
	} {
		var got rect
		proc("GetWindowRect", w.control(id), uintptr(unsafe.Pointer(&got)))
		proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&got)), 2)
		if got != want {
			t.Fatal("control does not match expected UI geometry", id, got, want)
		}
	}
	for _, id := range []int{idClaude, idCodex, idCopilot, idStartup, idTray} {
		if proc("GetWindowLongW", w.control(id), ^uintptr(15))&0xf != 3 {
			t.Fatal("custom appearance discarded native checkbox semantics", id)
		}
	}
	w.view = "idle"
	for _, theme := range []string{"dark", "light"} {
		w.status["appearance"] = theme
		for _, dpi := range []int{96, 120, 144, 168, 192, 216, 240, 288, 384} {
			w.view = "idle"
			w.dpi = dpi
			w.fonts()
			w.fixedSize(0, 0)
			w.render()
			assertStatus(idConnect, "Not Connected", w.palette.disconnected)
			var client rect
			proc("GetClientRect", w.Handle, uintptr(unsafe.Pointer(&client)))
			if int(client.Right) != windowWidth*dpi/96 || int(client.Bottom) != w.contentHeight()*dpi/96 {
				t.Fatalf("%s/%d DPI changed the fixed logical content size: %v", theme, dpi, client)
			}
			var frame rect
			proc("GetWindowRect", w.Handle, uintptr(unsafe.Pointer(&frame)))
			var limits minMaxInfo
			proc("SendMessageW", w.Handle, 0x24, 0, uintptr(unsafe.Pointer(&limits)))
			wantSize := point{frame.Right - frame.Left, frame.Bottom - frame.Top}
			if limits.MinTrackSize != wantSize || limits.MaxTrackSize != wantSize {
				t.Fatalf("%s/%d DPI allows Windows to clip the fixed content: %+v, frame %v", theme, dpi, limits, frame)
			}
			assertVisibleControlsFit()
			var hint, link control
			dc := proc("GetDC", w.Handle)
			if dc == 0 {
				t.Fatal("could not measure native text")
			}
			old, _, _ := gdi.NewProc("SelectObject").Call(dc, w.small)
			for _, c := range w.controls {
				if c.id != 3 && c.id != idTokenView {
					continue
				}
				if c.id == 3 {
					hint = c
				} else {
					link = c
				}
				var extent point
				if result, _, _ := gdi.NewProc("GetTextExtentPoint32W").Call(dc, uintptr(unsafe.Pointer(wide(c.title))),
					uintptr(len(syscall.StringToUTF16(c.title))-1), uintptr(unsafe.Pointer(&extent))); result == 0 {
					t.Fatal("could not measure native text", c.title)
				}
				if int(extent.X) > c.w*dpi/96 || int(extent.Y) > c.h*dpi/96 {
					t.Errorf("%s/%d DPI clips %q: text %dx%d, control %dx%d", theme, dpi, c.title,
						extent.X, extent.Y, c.w*dpi/96, c.h*dpi/96)
				}
				if c.x < 32 || c.x+c.w > 368 {
					t.Errorf("%s/%d DPI places sign-in text outside the content padding", theme, dpi)
				}
			}
			gdi.NewProc("SelectObject").Call(dc, old)
			old, _, _ = gdi.NewProc("SelectObject").Call(dc, w.font)
			for _, on := range []bool{false, true} {
				text := bridgeStatusText(idCopilot, on, false)
				var extent point
				if result, _, _ := gdi.NewProc("GetTextExtentPoint32W").Call(dc, uintptr(unsafe.Pointer(wide(text))),
					uintptr(len(syscall.StringToUTF16(text))-1), uintptr(unsafe.Pointer(&extent))); result == 0 {
					t.Fatal("could not measure Copilot status text", text)
				}
				if int(extent.X) > 180*dpi/96 || int(extent.Y) > 24*dpi/96 {
					t.Errorf("%s/%d DPI clips Copilot status %q: text %dx%d", theme, dpi, text, extent.X, extent.Y)
				}
			}
			gdi.NewProc("SelectObject").Call(dc, old)
			proc("ReleaseDC", w.Handle, dc)
			if hint.x+hint.w+6 > link.x || hint.y != link.y || hint.h != link.h {
				t.Errorf("%s/%d DPI overlaps or misaligns the token hint and link", theme, dpi)
			}
			if proc("GetWindowLongW", link.handle, ^uintptr(15))&0x10000 == 0 {
				t.Fatal("token link lost keyboard focusability")
			}
			if dir := os.Getenv("KOBASHI_UI_CAPTURE_DIR"); dir != "" {
				path := filepath.Join(dir, fmt.Sprintf("%s-idle-%d-dpi.png", theme, dpi))
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(f, captureClient(t, w.Handle, windowWidth*dpi/96, w.contentHeight()*dpi/96))
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			w.view = "connected"
			w.render()
			assertStatus(idAccounts, "Accounts", w.palette.connected)
			assertConnectedLayout()
			for _, id := range []int{idCodex, idClaude, idCopilot} {
				w.busy, w.pendingBridge = true, id
				w.render()
				assertConnectedLayout()
				if got := bridgeStatusText(id, false, w.pendingBridge == id); got != "Working..." {
					t.Fatal("pending Bridge status is missing", id, got)
				}
				w.accept(event{path: "/api/toggle-copilot", value: map[string]any{
					"codexEnabled": false, "claudeEnabled": true, "copilotEnabled": true,
					"notice": "Kobashi provider registered and selected for the current local session.",
				}})
				assertConnectedLayout()
				if w.notice != "" || w.busy || w.pendingBridge != 0 {
					t.Fatal("successful toggle leaves progress or provider explanation behind")
				}
			}
			for _, source := range []string{"notice", "copilot-ready", "copilot-pending", "error"} {
				if source == "notice" {
					w.notice = "Could not save settings."
				} else if source == "copilot-ready" {
					w.accept(event{path: "/api/toggle-copilot", value: map[string]any{
						"codexEnabled": false, "claudeEnabled": true, "copilotEnabled": true,
						"notice": "Kobashi provider registered and selected for the current local session.",
					}})
				} else if source == "copilot-pending" {
					w.accept(event{path: "/api/toggle-copilot", value: map[string]any{
						"codexEnabled": false, "claudeEnabled": true, "copilotEnabled": true,
						"notice": "Kobashi registered; no session was switched. Select its model when idle.",
					}})
				} else {
					w.status["error"] = "GitHub needs authorization."
				}
				w.render()
				if strings.HasPrefix(source, "copilot-") {
					assertConnectedLayout()
					if w.notice != "" {
						t.Fatal("successful provider registration must be silent")
					}
				} else {
					assertConnectedLayout()
				}
				w.notice = ""
				delete(w.status, "error")
				w.render()
				assertConnectedLayout()
			}
			w.status["usage"] = nil
			w.render()
			assertConnectedLayout()
			w.status["usage"] = map[string]any{"creditsUsed": float64(12), "entitlement": float64(100)}
			var mainFrame rect
			proc("GetWindowRect", w.Handle, uintptr(unsafe.Pointer(&mainFrame)))
			w.view = "settings"
			w.render()
			var settingsFrame rect
			proc("GetWindowRect", w.Handle, uintptr(unsafe.Pointer(&settingsFrame)))
			if settingsFrame.Top != mainFrame.Top || settingsFrame.Left != mainFrame.Left {
				t.Fatal("switching to Settings moved the window's top edge", theme, dpi)
			}
			proc("GetClientRect", w.Handle, uintptr(unsafe.Pointer(&client)))
			if int(client.Right) != windowWidth*dpi/96 || int(client.Bottom) != 236*dpi/96 {
				t.Fatalf("%s/%d DPI changed the Settings window size: %v", theme, dpi, client)
			}
			settingsImage := captureClient(t, w.Handle, int(client.Right), int(client.Bottom))
			if w.control(10) != 0 || w.control(11) != 0 || w.control(idTheme) != 0 {
				t.Fatal("Settings descriptions or header theme toggle are still present")
			}
			if proc("SendMessageW", w.control(idAppearance), 0x147, 0, 0) != map[string]uintptr{"light": 1, "dark": 2}[theme] {
				t.Fatal("Appearance selector does not reflect the saved mode", theme, dpi)
			}
			var appearanceBounds rect
			proc("GetWindowRect", w.control(idAppearance), uintptr(unsafe.Pointer(&appearanceBounds)))
			proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&appearanceBounds)), 2)
			if appearanceBounds.Left < int32(200*dpi/96) || appearanceBounds.Right > int32(376*dpi/96) ||
				appearanceBounds.Top < int32(140*dpi/96) || appearanceBounds.Bottom > int32(180*dpi/96) {
				t.Fatal("native Appearance selector overlaps other Settings controls", theme, dpi, appearanceBounds)
			}
			for _, row := range []struct{ label, selector int }{{idAppearanceLabel, idAppearance}, {idLanguageLabel, idLanguage}} {
				var label, selector rect
				proc("GetWindowRect", w.control(row.label), uintptr(unsafe.Pointer(&label)))
				proc("GetWindowRect", w.control(row.selector), uintptr(unsafe.Pointer(&selector)))
				if delta := int(label.Top + label.Bottom - selector.Top - selector.Bottom); delta < -2 || delta > 2 {
					t.Fatal("the native selector is not centered on its label", theme, dpi, row.selector, label, selector)
				}
			}
			w.notice = "Could not save settings."
			w.render()
			if proc("GetWindowLongW", w.control(9), ^uintptr(15))&0x10000000 != 0 ||
				proc("GetWindowLongW", w.control(12), ^uintptr(15))&0x10000000 == 0 {
				t.Fatal("a settings error must replace its heading without overlapping it")
			}
			w.notice = ""
			w.render()
			if proc("GetWindowLongW", w.control(9), ^uintptr(15))&0x10000000 == 0 ||
				proc("GetWindowLongW", w.control(12), ^uintptr(15))&0x10000000 != 0 {
				t.Fatal("an empty settings notice must not cover the heading")
			}
			background := color.NRGBA{R: byte(w.palette.background >> 16), G: byte(w.palette.background >> 8),
				B: byte(w.palette.background), A: 255}
			titleInk := visibleInkBounds(settingsImage, image.Rect(48*dpi/96, 0, 180*dpi/96, 56*dpi/96), background)
			startupInk := visibleInkBounds(settingsImage, image.Rect(24*dpi/96, 64*dpi/96, 376*dpi/96, 96*dpi/96), background)
			if titleInk.Empty() || startupInk.Empty() || titleInk.Min.Y < 24*dpi/96 ||
				startupInk.Min.Y-titleInk.Max.Y < 24*dpi/96 {
				t.Fatal("Settings needs visible top padding and separation before its first row", dpi, titleInk, startupInk)
			}
			var lastRow rect
			proc("GetWindowRect", w.control(idLanguage), uintptr(unsafe.Pointer(&lastRow)))
			proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&lastRow)), 2)
			if client.Bottom-lastRow.Bottom < int32(24*dpi/96) {
				t.Fatal("adding top padding crowded the last Settings row", dpi, lastRow, client)
			}
			for y := 56; y < 236; y++ {
				if settingsImage.NRGBAAt(0, y*dpi/96) != background {
					t.Fatalf("%s/%d DPI still draws a Settings panel border", theme, dpi)
				}
			}
			dc = proc("GetDC", w.Handle)
			old, _, _ = gdi.NewProc("SelectObject").Call(dc, w.font)
			for _, c := range w.controls {
				if c.id == idStartup || c.id == idTray || c.id == idAppearanceLabel {
					image := captureClient(t, c.handle, c.w*dpi/96, c.h*dpi/96)
					if image.NRGBAAt(image.Bounds().Max.X-1, image.Bounds().Max.Y-1) != background {
						t.Fatal("Settings controls do not share the window background", c.id, theme, dpi)
					}
					if (c.id == idStartup || c.id == idTray) && proc("SendMessageW", c.handle, 0xf0, 0, 0) == 1 {
						on := color.NRGBA{R: byte(w.palette.on >> 16), G: byte(w.palette.on >> 8), B: byte(w.palette.on), A: 255}
						if image.NRGBAAt((c.w-24)*dpi/96, 10*dpi/96) != on {
							t.Fatal("checked box does not use the contrast-safe berry fill", c.id, theme, dpi)
						}
					}
				}
				if c.id != idAppearanceLabel {
					continue
				}
				bounds := rect{Right: int32(c.w * dpi / 96), Bottom: int32(c.h * dpi / 96)}
				if proc("DrawTextW", dc, uintptr(unsafe.Pointer(wide(c.title))), ^uintptr(0),
					uintptr(unsafe.Pointer(&bounds)), 0xc10) == 0 {
					t.Fatal("could not measure settings description", c.title)
				}
				if int(bounds.Right) > c.w*dpi/96 || int(bounds.Bottom) > c.h*dpi/96 {
					t.Errorf("%s/%d DPI clips the settings description %q", theme, dpi, c.title)
				}
			}
			gdi.NewProc("SelectObject").Call(dc, old)
			proc("ReleaseDC", w.Handle, dc)
			if proc("GetNextDlgTabItem", w.Handle, w.control(idStartup), 0) != w.control(idTray) ||
				proc("GetNextDlgTabItem", w.Handle, w.control(idTray), 1) != w.control(idStartup) ||
				proc("GetNextDlgTabItem", w.Handle, w.control(idTray), 0) != w.control(idAppearance) ||
				proc("GetNextDlgTabItem", w.Handle, w.control(idAppearance), 0) != w.control(idLanguage) ||
				proc("GetNextDlgTabItem", w.Handle, w.control(idLanguage), 0) != w.control(idSettingsBack) {
				t.Fatal("Settings focus order must be Start at login, Close to tray, Appearance, Language, Back")
			}
			if proc("GetWindowLongW", w.control(idSettingsBack), ^uintptr(15))&0x10000 == 0 {
				t.Fatal("Back lost keyboard focusability")
			}
			if w.control(idQuit) != 0 || w.control(idProject) != 0 || w.control(idDone) != 0 || w.control(idPrivacy) != 0 {
				t.Fatal("Settings must not contain Done, Version, Quit, or Privacy links")
			}
			if dir := os.Getenv("KOBASHI_UI_CAPTURE_DIR"); dir != "" {
				path := filepath.Join(dir, fmt.Sprintf("%s-settings-%d-dpi.png", theme, dpi))
				f, err := os.Create(path)
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(f, captureClient(t, w.Handle, windowWidth*dpi/96, w.contentHeight()*dpi/96))
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			for _, view := range []string{"idle", "token", "device", "connected", "settings", "quitting"} {
				w.view = view
				w.render()
				assertVisibleControlsFit()
				if view == "device" {
					proc("GetClientRect", w.Handle, uintptr(unsafe.Pointer(&client)))
					if client.Bottom != int32(232*dpi/96) ||
						proc("GetWindowLongW", w.control(idSettings), ^uintptr(15))&0x10000000 != 0 ||
						proc("GetWindowLongW", w.control(idCancelAccount), ^uintptr(15))&0x10000000 == 0 {
						t.Fatal("the device dialog must use compact content and a navigation control", dpi)
					}
					for _, id := range []int{5, idBrowser, 6, idCopy, 12} {
						var bounds rect
						proc("GetWindowRect", w.control(id), uintptr(unsafe.Pointer(&bounds)))
						proc("MapWindowPoints", 0, w.Handle, uintptr(unsafe.Pointer(&bounds)), 2)
						if bounds.Left < int32(40*dpi/96) || bounds.Right > int32(360*dpi/96) ||
							bounds.Top < int32(64*dpi/96) || bounds.Bottom > int32(192*dpi/96) {
							t.Fatal("a device-card control violates its 16-point inner padding", dpi, id, bounds)
						}
					}
				}
				if dir := os.Getenv("KOBASHI_UI_CAPTURE_DIR"); dir != "" && dpi == 144 && (view == "connected" || view == "device") {
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("%s-%s-%d-dpi.png", theme, view, dpi)))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, captureClient(t, w.Handle, windowWidth*dpi/96, w.contentHeight()*dpi/96))
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				}
			}
		}
	}
	for _, connected := range []bool{false, true} {
		w.status["connected"] = connected
		w.view = "settings"
		w.notice = "Changes are saved automatically."
		w.command(idDone)
		want := "idle"
		if connected {
			want = "connected"
		}
		if w.view != want || w.notice != "" {
			t.Fatal("Done did not return to the main view", w.view, w.notice)
		}
		w.command(idSettings)
		if w.view != "settings" || proc("GetWindowLongW", w.control(idSettings), ^uintptr(15))&0x10000000 != 0 {
			t.Fatal("Settings icon did not open Settings or remains visible beside Back")
		}
		w.command(idSettingsBack)
		if w.view != want || proc("GetWindowLongW", w.control(idSettings), ^uintptr(15))&0x10000000 == 0 {
			t.Fatal("Back did not return to the main screen")
		}
	}
	w.status["ready"] = false
	w.view = "settings"
	w.command(idSettings)
	if w.view != "starting" {
		t.Fatal("closing Settings through its icon bypassed startup readiness")
	}
	delete(w.status, "ready")
	w.view = "connected"
	w.status["username"] = "xjin6"
	w.render()
	for id, want := range map[int]string{7: "@xjin6"} {
		buffer := make([]uint16, 128)
		proc("GetWindowTextW", w.control(id), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
		if syscall.UTF16ToString(buffer) != want {
			t.Fatal("author removal changed the native accessible label or signed-in identity", id)
		}
	}
	w.view = "settings"
	w.render()
	if proc("GetNextDlgTabItem", w.Handle, w.control(idLanguage), 0) != w.control(idSettingsBack) ||
		proc("GetNextDlgTabItem", w.Handle, w.control(idSettingsBack), 1) != w.control(idLanguage) {
		t.Fatal("Back lost forward or reverse keyboard navigation")
	}
}

func BenchmarkNativeArtwork(b *testing.B) {
	for _, name := range []string{"github", "codex"} {
		b.Run(name, func(b *testing.B) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			w := &Window{dpi: 192, palette: themePalette(true, false)}
			if err := w.loadArtwork(); err != nil {
				b.Fatal(err)
			}
			dc := proc("GetDC", 0)
			defer proc("ReleaseDC", 0, dc)
			mem, _, _ := gdi.NewProc("CreateCompatibleDC").Call(dc)
			defer gdi.NewProc("DeleteDC").Call(mem)
			bitmap, _, _ := gdi.NewProc("CreateCompatibleBitmap").Call(dc, 48, 48)
			if dc == 0 || mem == 0 || bitmap == 0 {
				b.Fatal("could not create the isolated artwork buffer")
			}
			old, _, _ := gdi.NewProc("SelectObject").Call(mem, bitmap)
			defer func() {
				gdi.NewProc("SelectObject").Call(mem, old)
				gdi.NewProc("DeleteObject").Call(bitmap)
			}()
			p := painter{w, mem}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.artwork(name, 0, 0, 24, w.palette.tile, w.palette.accent, name == "github")
			}
		})
	}
}

func TestNativeUsageHidesUnavailableValuesAndFormatsCredits(t *testing.T) {
	for _, usage := range []map[string]any{nil, {}, {"creditsUsed": float64(0), "entitlement": float64(0)},
		{"creditsUsed": float64(1), "entitlement": float64(100), "unlimited": true},
		{"creditsUsed": math.NaN(), "entitlement": float64(100)}} {
		w := Window{status: map[string]any{"usage": usage}}
		if _, _, ok := w.usage(); ok {
			t.Fatal("unavailable usage was shown", usage)
		}
	}

	if credits(1234567.5) != "1,234,567.5" {
		t.Fatal("credits formatting does not match the existing UI")
	}
}

func TestNativeThemeContrast(t *testing.T) {
	luminance := func(rgb uint32) float64 {
		channels := [3]float64{}
		for i, shift := range []uint{16, 8, 0} {
			value := float64((rgb>>shift)&255) / 255
			if value <= 0.04045 {
				channels[i] = value / 12.92
			} else {
				channels[i] = math.Pow((value+0.055)/1.055, 2.4)
			}
		}
		return channels[0]*0.2126 + channels[1]*0.7152 + channels[2]*0.0722
	}
	for _, dark := range []bool{false, true} {
		colors := themePalette(dark, false)
		for _, rgb := range []uint32{colors.accent, colors.secondary, colors.on} {
			r, g, b := rgb>>16, rgb>>8&255, rgb&255
			if r <= b || r <= g {
				t.Errorf("theme is not Pomegram: dark=%v color=%06x", dark, rgb)
			}
		}
		green, gray, red := uint32(0x15803d), uint32(0x6b7280), uint32(0xdc2626)
		if dark {
			green, gray, red = 0x3fb950, 0x9ca3af, 0xf85149
		}
		if colors.connected != green || colors.disconnected != gray || colors.danger != red {
			t.Errorf("theme overrides semantic status colors: dark=%v", dark)
		}
		for _, pair := range [][2]uint32{
			{colors.text, colors.background}, {colors.muted, colors.panel},
			{colors.accent, colors.badge}, {colors.secondary, colors.background},
			{colors.disconnected, colors.button}, {colors.connected, colors.panel},
			{colors.knob, colors.on},
		} {
			a, b := luminance(pair[0]), luminance(pair[1])
			if ratio := (max(a, b) + 0.05) / (min(a, b) + 0.05); ratio < 4.5 {
				t.Errorf("theme text contrast is %.2f: dark=%v colors=%x", ratio, dark, pair)
			}
		}
		for _, pair := range [][2]uint32{
			{colors.on, colors.panel}, {colors.toggle, colors.panel},
			{colors.knob, colors.on}, {colors.knob, colors.toggle},
		} {
			a, b := luminance(pair[0]), luminance(pair[1])
			if ratio := (max(a, b) + 0.05) / (min(a, b) + 0.05); ratio < 3 {
				t.Errorf("theme control contrast is %.2f: dark=%v colors=%x", ratio, dark, pair)
			}
		}
	}
}
