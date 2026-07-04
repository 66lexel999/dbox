//go:build windows

package walkui

// Secondary windows (New Download / status / completion / rename), each a native
// top-level walk window serviced by the single main message loop. Engine and
// server callbacks arrive on other goroutines and marshal onto the GUI thread
// via onGUI (WindowBase.Synchronize).

import (
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"

	"myidm/internal/engine"
	"myidm/internal/gui"
	"myidm/internal/procutil"
)

// PopupEngine is the engine surface the secondary windows need.
type PopupEngine interface {
	Add(rawURL, fileName string, segments int, dir string, scheduledAt time.Time, later bool) (engine.TaskView, error)
	AddVideo(rawURL, title, selector, ext, dir string, audio bool, scheduledAt time.Time, later bool) (engine.TaskView, error)
	Categories() map[string]string
	CategoryDir(name string) string
	Get(id string) (engine.TaskView, error)
	FilePath(id string) (string, error)
	Pause(id string) error
	Resume(id string) error
	Delete(id string, removeFile bool) error
	SuppressCompletionPopup(d time.Duration)
}

// open status windows, keyed by task id, so the completion popup can close the
// matching one ("Close" dismisses both).
var (
	detailMu   sync.Mutex
	detailWins = map[string]*walk.MainWindow{}
)

func registerDetail(id string, w *walk.MainWindow) {
	detailMu.Lock()
	detailWins[id] = w
	detailMu.Unlock()
}
func unregisterDetail(id string, w *walk.MainWindow) {
	detailMu.Lock()
	if detailWins[id] == w {
		delete(detailWins, id)
	}
	detailMu.Unlock()
}
func closeDetailWindow(id string) {
	detailMu.Lock()
	w := detailWins[id]
	detailMu.Unlock()
	if w != nil {
		w.Synchronize(func() { w.Close() })
	}
}

// guard runs fn on the GUI thread, recovering from any panic so a bug in a
// window callback can never crash the whole process (a panic that unwinds
// through walk's WndProc/Paint syscall callback takes the app down). It logs
// the panic + stack so the cause is captured.
func guard(log *slog.Logger, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if log != nil {
				log.Error("recovered panic in GUI callback", "where", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			}
		}
	}()
	fn()
}

// closeSoon disposes a popup window AFTER the current event finishes dispatching.
// Closing a walk window synchronously from inside one of its own control event
// handlers panics ("send on closed channel") when that same handler also marked
// the form's layout dirty earlier (e.g. a Label.SetText): walk disposes the form
// (closing its performLayout channel) and then, still unwinding the click event,
// tries to perform the pending layout on the dead form. Deferring the Close via
// Synchronize lets the in-flight event + layout settle on the live form first.
func closeSoon(w *walk.MainWindow) {
	if w != nil {
		w.Synchronize(func() { w.Close() })
	}
}

// ---- shared popup helpers ------------------------------------------------

func darkFrame(w *walk.MainWindow, edits ...*walk.LineEdit) {
	applyDarkFrame(w.Handle())
	darkThemeTree(w.Handle())
	for _, e := range edits {
		if e != nil {
			e.SetTextColor(cText)
		}
	}
}

// showSized sizes a non-modal popup (walk only applies declarative Size in Run(),
// which these windows skip) and centers it on the main window, then shows it.
func showSized(w *walk.MainWindow, width, height int) {
	w.SetSize(walk.Size{Width: width, Height: height})
	if mw := getMainWindow(); mw != nil {
		mb := mw.BoundsPixels()
		wb := w.BoundsPixels()
		w.SetBoundsPixels(walk.Rectangle{
			X:      mb.X + (mb.Width-wb.Width)/2,
			Y:      mb.Y + (mb.Height-wb.Height)/2,
			Width:  wb.Width,
			Height: wb.Height,
		})
	}
	w.SetVisible(true)
}

func osOpenFile(p string) {
	if runtime.GOOS != "windows" || p == "" {
		return
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		"Start-Process -LiteralPath '"+strings.ReplaceAll(p, "'", "''")+"'")
	procutil.Hidden(cmd)
	cmd.Start()
}

func osRevealFile(p string) {
	if runtime.GOOS != "windows" || p == "" {
		return
	}
	cmd := exec.Command("explorer.exe", "/select,", p)
	procutil.Hidden(cmd)
	cmd.Start()
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return 0
}

func fileNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

func formLabel(text string) dcl.Label {
	return dcl.Label{Text: text, TextColor: cMuted, MinSize: dcl.Size{Width: 70}, MaxSize: dcl.Size{Width: 70}}
}

// ---- New Download dialog -------------------------------------------------

type dialogWin struct {
	eng PopupEngine
	log *slog.Logger
	win *walk.MainWindow

	urlEd, nameEd, dirEd *walk.LineEdit
	catCombo             *walk.ComboBox
	msgLbl               *walk.Label

	isVideo bool
	vSel    string
	vExt    string
	vAudio  bool
	vTitle  string
	catIdx  int
	busy    bool
}

// OpenDialog opens the New Download window for the given query.
func OpenDialog(eng PopupEngine, log *slog.Logger, q url.Values) {
	onGUI(func() { guard(log, "buildDialog", func() { buildDialog(eng, log, q) }) })
}

func buildDialog(eng PopupEngine, log *slog.Logger, q url.Values) {
	d := &dialogWin{eng: eng, log: log}
	raw := q.Get("url")
	var name string
	if q.Get("video") == "1" {
		d.isVideo = true
		d.vSel = q.Get("selector")
		d.vExt = q.Get("vext")
		if d.vExt == "" {
			d.vExt = "mp4"
		}
		d.vAudio = q.Get("audio") == "1"
		d.vTitle = q.Get("title")
		d.catIdx = indexOf(catOrder, "Video")
		name = d.vTitle
		if name == "" {
			name = "video"
		}
		name += "." + d.vExt
	} else {
		name = q.Get("name")
		if name == "" {
			name = fileNameFromURL(raw)
		}
	}
	dir := eng.CategoryDir(catOrder[d.catIdx])

	title := "New Download"
	if d.isVideo {
		title = "Video Download"
		if d.vAudio {
			title = "Audio Download"
		}
	}

	var startBtn, laterBtn, cancelBtn, browseBtn *walk.PushButton
	err := dcl.MainWindow{
		AssignTo:   &d.win,
		Title:      title + " — MyIDM",
		Size:       dcl.Size{Width: 660, Height: 286},
		MinSize:    dcl.Size{Width: 520, Height: 286},
		Visible:    false,
		Background: dcl.SolidColorBrush{Color: cBg},
		Layout:     dcl.VBox{Margins: dcl.Margins{Left: 16, Top: 14, Right: 16, Bottom: 14}, Spacing: 8},
		Children: []dcl.Widget{
			dcl.Label{Text: title, TextColor: cText, Font: dcl.Font{PointSize: 11, Bold: true}},
			formRowW(formLabel("Address"), dcl.LineEdit{AssignTo: &d.urlEd, Text: raw, Background: dcl.SolidColorBrush{Color: cPanel2}}),
			formRowW(formLabel("File name"), dcl.LineEdit{AssignTo: &d.nameEd, Text: name, Background: dcl.SolidColorBrush{Color: cPanel2}}),
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					formLabel("Category"),
					dcl.ComboBox{AssignTo: &d.catCombo, Model: catOrder, CurrentIndex: d.catIdx, MaxSize: dcl.Size{Width: 160},
						OnCurrentIndexChanged: func() {
							d.dirEd.SetText(d.eng.CategoryDir(catOrder[clampIdx(d.catCombo.CurrentIndex(), len(catOrder))]))
						}},
					dcl.HSpacer{},
				},
			},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					formLabel("Save to"),
					dcl.LineEdit{AssignTo: &d.dirEd, Text: dir, Background: dcl.SolidColorBrush{Color: cPanel2}},
					dcl.PushButton{AssignTo: &browseBtn, Text: "Browse…", MaxSize: dcl.Size{Width: 90}, OnClicked: d.browse},
				},
			},
			dcl.Label{AssignTo: &d.msgLbl, Text: "", TextColor: cMuted},
			dcl.VSpacer{},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					dcl.HSpacer{},
					dcl.PushButton{AssignTo: &cancelBtn, Text: "Cancel", MaxSize: dcl.Size{Width: 90}, OnClicked: func() { closeSoon(d.win) }},
					dcl.PushButton{AssignTo: &laterBtn, Text: "Download Later", MaxSize: dcl.Size{Width: 120}, OnClicked: func() { guard(d.log, "dialog.start(later)", func() { d.start(true) }) }},
					dcl.PushButton{AssignTo: &startBtn, Text: "Start Download", MaxSize: dcl.Size{Width: 120}, OnClicked: func() { guard(d.log, "dialog.start", func() { d.start(false) }) }},
				},
			},
		},
	}.Create()
	if err != nil {
		log.Error("create dialog window", "err", err)
		return
	}
	d.nameEd.KeyPress().Attach(func(key walk.Key) {
		if key == walk.KeyReturn {
			guard(d.log, "dialog.start(enter)", func() { d.start(false) })
		}
	})
	darkFrame(d.win, d.urlEd, d.nameEd, d.dirEd)
	showSized(d.win, 660, 286)
}

func (d *dialogWin) browse() {
	cur := strings.TrimSpace(d.dirEd.Text())
	go func() {
		if p := gui.PickFolder(cur); p != "" {
			onGUI(func() { d.dirEd.SetText(p) })
		}
	}()
}

func (d *dialogWin) setMsg(s string, c walk.Color) {
	d.msgLbl.SetText(s)
	d.msgLbl.SetTextColor(c)
}

func (d *dialogWin) start(later bool) {
	if d.busy {
		return
	}
	raw := strings.TrimSpace(d.urlEd.Text())
	if raw == "" {
		d.setMsg("Enter a URL", cRed)
		return
	}
	dir := strings.TrimSpace(d.dirEd.Text())
	if dir == "" {
		dir = d.eng.CategoryDir(catOrder[clampIdx(d.catCombo.CurrentIndex(), len(catOrder))])
	}
	name := strings.TrimSpace(d.nameEd.Text())
	d.busy = true
	d.setMsg("Adding…", cMuted)

	var view engine.TaskView
	var err error
	if d.isVideo {
		title := name
		if i := strings.LastIndex(title, "."); i > 0 {
			title = title[:i]
		}
		if title == "" {
			title = d.vTitle
		}
		view, err = d.eng.AddVideo(raw, title, d.vSel, d.vExt, dir, d.vAudio, time.Time{}, false)
	} else {
		view, err = d.eng.Add(raw, name, 0, dir, time.Time{}, false)
	}
	if err != nil {
		d.busy = false
		d.setMsg("Failed: "+err.Error(), cRed)
		return
	}
	if later {
		d.eng.Pause(view.ID)
	} else {
		OpenDetail(d.eng, d.log, view.ID)
	}
	closeSoon(d.win)
}

// ---- status (per-connection) window --------------------------------------

type detailWin struct {
	eng PopupEngine
	log *slog.Logger
	win *walk.MainWindow
	id  string

	t engine.TaskView // latest snapshot, read by the paint funcs (GUI-thread only)

	nameLbl, statusLbl, infoLbl, ratesLbl, connLbl *walk.Label
	bar, segs                                      *walk.CustomWidget
	pauseBtn, resumeBtn, cancelBtn                 *walk.PushButton

	stop chan struct{}
}

// OpenDetail opens (or focuses) the status window for a task.
func OpenDetail(eng PopupEngine, log *slog.Logger, id string) {
	onGUI(func() {
		detailMu.Lock()
		existing := detailWins[id]
		detailMu.Unlock()
		if existing != nil {
			existing.SetFocus()
			return
		}
		guard(log, "buildDetail", func() { buildDetail(eng, log, id) })
	})
}

func buildDetail(eng PopupEngine, log *slog.Logger, id string) {
	d := &detailWin{eng: eng, log: log, id: id, stop: make(chan struct{})}
	if v, err := eng.Get(id); err == nil {
		d.t = v
	}

	err := dcl.MainWindow{
		AssignTo:   &d.win,
		Title:      "Download status — MyIDM",
		Size:       dcl.Size{Width: 560, Height: 380},
		MinSize:    dcl.Size{Width: 440, Height: 320},
		Visible:    false,
		Background: dcl.SolidColorBrush{Color: cBg},
		Layout:     dcl.VBox{Margins: dcl.Margins{Left: 16, Top: 14, Right: 16, Bottom: 14}, Spacing: 6},
		Children: []dcl.Widget{
			dcl.Label{AssignTo: &d.nameLbl, Text: d.t.FileName, TextColor: cText, Font: dcl.Font{PointSize: 10, Bold: true}},
			dcl.Label{AssignTo: &d.statusLbl, Text: statusText(d.t), TextColor: cMuted},
			dcl.CustomWidget{AssignTo: &d.bar, MinSize: dcl.Size{Height: 22}, MaxSize: dcl.Size{Height: 22},
				Background: dcl.SolidColorBrush{Color: cBg}, InvalidatesOnResize: true, PaintMode: dcl.PaintBuffered, Paint: d.paintBar},
			dcl.Label{AssignTo: &d.infoLbl, Text: "", TextColor: cMuted},
			dcl.Label{AssignTo: &d.ratesLbl, Text: "", TextColor: cMuted},
			dcl.Label{AssignTo: &d.connLbl, Text: "Connections", TextColor: cMuted},
			dcl.CustomWidget{AssignTo: &d.segs, MinSize: dcl.Size{Height: 14}, MaxSize: dcl.Size{Height: 14},
				Background: dcl.SolidColorBrush{Color: cBg}, InvalidatesOnResize: true, PaintMode: dcl.PaintBuffered, Paint: d.paintSegs},
			dcl.VSpacer{},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					dcl.PushButton{AssignTo: &d.resumeBtn, Text: "Resume", MaxSize: dcl.Size{Width: 90}, OnClicked: func() { d.eng.Resume(d.id) }},
					dcl.PushButton{AssignTo: &d.pauseBtn, Text: "Pause", MaxSize: dcl.Size{Width: 90}, OnClicked: func() { d.eng.Pause(d.id) }},
					dcl.PushButton{AssignTo: &d.cancelBtn, Text: "Cancel", MaxSize: dcl.Size{Width: 90}, OnClicked: func() {
						d.eng.Delete(d.id, true)
						closeSoon(d.win)
					}},
					dcl.HSpacer{},
					dcl.PushButton{Text: "Close", MaxSize: dcl.Size{Width: 90}, OnClicked: func() { closeSoon(d.win) }},
				},
			},
		},
	}.Create()
	if err != nil {
		log.Error("create detail window", "err", err)
		return
	}

	registerDetail(id, d.win)
	d.win.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		close(d.stop)
		unregisterDetail(id, d.win)
	})

	darkFrame(d.win)
	d.update(d.t)
	showSized(d.win, 560, 400)

	go func() {
		tk := time.NewTicker(400 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-d.stop:
				return
			case <-tk.C:
				v, err := eng.Get(id)
				if err != nil {
					continue
				}
				d.win.Synchronize(func() { guard(d.log, "detail.update", func() { d.update(v) }) })
			}
		}
	}()
}

// paintBar owner-draws the dark progress trough, green fill and centered percent.
func (d *detailWin) paintBar(c *walk.Canvas, _ walk.Rectangle) (err error) {
	defer func() {
		if r := recover(); r != nil && d.log != nil {
			d.log.Error("recovered panic in paintBar", "panic", fmt.Sprint(r))
		}
	}()
	b := d.bar.ClientBounds()
	if br := solidBrush(cPanel2); br != nil {
		c.FillRectangle(br, b)
	}
	frac := clampFrac(d.t.Progress)
	if fw := int(float64(b.Width) * frac); fw > 0 {
		if br := solidBrush(cGreen); br != nil {
			c.FillRectangle(br, walk.Rectangle{X: b.X, Y: b.Y, Width: fw, Height: b.Height})
		}
	}
	pct := "—"
	if d.t.Progress >= 0 {
		pct = fmt.Sprintf("%.1f%%", d.t.Progress*100)
	}
	c.DrawText(pct, d.bar.Font(), cText, b, walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
	return nil
}

// paintSegs owner-draws one dark mini-bar per download connection/segment.
func (d *detailWin) paintSegs(c *walk.Canvas, _ walk.Rectangle) (err error) {
	defer func() {
		if r := recover(); r != nil && d.log != nil {
			d.log.Error("recovered panic in paintSegs", "panic", fmt.Sprint(r))
		}
	}()
	// PaintBuffered: clear the whole widget bg ourselves (incl. the gaps between
	// segments and the empty case), since the background is no longer auto-erased.
	b := d.segs.ClientBounds()
	if br := solidBrush(cBg); br != nil {
		c.FillRectangle(br, b)
	}
	segs := d.t.Segments
	n := len(segs)
	if n == 0 {
		return nil
	}
	const gap = 3
	w := (b.Width - gap*(n-1)) / n
	if w < 1 {
		w = 1
	}
	trough := solidBrush(cPanel2)
	fill := solidBrush(cGreen)
	for i, s := range segs {
		x := b.X + i*(w+gap)
		if trough != nil {
			c.FillRectangle(trough, walk.Rectangle{X: x, Y: b.Y, Width: w, Height: b.Height})
		}
		length := s.End - s.Start + 1
		if length > 0 && s.Done > 0 && fill != nil {
			frac := clampFrac(float64(s.Done) / float64(length))
			c.FillRectangle(fill, walk.Rectangle{X: x, Y: b.Y, Width: int(float64(w) * frac), Height: b.Height})
		}
	}
	return nil
}

func clampFrac(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func (d *detailWin) update(t engine.TaskView) {
	d.t = t
	d.statusLbl.SetText(statusText(t))
	d.statusLbl.SetTextColor(statusColorWalk(t))
	d.infoLbl.SetText(humanBytes(t.Downloaded) + " of " + sizeStr(t))
	rates := nz(humanSpeed(t.Speed), "—")
	rates += "   ·   avg " + nz(humanSpeed(t.SpeedAvg), "—") + "   ·   max " + nz(humanSpeed(t.SpeedMax), "—")
	if eta := humanETA(t.ETA); eta != "" {
		rates += "   ·   ETA " + eta
	}
	d.ratesLbl.SetText(rates)
	d.connLbl.SetText("Connections (" + itoa(len(t.Segments)) + ")")
	d.bar.Invalidate()
	d.segs.Invalidate()

	// toggle action buttons by state
	dl := t.Status == engine.StatusDownloading || t.Status == engine.StatusQueued
	paused := t.Status == engine.StatusPaused || t.Status == engine.StatusFailed
	done := t.Status == engine.StatusCompleted
	d.pauseBtn.SetVisible(dl)
	d.resumeBtn.SetVisible(paused)
	d.cancelBtn.SetEnabled(!done)
}

func sizeStr(t engine.TaskView) string {
	if t.Size > 0 {
		return humanBytes(t.Size)
	}
	return "?"
}

func nz(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// ---- completion popup ----------------------------------------------------

type doneWin struct {
	eng      PopupEngine
	log      *slog.Logger
	win      *walk.MainWindow
	id       string
	t        engine.TaskView
	suppress *walk.CheckBox
}

// OpenDone opens the "Download complete" window for a finished task.
func OpenDone(eng PopupEngine, log *slog.Logger, id string) {
	onGUI(func() { guard(log, "buildDone", func() { buildDone(eng, log, id) }) })
}

func buildDone(eng PopupEngine, log *slog.Logger, id string) {
	d := &doneWin{eng: eng, log: log, id: id}
	if v, err := eng.Get(id); err == nil {
		d.t = v
	}
	exists := d.t.FileExists

	meta := ""
	if d.t.Size > 0 {
		meta = humanBytes(d.t.Size)
	}
	if d.t.FilePath != "" {
		if meta != "" {
			meta += "   ·   "
		}
		meta += d.t.FilePath
	}

	var openBtn, folderBtn *walk.PushButton
	err := dcl.MainWindow{
		AssignTo:   &d.win,
		Title:      "Download complete — MyIDM",
		Size:       dcl.Size{Width: 480, Height: 240},
		MinSize:    dcl.Size{Width: 400, Height: 220},
		Visible:    false,
		Background: dcl.SolidColorBrush{Color: cBg},
		Layout:     dcl.VBox{Margins: dcl.Margins{Left: 16, Top: 16, Right: 16, Bottom: 16}, Spacing: 6},
		Children: []dcl.Widget{
			dcl.Label{Text: "✓  Download complete", TextColor: cGreen, Font: dcl.Font{PointSize: 11, Bold: true}},
			dcl.Label{Text: d.t.FileName, TextColor: cText},
			dcl.Label{Text: meta, TextColor: cMuted, EllipsisMode: dcl.EllipsisPath},
			dcl.CheckBox{AssignTo: &d.suppress, Text: "Don't show this window for 2 hours"},
			dcl.VSpacer{},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					dcl.PushButton{AssignTo: &openBtn, Text: "Open", Enabled: exists, MaxSize: dcl.Size{Width: 90}, OnClicked: func() {
						osOpenFile(d.t.FilePath)
						d.finish()
					}},
					dcl.PushButton{AssignTo: &folderBtn, Text: "Show in folder", Enabled: exists, MaxSize: dcl.Size{Width: 110}, OnClicked: func() {
						osRevealFile(d.t.FilePath)
						d.finish()
					}},
					dcl.HSpacer{},
					dcl.PushButton{Text: "Close", MaxSize: dcl.Size{Width: 90}, OnClicked: d.finish},
				},
			},
		},
	}.Create()
	if err != nil {
		log.Error("create done window", "err", err)
		return
	}
	darkFrame(d.win)
	showSized(d.win, 480, 250)
}

func (d *doneWin) finish() {
	if d.suppress != nil && d.suppress.Checked() {
		d.eng.SuppressCompletionPopup(2 * time.Hour)
	}
	closeDetailWindow(d.id)
	closeSoon(d.win)
}

// ---- rename dialog (modal) ----------------------------------------------

func (a *App) renameDialog(t engine.TaskView) {
	var dlg *walk.Dialog
	var ed *walk.LineEdit
	var ok, cancel *walk.PushButton
	_, _ = dcl.Dialog{
		AssignTo:      &dlg,
		Title:         "Rename",
		DefaultButton: &ok,
		CancelButton:  &cancel,
		Size:          dcl.Size{Width: 420, Height: 130},
		Background:    dcl.SolidColorBrush{Color: cBg},
		Layout:        dcl.VBox{Margins: dcl.Margins{Left: 14, Top: 12, Right: 14, Bottom: 12}, Spacing: 8},
		Children: []dcl.Widget{
			dcl.Label{Text: "New file name:", TextColor: cText},
			dcl.LineEdit{AssignTo: &ed, Text: t.FileName, Background: dcl.SolidColorBrush{Color: cPanel2}},
			dcl.VSpacer{},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true, Spacing: 6},
				Children: []dcl.Widget{
					dcl.HSpacer{},
					dcl.PushButton{AssignTo: &ok, Text: "Rename", MaxSize: dcl.Size{Width: 90}, OnClicked: func() {
						name := strings.TrimSpace(ed.Text())
						if name != "" {
							if _, err := a.eng.RenameFile(t.ID, name); err != nil {
								a.log.Warn("rename failed", "err", err)
							}
						}
						dlg.Accept()
					}},
					dcl.PushButton{AssignTo: &cancel, Text: "Cancel", MaxSize: dcl.Size{Width: 90}, OnClicked: func() { dlg.Cancel() }},
				},
			},
		},
	}.Run(a.mw)
	a.refresh()
}

// formRowW builds a "label  [field]" horizontal row.
func formRowW(label dcl.Label, field dcl.Widget) dcl.Widget {
	return dcl.Composite{
		Layout:   dcl.HBox{MarginsZero: true, Spacing: 6},
		Children: []dcl.Widget{label, field},
	}
}
