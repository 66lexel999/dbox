//go:build windows

// Package walkui is MyIDM's native desktop UI, built on lxn/walk — real Win32
// ComCtl32 widgets (ListView, TreeView, ToolBar) rendered in a forced dark
// theme. It replaces the previous Gio (GPU immediate-mode) front end with the
// most authentic, flash-free "looks like a real Windows app / IDM" presentation.
package walkui

import (
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"

	"myidm/internal/engine"
	"myidm/internal/procutil"
)

// Engine is the subset of *engine.Engine the main window uses.
type Engine interface {
	List() []engine.TaskView
	Categories() map[string]string
	CategoryDir(name string) string
	Add(rawURL, fileName string, segments int, dir string, scheduledAt time.Time, later bool) (engine.TaskView, error)
	Pause(id string) error
	Resume(id string) error
	Delete(id string, removeFile bool) error
	RenameFile(id, newName string) (engine.TaskView, error)
	FilePath(id string) (string, error)
}

var (
	activeStatuses = map[engine.Status]bool{
		engine.StatusDownloading: true, engine.StatusQueued: true,
		engine.StatusPaused: true, engine.StatusFailed: true,
	}
	catOrder   = []string{"General", "Compressed", "Documents", "Music", "Video", "Programs", "Images"}
	segChoices = []int{0, 2, 4, 8, 16}
	connLabels = []string{"auto", "2", "4", "8", "16"}
	// column index -> sort key
	colKeys = []string{"name", "size", "status", "eta", "speed", "date"}
)

// mainWin holds the live main window so engine/server callbacks (on other
// goroutines) can marshal popup creation onto the GUI thread via Synchronize.
var (
	mainMu  sync.Mutex
	mainWin *walk.MainWindow
)

func setMainWindow(w *walk.MainWindow) { mainMu.Lock(); mainWin = w; mainMu.Unlock() }
func getMainWindow() *walk.MainWindow  { mainMu.Lock(); defer mainMu.Unlock(); return mainWin }

// onGUI runs f on the GUI thread (no-op if the main window isn't up yet).
func onGUI(f func()) {
	if w := getMainWindow(); w != nil {
		w.Synchronize(f)
	}
}

// App is the walk main window.
type App struct {
	eng Engine
	log *slog.Logger

	onPrompt func(url string)
	onDetail func(id string)

	mw       *walk.MainWindow
	tv       *walk.TableView // downloads
	side     *walk.TableView // categories sidebar
	urlEdit  *walk.LineEdit
	catCombo *walk.ComboBox
	connSel  *walk.ComboBox
	sbItem   *walk.Label

	model  *tableModel
	smodel *sideModel

	cats     map[string]string
	filter   string
	selID    string
	resizing bool   // true during a modal resize drag — skip the costly column re-fit
	lastSig  string // fingerprint of last-rendered data; skip repaint when unchanged
}

// New builds the app (window is created later, on the GUI thread, by Run).
func New(eng Engine, log *slog.Logger, onPrompt func(string), onDetail func(string)) *App {
	a := &App{eng: eng, log: log, onPrompt: onPrompt, onDetail: onDetail, filter: "all"}
	a.model = &tableModel{app: a, sortCol: 5, order: walk.SortDescending}
	a.smodel = &sideModel{app: a}
	return a
}

// RunBlocking creates the window on the (locked) calling thread, runs the walk
// message loop, and exits the process when the window closes (after onClose).
func RunBlocking(a *App, onClose func()) {
	runtime.LockOSThread()
	enableDarkModeApp()
	if err := a.create(); err != nil {
		a.log.Error("create main window", "err", err)
		os.Exit(1)
	}
	a.refresh()
	a.startPolling()
	setMainWindow(a.mw)

	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		a.log.Info("main window closing", "reason", reason)
		if onClose != nil {
			onClose()
		}
	})
	a.mw.Run()
	a.log.Info("walk message loop returned — exiting")
	os.Exit(0)
}

func (a *App) create() error {
	err := dcl.MainWindow{
		AssignTo:   &a.mw,
		Title:      "MyIDM — Download Manager",
		Name:       "myidm",
		Size:       dcl.Size{Width: 1240, Height: 820},
		MinSize:    dcl.Size{Width: 820, Height: 500},
		Font:       dcl.Font{Family: "Segoe UI", PointSize: 10},
		Visible:    false, // shown after dark theming, to avoid a white flash
		Background: dcl.SolidColorBrush{Color: cBg},
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []dcl.Widget{
			a.toolbar(),
			a.addBar(),
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, SpacingZero: true},
				Children: []dcl.Widget{
					a.sidebar(),
					a.table(),
				},
			},
			a.statusBar(),
		},
		OnSizeChanged: a.resizeColumns,
	}.Create()
	if err != nil {
		return err
	}

	a.applyDark()
	a.initURLPlaceholder()
	// Suppress the expensive column re-fit during a live resize drag; fit once
	// when the drag ends (a column-width change repaints every row).
	installResizeGuard(a.mw.Handle(),
		func() { a.resizing = true },
		func() { a.resizing = false; a.resizeColumns() },
	)
	a.mw.SetVisible(true)
	return nil
}

// urlPlaceholder is the muted hint shown in the empty URL field. walk's native
// CueBanner can't be colored (invisible on a dark field), so we manage it
// manually: show it muted when the field is empty + unfocused, clear it on focus.
const urlPlaceholder = "Paste a download URL (http/https)…"

func (a *App) initURLPlaceholder() {
	a.showURLPlaceholder()
	a.urlEdit.FocusedChanged().Attach(func() {
		if a.urlEdit.Focused() {
			if a.urlEdit.Text() == urlPlaceholder {
				a.urlEdit.SetText("")
				a.urlEdit.SetTextColor(cText)
			}
		} else if strings.TrimSpace(a.urlEdit.Text()) == "" {
			a.showURLPlaceholder()
		}
	})
}

func (a *App) showURLPlaceholder() {
	a.urlEdit.SetText(urlPlaceholder)
	a.urlEdit.SetTextColor(cMuted)
}

// urlText returns the real URL in the field, treating the placeholder as empty.
func (a *App) urlText() string {
	t := strings.TrimSpace(a.urlEdit.Text())
	if t == urlPlaceholder {
		return ""
	}
	return t
}

// resetURLField clears the field after a successful add, restoring the
// placeholder if focus has already moved away (e.g. the Add button was clicked).
func (a *App) resetURLField() {
	if a.urlEdit.Focused() {
		a.urlEdit.SetText("")
		a.urlEdit.SetTextColor(cText)
	} else {
		a.showURLPlaceholder()
	}
}

// toolbar is a custom dark strip of icon+label buttons (the native ComCtl32
// ToolBar can't be reliably darkened).
func (a *App) toolbar() dcl.Widget {
	sep := func() dcl.Widget {
		return dcl.Composite{MaxSize: dcl.Size{Width: 1}, MinSize: dcl.Size{Width: 1}, Background: dcl.SolidColorBrush{Color: cBorder}}
	}
	return dcl.Composite{
		Background: dcl.SolidColorBrush{Color: cBg2},
		Layout:     dcl.HBox{Margins: dcl.Margins{Left: 4, Top: 3, Right: 4, Bottom: 3}, Spacing: 2},
		Children: []dcl.Widget{
			a.toolBtn("add", "Add URL"),
			a.toolBtn("resume", "Resume"),
			a.toolBtn("stop", "Stop"),
			a.toolBtn("stopall", "Stop All"),
			sep(),
			a.toolBtn("delete", "Delete"),
			a.toolBtn("delcompleted", "Delete Completed"),
			dcl.HSpacer{},
		},
	}
}

func (a *App) toolBtn(key, label string) dcl.Widget {
	click := func(x, y int, b walk.MouseButton) { a.toolActionRun(key) }
	return dcl.Composite{
		Background:  dcl.SolidColorBrush{Color: cBg2},
		Layout:      dcl.HBox{Margins: dcl.Margins{Left: 8, Top: 4, Right: 8, Bottom: 4}, Spacing: 6},
		OnMouseDown: click,
		Children: []dcl.Widget{
			dcl.ImageView{Image: toolBitmap(key), OnMouseDown: click},
			dcl.Label{Text: label, TextColor: cText, OnMouseDown: click},
		},
	}
}

// statusBar is a custom dark bottom strip (the native ComCtl32 status bar can't
// be darkened, like the toolbar). A thin top border separates it from the table.
func (a *App) statusBar() dcl.Widget {
	return dcl.Composite{
		Background: dcl.SolidColorBrush{Color: cBg2},
		MaxSize:    dcl.Size{Height: 26},
		MinSize:    dcl.Size{Height: 26},
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []dcl.Widget{
			dcl.Composite{MaxSize: dcl.Size{Height: 1}, MinSize: dcl.Size{Height: 1}, Background: dcl.SolidColorBrush{Color: cBorder}},
			dcl.Composite{
				Background: dcl.SolidColorBrush{Color: cBg2},
				Layout:     dcl.HBox{Margins: dcl.Margins{Left: 10, Top: 2, Right: 10, Bottom: 2}, Spacing: 6},
				Children: []dcl.Widget{
					dcl.Label{AssignTo: &a.sbItem, Text: "Ready", TextColor: cMuted},
					dcl.HSpacer{},
				},
			},
		},
	}
}

// addBar is the URL entry strip: [ url ............ ] [cat] [conn] [Save as…] [Add]
func (a *App) addBar() dcl.Widget {
	return dcl.Composite{
		Background: dcl.SolidColorBrush{Color: cBg},
		Layout:     dcl.HBox{MarginsZero: false, Margins: dcl.Margins{Left: 8, Top: 6, Right: 8, Bottom: 6}, Spacing: 6},
		Children: []dcl.Widget{
			// No native CueBanner: its placeholder text color isn't settable and
			// renders invisibly dark-on-dark. We draw our own placeholder instead
			// (initURLPlaceholder), toggled on focus.
			dcl.LineEdit{AssignTo: &a.urlEdit,
				Background: dcl.SolidColorBrush{Color: cPanel2}, TextColor: cMuted, OnKeyDown: func(key walk.Key) {
					if key == walk.KeyReturn {
						a.doAdd()
					}
				}},
			dcl.ComboBox{AssignTo: &a.catCombo, Model: catOrder, CurrentIndex: 0, MaxSize: dcl.Size{Width: 130}, MinSize: dcl.Size{Width: 110}},
			dcl.ComboBox{AssignTo: &a.connSel, Model: connLabels, CurrentIndex: 3, MaxSize: dcl.Size{Width: 80}, MinSize: dcl.Size{Width: 64}},
			dcl.PushButton{Text: "Save as…", MaxSize: dcl.Size{Width: 90}, OnClicked: func() {
				if u := a.urlText(); u != "" && a.onPrompt != nil {
					a.onPrompt(u)
				}
			}},
			dcl.PushButton{Text: "Add", MaxSize: dcl.Size{Width: 70}, OnClicked: a.doAdd},
		},
	}
}

func (a *App) sidebar() dcl.Widget {
	return dcl.TableView{
		AssignTo:              &a.side,
		HeaderHidden:          true,
		MinSize:               dcl.Size{Width: 230},
		MaxSize:               dcl.Size{Width: 230},
		CustomRowHeight:       28,
		Background:            dcl.SolidColorBrush{Color: cPanel},
		LastColumnStretched:   true,
		Columns:               []dcl.TableViewColumn{{Title: "Category"}},
		Model:                 a.smodel,
		OnCurrentIndexChanged: a.onSidebarPick,
	}
}

func (a *App) table() dcl.Widget {
	return dcl.TableView{
		AssignTo:           &a.tv,
		Background:         dcl.SolidColorBrush{Color: cPanel},
		AlternatingRowBG:   false,
		CustomRowHeight:    26,
		CustomHeaderHeight: 30,
		Columns: []dcl.TableViewColumn{
			{Title: "Name", Width: 360},
			{Title: "Size", Width: 100, Alignment: dcl.AlignFar},
			{Title: "Status", Width: 110},
			{Title: "Time Left", Width: 100, Alignment: dcl.AlignFar},
			{Title: "Speed", Width: 110, Alignment: dcl.AlignFar},
			{Title: "Date", Width: 150},
		},
		Model:           a.model,
		OnItemActivated: a.onItemActivated,
		OnCurrentIndexChanged: func() {
			if i := a.tv.CurrentIndex(); i >= 0 && i < len(a.model.rows) {
				a.selID = a.model.rows[i].ID
			}
		},
		ContextMenuItems: a.contextMenu(),
	}
}

// ---- dark theming applied after Create(), before the window is shown -------

func (a *App) applyDark() {
	applyDarkFrame(a.mw.Handle()) // dark title bar / non-client frame
	darkThemeTree(a.mw.Handle())  // dark scrollbars/headers where supported
	// ApplySysColors resets the list bg, so force our dark colors afterwards.
	a.tv.ApplySysColors()
	a.side.ApplySysColors()
	a.darkenLists()
	a.tv.Invalidate()
	a.side.Invalidate()
	a.resizeColumns()
}

func (a *App) darkenLists() {
	darkenListViewBodies(a.tv.Handle(), cPanel)
	darkenListViewBodies(a.side.Handle(), cPanel)
}

// resizeColumns stretches the Name column to fill the leftover table width so
// the columns span exactly the client area — no trailing white header gap and no
// horizontal scrollbar (IDM shows neither). ClientBoundsPixels already excludes
// the vertical scrollbar, so we fill the full client width less a 1px safety
// margin. Called on resize AND after each populate (the scrollbar appears once
// rows overflow, shrinking the client width).
func (a *App) resizeColumns() {
	if a.tv == nil || a.resizing {
		return // mid-drag: defer the costly re-fit to WM_EXITSIZEMOVE
	}
	cols := a.tv.Columns()
	if cols.Len() < 6 {
		return
	}
	// Use the 96-dpi-logical ClientBounds (NOT ClientBoundsPixels): TableViewColumn
	// widths are logical units, so mixing in physical pixels over-stretches Name on
	// scaled displays.
	total := a.tv.ClientBounds().Width
	other := 0
	for i := 1; i < cols.Len(); i++ {
		other += cols.At(i).Width()
	}
	if nameW := total - other - 1; nameW > 160 && nameW != cols.At(0).Width() {
		cols.At(0).SetWidth(nameW)
	}
	// NB: do NOT re-assert darkenLists() here. LVM_SETBKCOLOR forces a full
	// listview repaint; calling it on every WM_SIZE (fires continuously while
	// dragging) AND every 330ms refresh made resizing very laggy. The dark list
	// bg is a persistent property set once in applyDark — resize doesn't reset it.
}

// ---- polling -------------------------------------------------------------

func (a *App) startPolling() {
	go func() {
		t := time.NewTicker(330 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if getMainWindow() == nil {
				return
			}
			onGUI(a.refresh)
		}
	}()
}

// refresh re-reads the engine and repaints the table, sidebar and status bar.
// Runs only on the GUI thread.
func (a *App) refresh() {
	a.cats = a.eng.Categories()
	tasks := a.eng.List()

	// Only rebuild + repaint when something the UI shows actually changed.
	// Otherwise the polling loop reset+re-selected both lists ~3x/sec, repainting
	// the selected row each time — a visible flicker/blink (worst while hovering,
	// on an all-complete idle list). updateStatus still runs every tick (cheap).
	if sig := a.tasksSig(tasks); sig != a.lastSig {
		a.lastSig = sig
		a.smodel.rebuild(tasks)
		a.smodel.PublishRowsReset()
		a.syncSidebarSelection()

		a.model.rebuild(tasks)
		a.model.PublishRowsReset()
		a.restoreSelection()
		a.resizeColumns() // re-fill Name col: the v-scrollbar's presence (and thus
		// the client width) changes as the row count crosses the overflow threshold.
	}

	a.updateStatus(tasks)
}

// tasksSig is a cheap fingerprint of everything the table/sidebar render, so
// refresh() can skip the model reset when nothing changed. Includes the filter
// (changes which rows show) and every displayed per-task field.
func (a *App) tasksSig(tasks []engine.TaskView) string {
	var b strings.Builder
	b.WriteString(a.filter)
	for i := range tasks {
		t := &tasks[i]
		b.WriteByte('|')
		b.WriteString(t.ID)
		b.WriteByte(':')
		b.WriteString(string(t.Status))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(t.Size, 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(t.Downloaded, 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(int64(t.Speed), 10))
		b.WriteByte(':')
		b.WriteString(t.Dir)
		b.WriteByte(':')
		b.WriteString(t.FileName)
	}
	return b.String()
}

func (a *App) restoreSelection() {
	if a.selID == "" {
		return
	}
	for i, t := range a.model.rows {
		if t.ID == a.selID {
			a.tv.SetCurrentIndex(i)
			return
		}
	}
}

func (a *App) syncSidebarSelection() {
	for i, n := range a.smodel.items {
		if n.key == a.filter {
			a.side.SetCurrentIndex(i)
			return
		}
	}
}

func (a *App) updateStatus(tasks []engine.TaskView) {
	var totalSpeed float64
	active := 0
	shown := len(a.model.rows)
	for _, t := range tasks {
		if t.Status == engine.StatusDownloading {
			totalSpeed += t.Speed
			active++
		}
	}
	s := a.filterLabel() + " — " + itoa(shown) + " item" + plural(shown)
	if active > 0 {
		s += ", " + itoa(active) + " downloading"
	}
	if totalSpeed > 0 {
		s += "      ↓ " + humanSpeed(totalSpeed)
	}
	a.sbItem.SetText(s)
}

// ---- actions -------------------------------------------------------------

func (a *App) doAdd() {
	u := a.urlText()
	if u == "" {
		return
	}
	dir := a.eng.CategoryDir(catOrder[clampIdx(a.catCombo.CurrentIndex(), len(catOrder))])
	seg := segChoices[clampIdx(a.connSel.CurrentIndex(), len(segChoices))]
	if _, err := a.eng.Add(u, "", seg, dir, time.Time{}, false); err != nil {
		a.log.Warn("add failed", "err", err)
		walk.MsgBox(a.mw, "Add failed", err.Error(), walk.MsgBoxIconWarning)
		return
	}
	a.resetURLField()
	a.refresh()
}

func (a *App) onSidebarPick() {
	i := a.side.CurrentIndex()
	if i < 0 || i >= len(a.smodel.items) {
		return
	}
	a.filter = a.smodel.items[i].key
	a.refresh()
}

func (a *App) onItemActivated() {
	t, ok := a.current()
	if !ok {
		return
	}
	if t.Status == engine.StatusCompleted && t.FileExists {
		a.openFile(t.ID)
	} else if activeStatuses[t.Status] && a.onDetail != nil {
		a.onDetail(t.ID)
	}
}

func (a *App) current() (engine.TaskView, bool) {
	i := a.tv.CurrentIndex()
	if i < 0 || i >= len(a.model.rows) {
		return engine.TaskView{}, false
	}
	return a.model.rows[i], true
}

func (a *App) toolActionRun(key string) {
	tasks := a.eng.List()
	t, has := a.current()
	switch key {
	case "add":
		a.urlEdit.SetFocus()
	case "resume":
		if has && (t.Status == engine.StatusPaused || t.Status == engine.StatusFailed) {
			a.try("resume", a.eng.Resume(t.ID))
		} else {
			for _, x := range tasks {
				if x.Status == engine.StatusPaused || x.Status == engine.StatusFailed {
					a.try("resume", a.eng.Resume(x.ID))
				}
			}
		}
	case "stop":
		if has && (t.Status == engine.StatusDownloading || t.Status == engine.StatusQueued) {
			a.try("pause", a.eng.Pause(t.ID))
		} else {
			a.stopAll(tasks)
		}
	case "stopall":
		a.stopAll(tasks)
	case "delete":
		if has {
			a.confirmDelete(t)
		}
	case "delcompleted":
		for _, x := range tasks {
			if x.Status == engine.StatusCompleted {
				a.try("delete", a.eng.Delete(x.ID, false))
			}
		}
	}
	a.refresh()
}

func (a *App) stopAll(tasks []engine.TaskView) {
	for _, x := range tasks {
		if x.Status == engine.StatusDownloading || x.Status == engine.StatusQueued {
			a.try("pause", a.eng.Pause(x.ID))
		}
	}
}

func (a *App) confirmDelete(t engine.TaskView) {
	withFile := t.FileExists
	msg := "Remove \"" + trunc(t.FileName, 60) + "\" from the list?"
	if withFile {
		msg += "\n\nClick Yes to also delete the file from disk, No to keep the file."
		switch walk.MsgBox(a.mw, "Delete", msg, walk.MsgBoxYesNoCancel|walk.MsgBoxIconQuestion) {
		case walk.DlgCmdYes:
			a.try("delete+file", a.eng.Delete(t.ID, true))
		case walk.DlgCmdNo:
			a.try("delete", a.eng.Delete(t.ID, false))
		}
		return
	}
	if walk.MsgBox(a.mw, "Delete", msg, walk.MsgBoxOKCancel|walk.MsgBoxIconQuestion) == walk.DlgCmdOK {
		a.try("delete", a.eng.Delete(t.ID, false))
	}
}

func (a *App) try(what string, err error) {
	if err != nil {
		a.log.Warn("action failed", "action", what, "err", err)
	}
}

func (a *App) openFile(id string) {
	p, err := a.eng.FilePath(id)
	if err != nil || runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Start-Process -LiteralPath '"+strings.ReplaceAll(p, "'", "''")+"'")
	procutil.Hidden(cmd)
	a.try("open", cmd.Start())
}

func (a *App) revealFile(id string) {
	p, err := a.eng.FilePath(id)
	if err != nil || runtime.GOOS != "windows" {
		return
	}
	cmd := exec.Command("explorer.exe", "/select,", p)
	procutil.Hidden(cmd)
	a.try("reveal", cmd.Start())
}

// ---- filtering / category resolution -------------------------------------

func (a *App) catOf(t engine.TaskView) string {
	d := normDir(t.Dir)
	for name, dir := range a.cats {
		if normDir(dir) == d {
			return name
		}
	}
	return "General"
}

func (a *App) matchesFilter(t engine.TaskView) bool {
	switch {
	case a.filter == "all":
		return true
	case a.filter == "unfinished":
		return activeStatuses[t.Status]
	case a.filter == "finished":
		return t.Status == engine.StatusCompleted
	case strings.HasPrefix(a.filter, "cat:"):
		return a.catOf(t) == strings.TrimPrefix(a.filter, "cat:")
	}
	return false
}

func (a *App) filterLabel() string {
	switch {
	case a.filter == "all":
		return "All Downloads"
	case a.filter == "unfinished":
		return "Unfinished"
	case a.filter == "finished":
		return "Finished"
	case strings.HasPrefix(a.filter, "cat:"):
		return strings.TrimPrefix(a.filter, "cat:")
	}
	return "All"
}

// ---- helpers -------------------------------------------------------------

func clampIdx(i, n int) int {
	if i < 0 || i >= n {
		return 0
	}
	return i
}

func itoa(n int) string { return strconv.Itoa(n) }

var statusRank = map[engine.Status]int{
	engine.StatusDownloading: 0, engine.StatusQueued: 1, engine.StatusPaused: 2,
	engine.StatusFailed: 3, engine.StatusCompleted: 4,
}

// sortRows orders rows by the model's current sort column/direction.
func sortRows(rows []engine.TaskView, key string, asc bool) {
	dir := 1
	if !asc {
		dir = -1
	}
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := rows[i], rows[j]
		var c int
		switch key {
		case "name":
			c = strings.Compare(strings.ToLower(ti.FileName), strings.ToLower(tj.FileName))
		case "size":
			c = cmpInt(ti.Size, tj.Size)
		case "status":
			c = cmpInt(int64(statusRank[ti.Status]), int64(statusRank[tj.Status]))
		case "eta":
			c = cmpF(etaVal(ti), etaVal(tj))
		case "speed":
			c = cmpF(spdVal(ti), spdVal(tj))
		default:
			c = cmpInt(dateVal(ti), dateVal(tj))
		}
		if c == 0 {
			return dateVal(ti) > dateVal(tj)
		}
		return c*dir < 0
	})
}
