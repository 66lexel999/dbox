//go:build windows

package walkui

import (
	"sync"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"

	"myidm/internal/engine"
)

// solidBrush caches solid-color brushes (created on the GUI thread, reused for
// every owner-drawn cell).
var (
	brushMu    sync.Mutex
	brushCache = map[walk.Color]*walk.SolidColorBrush{}
)

func solidBrush(c walk.Color) *walk.SolidColorBrush {
	brushMu.Lock()
	defer brushMu.Unlock()
	if b, ok := brushCache[c]; ok {
		return b
	}
	b, err := walk.NewSolidColorBrush(c)
	if err != nil {
		return nil
	}
	brushCache[c] = b
	return b
}

// paintCell owner-draws a dark cell: solid background, an optional 16px icon,
// ellipsized text with the given alignment, and (when grid is set) IDM-style
// 1px column/row separators on the right and bottom edges. walk hands us a
// Canvas only during cell post-paint; when it doesn't (measuring), this returns
// false and the caller falls back to walk's default draw. Touching the Canvas
// makes walk skip its default draw, so we paint everything ourselves.
func paintCell(style *walk.CellStyle, font *walk.Font, icon *walk.Bitmap, text string, fg, bg walk.Color, align walk.DrawTextFormat, indent int, grid bool) bool {
	c := style.Canvas()
	if c == nil {
		return false
	}
	b := style.Bounds()
	if br := solidBrush(bg); br != nil {
		c.FillRectangle(br, b)
	}
	if grid {
		if gb := solidBrush(cGrid); gb != nil {
			c.FillRectangle(gb, walk.Rectangle{X: b.X + b.Width - 1, Y: b.Y, Width: 1, Height: b.Height})
			c.FillRectangle(gb, walk.Rectangle{X: b.X, Y: b.Y + b.Height - 1, Width: b.Width, Height: 1})
		}
	}
	textX := b.X + 6 + indent
	if icon != nil {
		c.DrawImage(icon, walk.Point{X: b.X + 4 + indent, Y: b.Y + (b.Height-16)/2})
		textX = b.X + 4 + indent + 20
	}
	tb := walk.Rectangle{X: textX, Y: b.Y, Width: b.X + b.Width - textX - 6, Height: b.Height}
	c.DrawText(text, font, fg, tb, align|walk.TextVCenter|walk.TextSingleLine|walk.TextEndEllipsis)
	return true
}

func cellText(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// ---- downloads table model ----------------------------------------------

type tableModel struct {
	walk.TableModelBase
	app     *App
	rows    []engine.TaskView
	sortCol int
	order   walk.SortOrder
}

func (m *tableModel) RowCount() int { return len(m.rows) }

func (m *tableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.rows) {
		return ""
	}
	t := m.rows[row]
	switch col {
	case 0:
		return t.FileName
	case 1:
		if t.Size > 0 {
			return humanBytes(t.Size)
		}
		if t.Status == engine.StatusCompleted {
			return humanBytes(t.Downloaded)
		}
		return ""
	case 2:
		return statusText(t)
	case 3:
		if t.Status == engine.StatusDownloading {
			return humanETA(t.ETA)
		}
		return ""
	case 4:
		if t.Status == engine.StatusDownloading {
			return humanSpeed(t.Speed)
		}
		return ""
	case 5:
		ts := t.CreatedAt
		if t.CompletedAt != nil {
			ts = *t.CompletedAt
		}
		return humanDate(ts)
	}
	return ""
}

// columnAlign maps each table column to its text alignment (numeric columns are
// right-aligned, matching IDM).
var columnAlign = []walk.DrawTextFormat{
	walk.TextLeft,  // Name
	walk.TextRight, // Size
	walk.TextLeft,  // Status
	walk.TextRight, // Time Left
	walk.TextRight, // Speed
	walk.TextLeft,  // Date
}

// headerTitles are the column captions, re-drawn by styleHeader (the native
// SysHeader32 paints light/white 3D dividers that DarkMode_Explorer doesn't
// darken, so we over-paint the whole header cell dark instead).
var headerTitles = []string{"Name", "Size", "Status", "Time Left", "Speed", "Date"}

// styleHeader owner-draws a column header cell dark. walk calls StyleCell with
// row == -1 for the header (only when CustomHeaderHeight != 0) at the post-paint
// stage, handing us the Canvas — so we cover the native white dividers with a
// dark fill, the caption, a sort arrow on the active column, and a gray
// separator matching the body grid.
func (m *tableModel) styleHeader(style *walk.CellStyle) {
	col := style.Col()
	title := ""
	if col >= 0 && col < len(headerTitles) {
		title = headerTitles[col]
	}
	if col == m.sortCol {
		if m.order == walk.SortAscending {
			title += "  ▲"
		} else {
			title += "  ▼"
		}
	}
	align := walk.TextLeft
	if col >= 0 && col < len(columnAlign) {
		align = columnAlign[col]
	}
	paintCell(style, m.app.tv.Font(), nil, title, cText, cBg2, align, 0, true)
}

// StyleCell paints the dark theme: every cell gets a solid dark background and a
// 1px gray grid separator (IDM look). The name column carries a tinted category
// icon; the status column is status-colored; numbers are right-aligned + muted.
func (m *tableModel) StyleCell(style *walk.CellStyle) {
	row := style.Row()
	if row == -1 { // header cell
		m.styleHeader(style)
		return
	}
	if row < 0 || row >= len(m.rows) {
		return
	}
	t := m.rows[row]
	bg := cPanel
	if t.ID == m.app.selID {
		bg = cSel
	}
	style.BackgroundColor = bg

	col := style.Col()
	var icon *walk.Bitmap
	fg := cMuted
	switch col {
	case 0:
		icon = catBitmap(m.app.catOf(t))
		fg = cText
	case 2:
		fg = statusColorWalk(t)
	}
	align := walk.TextLeft
	if col >= 0 && col < len(columnAlign) {
		align = columnAlign[col]
	}
	// NB: deliberately do NOT set style.Image as a "no Canvas" fallback. walk asks
	// for a cell image during LVN_GETDISPINFO (no Canvas, empty bounds); setting
	// style.Image there makes walk build a native imagelist and draw icons NATIVELY
	// on top of our owner-drawn ones — those stray native icons bleed into the
	// header / hover bands and flicker through category colors. We always own-draw
	// the icon at paint time (Canvas present), so the imagelist must stay unused.
	if !paintCell(style, m.app.tv.Font(), icon, cellText(m.Value(row, col)), fg, bg, align, 0, true) {
		style.TextColor = fg
	}
}

// ---- walk.Sorter (header-click sorting) ---------------------------------

func (m *tableModel) ColumnSortable(col int) bool { return true }
func (m *tableModel) SortedColumn() int           { return m.sortCol }
func (m *tableModel) SortOrder() walk.SortOrder   { return m.order }

func (m *tableModel) Sort(col int, order walk.SortOrder) error {
	m.sortCol, m.order = col, order
	sortRows(m.rows, colKeys[col], order == walk.SortAscending)
	m.PublishRowsReset()
	m.app.restoreSelection()
	return nil
}

func (m *tableModel) rebuild(tasks []engine.TaskView) {
	rows := tasks[:0:0]
	for _, t := range tasks {
		if m.app.matchesFilter(t) {
			rows = append(rows, t)
		}
	}
	sortRows(rows, colKeys[m.sortCol], m.order == walk.SortAscending)
	m.rows = rows
}

func statusColorWalk(t engine.TaskView) walk.Color {
	switch t.Status {
	case engine.StatusDownloading:
		return cAccent
	case engine.StatusCompleted:
		return cText
	case engine.StatusFailed:
		return cRed
	case engine.StatusPaused, engine.StatusQueued:
		return cMuted
	}
	return cText
}

// ---- sidebar (categories) model -----------------------------------------

type sideNav struct {
	key, label string
	img        *walk.Bitmap
	child      bool
}

type sideModel struct {
	walk.TableModelBase
	app   *App
	items []sideNav
}

func (m *sideModel) RowCount() int                  { return len(m.items) }
func (m *sideModel) Value(row, col int) interface{} { return m.items[row].label }

func (m *sideModel) StyleCell(style *walk.CellStyle) {
	i := style.Row()
	if i < 0 || i >= len(m.items) {
		return
	}
	it := m.items[i]
	bg := cPanel
	if it.key == m.app.filter {
		bg = cSel
	}
	style.BackgroundColor = bg
	style.TextColor = cText
	indent := 0
	if it.child {
		indent = 16
	}
	// No style.Image fallback — it would re-enable walk's native imagelist and
	// double-draw stray icons (see tableModel.StyleCell). We always own-draw.
	paintCell(style, m.app.side.Font(), it.img, it.label, cText, bg, walk.TextLeft, indent, false)
}

func (m *sideModel) rebuild(tasks []engine.TaskView) {
	c := m.app.counts(tasks)
	items := []sideNav{
		{"all", "All Downloads (" + itoa(c["all"]) + ")", navBitmap("all"), false},
	}
	for _, name := range catOrder {
		items = append(items, sideNav{"cat:" + name, name + " (" + itoa(c["cat:"+name]) + ")", catBitmap(name), true})
	}
	items = append(items,
		sideNav{"unfinished", "Unfinished (" + itoa(c["unfinished"]) + ")", navBitmap("unfinished"), false},
		sideNav{"finished", "Finished (" + itoa(c["finished"]) + ")", navBitmap("finished"), false},
	)
	m.items = items
}

func (a *App) counts(tasks []engine.TaskView) map[string]int {
	m := map[string]int{"all": len(tasks)}
	for _, t := range tasks {
		if activeStatuses[t.Status] {
			m["unfinished"]++
		}
		if t.Status == engine.StatusCompleted {
			m["finished"]++
		}
		m["cat:"+a.catOf(t)]++
	}
	return m
}

// ---- context menu --------------------------------------------------------

func (a *App) contextMenu() []dcl.MenuItem {
	act := func(text string, fn func(t engine.TaskView)) dcl.Action {
		return dcl.Action{Text: text, OnTriggered: func() {
			if t, ok := a.current(); ok {
				fn(t)
			}
		}}
	}
	return []dcl.MenuItem{
		act("Open", func(t engine.TaskView) {
			if t.Status == engine.StatusCompleted && t.FileExists {
				a.openFile(t.ID)
			}
		}),
		act("Show in folder", func(t engine.TaskView) { a.revealFile(t.ID) }),
		act("Show status", func(t engine.TaskView) {
			if a.onDetail != nil {
				a.onDetail(t.ID)
			}
		}),
		dcl.Separator{},
		act("Resume", func(t engine.TaskView) { a.try("resume", a.eng.Resume(t.ID)); a.refresh() }),
		act("Pause", func(t engine.TaskView) { a.try("pause", a.eng.Pause(t.ID)); a.refresh() }),
		dcl.Separator{},
		act("Rename…", func(t engine.TaskView) { a.renameDialog(t) }),
		act("Delete", func(t engine.TaskView) { a.confirmDelete(t); a.refresh() }),
	}
}
