// Package ui is the GTK4/Libadwaita front end of Bluefin Report.
//
// puregotk callbacks live in a fixed, never-freed table, so every signal is
// connected exactly once in build*; refreshes only mutate widgets and models.
package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"codeberg.org/puregotk/puregotk/v4/adw"
	"codeberg.org/puregotk/puregotk/v4/gio"
	"codeberg.org/puregotk/puregotk/v4/glib"
	"codeberg.org/puregotk/puregotk/v4/gobject"
	"codeberg.org/puregotk/puregotk/v4/gtk"
	"codeberg.org/puregotk/puregotk/v4/pango"

	"github.com/projectbluefin/bluefin-report/internal/extras"
	"github.com/projectbluefin/bluefin-report/internal/privacy"
	"github.com/projectbluefin/bluefin-report/internal/publish"
	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

const appID = "io.projectbluefin.Report"

// appName is the user-visible name; data/*.desktop and metainfo repeat it.
const appName = "Report a Problem"

// appIcon is the icon name used by the About dialog; data/*.desktop Icon= must match (checked by a test).
const appIcon = "io.projectbluefin.Report"

// previewBytes caps the Attachment tab; exported/published bytes are always full.
const previewBytes = 200 << 10

var queues = []string{"", "3-clanker-queue", "3-human-queue"}

var areas = []string{"", "desktop", "flatpak", "gaming", "hardware", "installer", "dx"}

// Version is set at release build time via -ldflags -X.
var Version = "dev"

// Main-thread dispatch. puregotk caches callbacks by function-pointer address
// in a fixed, never-freed table, so a fresh closure per call would exhaust it;
// every post reuses the single package-level mainCb.
var (
	mainMu sync.Mutex
	mainQ  []func()
	mainCb glib.SourceFunc = func(uintptr) bool {
		mainMu.Lock()
		q := mainQ
		mainQ = nil
		mainMu.Unlock()
		for _, fn := range q {
			fn()
		}
		return false
	}
)

// post runs fn on the GLib main thread.
func post(fn func()) {
	mainMu.Lock()
	mainQ = append(mainQ, fn)
	mainMu.Unlock()
	glib.IdleAdd(&mainCb, 0)
}

type ui struct {
	dryRun   bool
	stateDir string
	app      *adw.Application
	win      *adw.ApplicationWindow
	toasts   *adw.ToastOverlay
	nav      *adw.NavigationView

	// overview
	gen        int
	cancel     context.CancelFunc
	refreshAct *gio.SimpleAction
	snap       *snapshot.Snapshot
	sum        snapshot.Summary
	ovStack    *gtk.Stack
	spinner    *adw.Spinner
	statusPage *adw.StatusPage
	sysRows    [12]*adw.ActionRow
	ident      extras.Identity
	health     *gtk.ListBox

	// metrics
	metrics     *gtk.StringList
	filter      *gtk.StringFilter
	search      *gtk.SearchEntry
	syncResults func() // filtered n-items notify misses 0→0 when the store empties under a query

	// compose
	mode        publish.Mode
	composePage *adw.NavigationPage
	repo        *adw.ActionRow // shows repoName; there is no choice: reports go to the running image's repo
	repoName    string
	helpGroup   *adw.PreferencesGroup
	helpBtns    *adw.WrapBox
	title       *adw.EntryRow
	issueRef    *adw.EntryRow
	desc        *gtk.TextBuffer
	steps       *gtk.TextBuffer
	optIn       []*adw.SwitchRow // parallel to extras.Sections
	attach      *adw.SwitchRow
	includeUser *adw.SwitchRow
	queue       *adw.ComboRow
	area        *adw.ComboRow
	newOnly     []*gtk.Widget
	descGroup   *adw.PreferencesGroup
	reviewBtn   *gtk.Button
	refErr      *gtk.Label
	shots       []string // screenshot paths chosen on Compose
	descView    *gtk.TextView
	addShot     *adw.ButtonRow // last row of shotList
	shotList    *gtk.ListBox
	shotDialog  *gtk.FileDialog

	// review
	payload       privacy.Payload
	reportID      string
	publishing    bool
	issueText     *gtk.TextBuffer
	settingText   bool
	attachText    *gtk.TextBuffer
	removed       *gtk.ListBox
	consentIssue  *adw.SwitchRow
	consentReport *adw.SwitchRow
	publishBtn    *gtk.Button
	saveDialog    *gtk.FileDialog
	shotsGroup    *adw.PreferencesGroup
	consentGroup  *adw.PreferencesGroup

	// done
	donePage *adw.StatusPage
	doneURL  string

	// drafts
	draftsRow   *adw.ActionRow
	draftsCount *gtk.Label
	draftsGrp   *adw.PreferencesGroup
	draftList   *gtk.ListBox
	draftsStack *gtk.Stack // "list" or "empty"
	discardDlg  *adw.AlertDialog
	discardID   string
	discardAct  *gio.SimpleAction

	// sign-in
	signInDlg  *adw.AlertDialog
	codeDlg    *adw.AlertDialog
	codeLabel  *gtk.Label
	codeURL    string
	signCancel context.CancelFunc
	retry      func() // the publish/resume that hit ErrNotSignedIn
}

// keep holds every Go callback so the GC never collects one puregotk still points at.
var keep []any

func hold[T any](f T) *T { p := &f; keep = append(keep, p); return p }

// Run starts the GTK application and returns its exit code.
func Run(dryRun bool) int {
	u := &ui{dryRun: dryRun}
	u.app = adw.NewApplication(appID, gio.GApplicationNonUniqueValue)
	u.app.ConnectActivate(hold(func(gio.Application) { u.activate() }))
	return int(u.app.Run(1, []string{"bluefin-report"}))
}

func (u *ui) activate() {
	if u.win != nil {
		u.win.Present()
		return
	}
	log.Print("ui: building window")
	u.stateDir = stateDir()
	if u.dryRun {
		d, err := os.MkdirTemp("", "bluefin-report-dry-run-")
		if err != nil {
			log.Print("ui: ", err)
			u.app.Quit()
			return
		}
		u.stateDir = d
	}
	u.win = adw.NewApplicationWindow(&u.app.Application)
	u.win.SetDefaultSize(720, 760)
	u.win.SetSizeRequest(360, 294)
	u.win.SetTitle(appName)

	u.nav = adw.NewNavigationView()
	u.nav.Add(u.buildOverview())
	u.buildMetrics()
	u.buildCompose()
	u.buildReview()
	u.buildDone()
	u.buildDrafts()
	u.buildSignIn()

	u.toasts = adw.NewToastOverlay()
	u.toasts.SetChild(&u.nav.Widget)
	u.win.SetContent(&u.toasts.Widget)

	action := func(name string, cb func()) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, nil)
		a.ConnectActivate(hold(func(gio.SimpleAction, uintptr) { cb() }))
		u.win.AddAction(a)
		return a
	}
	u.refreshAct = action("refresh", u.refresh)
	action("about", u.about)
	action("find", func() {
		if u.nav.GetVisiblePageTag() == "metrics" {
			u.search.GrabFocus()
		}
	})
	action("close", func() { u.win.Close() })
	action("drafts", func() { u.refreshDrafts(); u.nav.PushByTag("drafts") })
	action("shortcuts", u.shortcuts)
	action("help", func() {
		log.Print("ui: opening docs")
		gtk.NewUriLauncher("https://docs.projectbluefin.io").Launch(&u.win.Window, nil, nil, 0)
	})
	action("advance", func() {
		switch u.nav.GetVisiblePageTag() {
		case "compose":
			if u.reviewBtn.GetSensitive() {
				u.openReview()
			}
		case "review":
			if u.publishBtn.GetSensitive() {
				u.publish()
			}
		}
	})
	// Rows rebuilt on refresh target these string-parameter actions (report ID,
	// metric value, screenshot path), so no per-row closure is ever connected.
	stringAction := func(name string, cb func(s string)) *gio.SimpleAction {
		a := gio.NewSimpleAction(name, glib.NewVariantType("s"))
		a.ConnectActivate(hold(func(_ gio.SimpleAction, p uintptr) {
			cb((*(**glib.Variant)(unsafe.Pointer(&p))).GetString(nil))
		}))
		u.win.AddAction(a)
		return a
	}
	stringAction("copy-metric", func(value string) {
		u.win.GetClipboard().SetText(value)
		u.toast("Metric value copied")
	})
	stringAction("draft-resume", u.resume)
	u.discardAct = stringAction("draft-discard", func(id string) {
		if u.publishing {
			u.toast("Wait for publishing to finish")
			return
		}
		u.discardID = id
		u.discardDlg.Present(&u.win.Widget)
	})
	stringAction("shot-remove", func(path string) {
		u.shots = slices.DeleteFunc(u.shots, func(p string) bool { return p == path })
		u.syncShots()
	})
	quit := gio.NewSimpleAction("quit", nil)
	quit.ConnectActivate(hold(func(gio.SimpleAction, uintptr) { u.app.Quit() }))
	u.app.AddAction(quit)
	u.app.SetAccelsForAction("win.refresh", []string{"<Control>r", "F5"})
	u.app.SetAccelsForAction("win.find", []string{"<Control>f"})
	u.app.SetAccelsForAction("win.close", []string{"<Control>w"})
	u.app.SetAccelsForAction("win.shortcuts", []string{"<Control>question"})
	u.app.SetAccelsForAction("win.help", []string{"F1"})
	u.app.SetAccelsForAction("win.advance", []string{"<Control>Return"})
	u.app.SetAccelsForAction("app.quit", []string{"<Control>q"})

	u.win.Present()
	u.refreshDrafts()
	u.refresh()
}

// showError reports a failed user action in a dialog: it stays until
// dismissed and appears on whichever page the action ran from.
func (u *ui) showError(heading, body string) {
	log.Printf("ui: error: %s: %s", heading, body)
	if r, n := utf8.DecodeRuneInString(body); n > 0 { // Go errors start lowercase; dialog bodies are sentences
		body = string(unicode.ToUpper(r)) + body[n:]
	}
	a := adw.NewAlertDialog(heading, body)
	a.AddResponse("close", "_Close")
	a.SetDefaultResponse("close")
	a.Present(&u.win.Widget)
}

func (u *ui) toast(msg string) { u.toasts.AddToast(adw.NewToast(msg)) }

// toolbar adds the header, plus the dry-run banner below it, to a page.
func (u *ui) toolbar(header *adw.HeaderBar, content *gtk.Widget) *adw.ToolbarView {
	tv := adw.NewToolbarView()
	if header == nil {
		header = adw.NewHeaderBar()
	}
	tv.AddTopBar(&header.Widget)
	if u.dryRun {
		b := adw.NewBanner("Dry run: nothing will be published")
		b.SetRevealed(true)
		tv.AddTopBar(&b.Widget)
	}
	tv.SetContent(content)
	return tv
}

func (u *ui) page(title, tag string, header *adw.HeaderBar, content *gtk.Widget) *adw.NavigationPage {
	p := adw.NewNavigationPage(&u.toolbar(header, content).Widget, title)
	p.SetTag(tag)
	return p
}

// scrolled wraps groups in a clamped, scrollable column.
func scrolled(children ...*gtk.Widget) *gtk.Widget {
	box := gtk.NewBox(gtk.OrientationVerticalValue, 24)
	box.SetMarginTop(24)
	box.SetMarginBottom(24)
	box.SetMarginStart(12)
	box.SetMarginEnd(12)
	for _, c := range children {
		box.Append(c)
	}
	clamp := adw.NewClamp()
	clamp.SetChild(&box.Widget)
	sw := gtk.NewScrolledWindow()
	sw.SetPolicy(gtk.PolicyNeverValue, gtk.PolicyAutomaticValue)
	sw.SetChild(&clamp.Widget)
	sw.SetVexpand(true)
	return &sw.Widget
}

func group(title string) *adw.PreferencesGroup {
	g := adw.NewPreferencesGroup()
	g.SetTitle(title)
	return g
}

func pill(label string, cb func()) *gtk.Button {
	b := gtk.NewButtonWithLabel(label)
	b.AddCssClass("pill")
	b.ConnectClicked(hold(func(gtk.Button) { cb() }))
	return b
}

// ---------- Overview ----------

func (u *ui) buildOverview() *adw.NavigationPage {
	header := adw.NewHeaderBar()
	refreshBtn := gtk.NewButtonFromIconName("view-refresh-symbolic")
	refreshBtn.SetTooltipText("Refresh")
	refreshBtn.SetActionName("win.refresh")
	header.PackStart(&refreshBtn.Widget)
	menu := gio.NewMenu()
	section := func(items ...[2]string) {
		s := gio.NewMenu()
		for _, it := range items {
			s.Append(it[0], it[1])
		}
		item := gio.NewMenuItemSection("", &s.MenuModel)
		item.SetAttributeValue("label", nil) // unlabeled: a separator, not a blank header
		menu.AppendItem(item)
	}
	section([2]string{"_Drafts", "win.drafts"})
	section([2]string{"_Keyboard Shortcuts", "win.shortcuts"}, [2]string{"_Help", "win.help"}, [2]string{"_About " + appName, "win.about"})
	mb := gtk.NewMenuButton()
	mb.SetIconName("open-menu-symbolic")
	mb.SetTooltipText("Main Menu")
	mb.SetMenuModel(&menu.MenuModel)
	mb.SetPrimary(true)
	header.PackEnd(&mb.Widget)

	sys := group("This System")
	for i, t := range []string{"Name", "Version", "Image", "Kernel", "Architecture", "State", "Image ref", "Tag", "Digest", "CPU", "GPU", "GNOME"} {
		r := adw.NewActionRow()
		r.SetTitle(t)
		r.AddCssClass("property")
		u.sysRows[i] = r
		sys.Add(&r.Widget)
	}

	hg := group("Health")
	u.health = gtk.NewListBox()
	u.health.SetSelectionMode(gtk.SelectionNoneValue)
	u.health.AddCssClass("boxed-list")
	hg.Add(&u.health.Widget)

	help := group("Get Help")
	u.helpGroup = help
	btns := adw.NewWrapBox() // two pills side by side, stacked when narrow; unlike GtkFlowBox, not a focus container
	btns.SetChildSpacing(12)
	btns.SetLineSpacing(12)
	btns.SetHalign(gtk.AlignCenterValue)
	btns.SetAlign(0.5)
	report := pill("Report a Problem", func() { u.openCompose(publish.NewIssue) })
	report.AddCssClass("suggested-action")
	confirm := pill("Confirm Existing Issue", func() { u.openCompose(publish.Confirm) })
	same := gtk.NewSizeGroup(gtk.SizeGroupHorizontalValue) // HIG: adjacent buttons share a width
	same.AddWidget(&report.Widget)
	same.AddWidget(&confirm.Widget)
	btns.Append(&report.Widget)
	btns.Append(&confirm.Widget)
	help.Add(&btns.Widget)
	btns.SetSensitive(false) // until a snapshot tells us which repo this image reports to
	u.helpBtns = btns

	browse := adw.NewActionRow()
	browse.SetTitle("Browse All Metrics")
	browse.SetActivatable(true)
	browse.AddSuffix(&gtk.NewImageFromIconName("go-next-symbolic").Widget)
	browse.ConnectActivated(hold(func(adw.ActionRow) { u.nav.PushByTag("metrics") }))
	dg := adw.NewPreferencesGroup() // unfinished reports belong next to the actions that create them
	dg.SetVisible(false)
	u.draftsGrp = dg
	bg := adw.NewPreferencesGroup()
	u.draftsRow = adw.NewActionRow()
	u.draftsRow.SetTitle("Unfinished Reports")
	u.draftsRow.SetActivatable(true)
	u.draftsRow.AddPrefix(&gtk.NewImageFromIconName("document-edit-symbolic").Widget)
	u.draftsCount = gtk.NewLabel("")
	u.draftsCount.AddCssClass("dim-label")
	u.draftsRow.AddSuffix(&u.draftsCount.Widget)
	u.draftsRow.AddSuffix(&gtk.NewImageFromIconName("go-next-symbolic").Widget)
	u.draftsRow.SetActionName("win.drafts")
	dg.Add(&u.draftsRow.Widget)
	bg.Add(&browse.Widget)

	u.spinner = adw.NewSpinner()
	loading := adw.NewStatusPage()
	loading.SetTitle("Collecting system report…")
	loading.SetChild(&u.spinner.Widget)

	u.statusPage = adw.NewStatusPage()
	u.statusPage.SetIconName("dialog-warning-symbolic")
	u.statusPage.SetChild(&pill("Try Again", u.refresh).Widget)

	u.ovStack = gtk.NewStack()
	u.ovStack.AddNamed(&loading.Widget, "loading")
	// Actions first so reporting never needs a scroll; the long property list goes last.
	u.ovStack.AddNamed(scrolled(&help.Widget, &dg.Widget, &hg.Widget, &sys.Widget, &bg.Widget), "ready")
	u.ovStack.AddNamed(&u.statusPage.Widget, "error")
	return u.page(appName, "overview", header, &u.ovStack.Widget)
}

func (u *ui) refresh() {
	if u.cancel != nil {
		u.cancel()
	}
	u.gen++
	gen := u.gen
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	u.cancel = cancel
	u.snap = nil
	u.reviewBtn.SetSensitive(false)
	u.ovStack.SetVisibleChildName("loading")
	u.refreshAct.SetEnabled(false)
	log.Printf("ui: collecting snapshot (gen %d)", gen)
	go func() {
		defer cancel()
		start := time.Now()
		s, err := snapshot.Take(ctx, snapshot.Run, nil)
		id := extras.Probe(ctx)
		post(func() {
			if gen != u.gen {
				log.Printf("ui: dropping stale snapshot gen %d", gen)
				return
			}
			u.refreshAct.SetEnabled(true)
			log.Printf("ui: snapshot gen %d in %s: %d system, %d user metrics, err=%v",
				gen, time.Since(start).Round(time.Millisecond), len(s.System.Metrics), len(s.User.Metrics), err)
			log.Printf("ui: identity ref=%q tag=%q digest=%q cpu=%q gpu=%q gnome=%q", id.Ref, id.Tag, id.Digest, id.CPU, id.GPU, id.GNOME)
			u.ident = id
			u.loaded(s, err)
		})
	}()
}

func (u *ui) loaded(s snapshot.Snapshot, err error) {
	if err != nil {
		u.snap = nil
		if errors.Is(err, snapshot.ErrNotInstalled) {
			u.statusPage.SetTitle("System reports are unavailable")
			u.statusPage.SetDescription(snapshot.Binary + " was not found. This system needs systemd 261 or newer with systemd-report")
		} else {
			u.statusPage.SetTitle("Couldn't collect a report")
			u.statusPage.SetDescription(err.Error())
		}
		u.ovStack.SetVisibleChildName("error")
		return
	}
	u.snap = &s
	u.sum = s.Summary()
	u.repoName = publish.Route(u.sum.ImageID)
	u.helpBtns.SetSensitive(u.repoName != "")
	if u.repoName == "" {
		u.helpGroup.SetDescription("Reporting isn't available for this image (" + u.sum.ImageID + "). Only Dakota and Utah accept reports")
	} else {
		u.helpGroup.SetDescription("Reports go to " + u.repoName + ", the repository for the image this system runs")
	}
	for i, v := range []string{u.sum.Name, u.sum.ImageVersion, u.sum.ImageID, u.sum.Kernel, u.sum.Arch, u.sum.SystemState,
		u.ident.Ref, u.ident.Tag, u.ident.Digest, u.ident.CPU, u.ident.GPU, u.ident.GNOME} {
		if v == "" {
			v = "Unknown"
		}
		u.sysRows[i].SetSubtitle(v)
	}

	u.health.RemoveAll()
	add := func(title, sub, icon string) {
		r := adw.NewActionRow()
		r.SetTitle(title)
		r.SetSubtitle(sub)
		r.AddPrefix(&gtk.NewImageFromIconName(icon).Widget)
		u.health.Append(&r.Widget)
	}
	for _, f := range u.sum.FailedUnits {
		add(f, "Failed unit", "dialog-error-symbolic")
	}
	for _, w := range u.sum.Unreachable {
		add(w, "Source unreachable", "network-offline-symbolic")
	}
	for _, w := range u.sum.Warnings {
		if !slices.Contains(u.sum.Unreachable, w) { // Unreachable is a subset of Warnings
			add(w, "Warning", "dialog-warning-symbolic")
		}
	}
	if len(u.sum.FailedUnits)+len(u.sum.Warnings) == 0 {
		add("No problems found", "No failed units or warnings", "object-select-symbolic")
	}

	rows := make([]string, 0, len(s.System.Metrics)+len(s.User.Metrics))
	row := func(prefix string, m snapshot.Metric) string {
		name := prefix + m.Name
		if m.Object != "" {
			name += " [" + m.Object + "]"
		}
		return name + "\n" + m.String() // bind splits name / value onto two lines
	}
	for _, m := range s.System.Metrics {
		rows = append(rows, row("", m))
	}
	for _, m := range s.User.Metrics {
		rows = append(rows, row("user: ", m))
	}
	u.metrics.Splice(0, u.metrics.GetNItems(), rows)
	u.syncResults()
	u.ovStack.SetVisibleChildName("ready")
	u.syncReview()
}

// ---------- Metrics ----------

func (u *ui) buildMetrics() {
	u.metrics = gtk.NewStringList(nil)
	expr := gtk.NewPropertyExpression(gtk.StringObjectGLibType(), nil, "string")
	u.filter = gtk.NewStringFilter(&expr.Expression)
	u.filter.SetMatchMode(gtk.StringFilterMatchModeSubstringValue)
	u.filter.SetIgnoreCase(true)
	model := gtk.NewFilterListModel(u.metrics, &u.filter.Filter)

	f := gtk.NewSignalListItemFactory()
	f.ConnectSetup(hold(func(_ gtk.SignalListItemFactory, p uintptr) {
		row := gtk.NewBox(gtk.OrientationHorizontalValue, 12)
		row.SetMarginStart(12)
		row.SetMarginEnd(12)
		row.SetMarginTop(6)
		row.SetMarginBottom(6)
		box := gtk.NewBox(gtk.OrientationVerticalValue, 6)
		box.SetHexpand(true)
		for _, mode := range []pango.EllipsizeMode{pango.EllipsizeMiddleValue, pango.EllipsizeEndValue} { // name, value
			l := gtk.NewLabel("")
			l.SetXalign(0)
			l.SetEllipsize(mode)
			l.AddCssClass("monospace")
			if mode == pango.EllipsizeEndValue {
				l.AddCssClass("dim-label")
				l.AddCssClass("caption")
			}
			box.Append(&l.Widget)
		}
		row.Append(&box.Widget)
		copy := gtk.NewButtonFromIconName("edit-copy-symbolic")
		copy.SetTooltipText("Copy Metric Value")
		copy.SetValign(gtk.AlignCenterValue)
		copy.AddCssClass("flat")
		copy.SetActionName("win.copy-metric")
		row.Append(&copy.Widget)
		gtk.ListItemNewFromInternalPtr(p).SetChild(&row.Widget)
	}))
	f.ConnectBind(hold(func(_ gtk.SignalListItemFactory, p uintptr) {
		li := gtk.ListItemNewFromInternalPtr(p)
		s := gtk.StringObjectNewFromInternalPtr(li.GetItem().GoPointer()).GetString()
		name, val, _ := strings.Cut(s, "\n")
		row := li.GetChild()
		box := row.GetFirstChild()
		gtk.LabelNewFromInternalPtr(box.GetFirstChild().GoPointer()).SetText(name)
		gtk.LabelNewFromInternalPtr(box.GetLastChild().GoPointer()).SetText(val)
		box.SetTooltipText(name + " = " + val)
		gtk.ButtonNewFromInternalPtr(row.GetLastChild().GoPointer()).SetActionTargetValue(glib.NewVariantString(val))
	}))
	lv := gtk.NewListView(gtk.NewNoSelection(model), &f.ListItemFactory)
	clamp := adw.NewClampScrollable() // same max width as the other pages
	clamp.SetChild(&lv.Widget)
	sw := gtk.NewScrolledWindow()
	sw.SetChild(&clamp.Widget)
	sw.SetVexpand(true)

	empty := adw.NewStatusPage()
	empty.SetIconName("edit-find-symbolic")
	results := gtk.NewStack()
	results.AddNamed(&sw.Widget, "results")
	results.AddNamed(&empty.Widget, "empty")
	results.SetVexpand(true)
	u.syncResults = func() {
		if model.GetNItems() > 0 {
			results.SetVisibleChildName("results")
			return
		}
		title, desc := emptyState(u.metrics.GetNItems(), u.search.GetText())
		empty.SetTitle(title)
		empty.SetDescription(desc)
		results.SetVisibleChildName("empty")
	}
	model.ConnectNotify(hold(func(gobject.Object, uintptr) { u.syncResults() }))

	search := gtk.NewSearchEntry()
	u.search = search
	search.SetPlaceholderText("Filter metrics")
	a11yLabel(&search.Widget, "Filter metrics") // a placeholder is not an accessible name
	search.ConnectSearchChanged(hold(func(gtk.SearchEntry) {
		u.filter.SetSearch(search.GetText())
		u.syncResults()
	}))
	search.SetMarginTop(6)
	search.SetMarginBottom(6)
	search.SetMarginStart(12)
	search.SetMarginEnd(12)

	box := gtk.NewBox(gtk.OrientationVerticalValue, 0)
	sclamp := adw.NewClamp()
	sclamp.SetChild(&search.Widget)
	box.Append(&sclamp.Widget)
	box.Append(&results.Widget)
	u.syncResults()
	u.nav.Add(u.page("Metrics", "metrics", nil, &box.Widget))
}

// emptyState picks the metrics status page text: an empty collection wins over
// a non-matching query so a retained search never hides a failed collection.
func emptyState(total uint32, query string) (title, desc string) {
	if total > 0 && query != "" {
		return "No Results Found", "Try a different search or clear the filter"
	}
	return "No Metrics Available", "Refresh the snapshot and check the Overview for collection warnings"
}

// ---------- Compose ----------

func (u *ui) buildCompose() {
	g := adw.NewPreferencesGroup()
	u.repo = adw.NewActionRow()
	u.repo.SetTitle("Project")
	u.repo.AddCssClass("property")
	u.repo.SetSubtitleSelectable(true)
	g.Add(&u.repo.Widget)
	sync := hold(func(gobject.Object, uintptr) { u.syncReview() })

	u.title = adw.NewEntryRow()
	u.title.SetTitle("Title")
	u.title.ConnectNotify(sync)
	g.Add(&u.title.Widget)

	u.issueRef = adw.NewEntryRow()
	u.issueRef.SetTitle("Issue link or number (#123)")
	u.issueRef.ConnectNotify(sync)
	g.Add(&u.issueRef.Widget)
	u.refErr = gtk.NewLabel("")
	u.refErr.SetXalign(0)
	u.refErr.SetWrap(true)
	u.refErr.SetMarginTop(6)
	u.refErr.AddCssClass("error")
	u.refErr.AddCssClass("caption")
	u.refErr.SetVisible(false)
	g.Add(&u.refErr.Widget) // not a row: AdwPreferencesGroup places it below the list

	dg := group("What Happened?")
	u.descGroup = dg
	u.desc = gtk.NewTextBuffer(nil)
	tv := gtk.NewTextViewWithBuffer(u.desc)
	u.descView = tv
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
	tv.SetSizeRequest(-1, 160)
	tv.AddCssClass("card")
	tv.SetAcceptsTab(false) // Tab moves focus, as in every other form field
	dg.Add(&tv.Widget)

	stepsGroup := group("Steps to Reproduce (Optional)")
	u.steps = gtk.NewTextBuffer(nil)
	stv := gtk.NewTextViewWithBuffer(u.steps)
	stv.SetWrapMode(gtk.WrapWordCharValue)
	stv.SetTopMargin(12)
	stv.SetBottomMargin(12)
	stv.SetLeftMargin(12)
	stv.SetRightMargin(12)
	stv.SetSizeRequest(-1, 120)
	stv.AddCssClass("card")
	stv.SetAcceptsTab(false)
	a11yLabel(&stv.Widget, "Steps to reproduce")
	stepsGroup.Add(&stv.Widget)

	og := group("Options")
	u.attach = adw.NewSwitchRow()
	u.attach.SetTitle("Attach System Report")
	u.attach.SetSubtitle("Reviewed before anything is sent")
	u.attach.SetActive(true)
	og.Add(&u.attach.Widget)
	u.includeUser = adw.NewSwitchRow()
	u.includeUser.SetTitle("Include User Session")
	og.Add(&u.includeUser.Widget)
	u.queue = adw.NewComboRow()
	u.queue.SetTitle("Who should look at it")
	u.queue.SetModel(gtk.NewStringList([]string{"No preference", "Machine analysis", "Human only"}))
	og.Add(&u.queue.Widget)
	u.area = adw.NewComboRow()
	u.area.SetTitle("Area")
	u.area.SetModel(gtk.NewStringList([]string{"Not sure", "Desktop", "Flatpak apps", "Gaming", "Hardware", "Installer", "Developer tools"}))
	og.Add(&u.area.Widget)

	xg := group("Include Diagnostics")
	xg.SetDescription("Off by default. Scrubbed like the report and shown in Review before anything is sent")
	for _, s := range extras.Sections {
		r := adw.NewSwitchRow()
		r.SetTitle(s.Title)
		xg.Add(&r.Widget)
		u.optIn = append(u.optIn, r)
	}

	sg := group("Screenshots")
	sg.SetDescription("Public. Metadata is stripped by re-encoding. PNG, JPEG or GIF; up to 10 files, 10 MB each")
	u.shotList = gtk.NewListBox()
	u.shotList.SetSelectionMode(gtk.SelectionNoneValue)
	u.shotList.AddCssClass("boxed-list")
	sg.Add(&u.shotList.Widget)
	filter := gtk.NewFileFilter()
	filter.SetName("Images")
	for _, ext := range publish.ScreenshotExts {
		filter.AddSuffix(ext[1:])
	}
	u.shotDialog = gtk.NewFileDialog()
	u.shotDialog.SetTitle("Add Screenshots")
	u.shotDialog.SetDefaultFilter(filter)
	picked := hold(gio.AsyncReadyCallback(func(_, res, _ uintptr) {
		files, err := u.shotDialog.OpenMultipleFinish(gio.SimpleAsyncResultNewFromInternalPtr(res))
		if err != nil || files == nil {
			return // cancelled
		}
		for i := range files.GetNItems() {
			o := files.GetObject(i)
			if p := (&gio.FileBase{Ptr: o.Ptr}).GetPath(); p != "" && !slices.Contains(u.shots, p) && fileExists(p) {
				u.shots = append(u.shots, p)
			}
			o.Unref()
		}
		if len(u.shots) > publish.MaxScreenshots {
			u.shots = u.shots[:publish.MaxScreenshots]
			u.toast(fmt.Sprintf("Only %d screenshots can be attached", publish.MaxScreenshots))
		}
		u.syncShots()
	}))
	u.addShot = adw.NewButtonRow() // HIG: add button row at the end of an editable list
	u.addShot.Ref()                // syncShots re-appends it after RemoveAll
	u.addShot.SetTitle("Add Screenshots…")
	u.addShot.SetStartIconName("list-add-symbolic")
	u.addShot.ConnectActivated(hold(func(adw.ButtonRow) { u.shotDialog.OpenMultiple(&u.win.Window, nil, picked, 0) }))
	u.shotList.Append(&u.addShot.Widget)

	u.reviewBtn = pill("Review", u.openReview)
	u.reviewBtn.AddCssClass("suggested-action")
	u.reviewBtn.SetHalign(gtk.AlignCenterValue)
	u.reviewBtn.SetMarginTop(6)
	u.reviewBtn.SetMarginBottom(6)
	u.reviewBtn.SetSensitive(false)

	u.newOnly = []*gtk.Widget{&u.title.Widget, &u.queue.Widget, &u.area.Widget, &stepsGroup.Widget}
	tv2 := u.toolbar(nil, scrolled(&g.Widget, &dg.Widget, &stepsGroup.Widget, &og.Widget, &xg.Widget, &sg.Widget))
	tv2.AddBottomBar(&u.reviewBtn.Widget) // like Review's Publish: always visible, never scrolled away
	u.composePage = adw.NewNavigationPage(&tv2.Widget, "New Issue")
	u.composePage.SetTag("compose")
	u.nav.Add(u.composePage)
}

// syncShots rebuilds the screenshot list; each Remove button targets win.shot-remove with its path.
func (u *ui) syncShots() {
	u.shotList.RemoveAll()
	for _, p := range u.shots {
		r := adw.NewActionRow()
		r.SetTitle(filepath.Base(p))
		r.SetTitleLines(1)
		b := gtk.NewButtonFromIconName("list-remove-symbolic")
		b.SetTooltipText("Remove")
		b.SetValign(gtk.AlignCenterValue)
		b.AddCssClass("flat")
		b.SetActionName("win.shot-remove")
		b.SetActionTargetValue(glib.NewVariantString(p))
		r.AddSuffix(&b.Widget)
		u.shotList.Append(&r.Widget)
	}
	u.shotList.Append(&u.addShot.Widget)
	if u.consentIssue != nil { // shots changed: Review consent no longer covers them
		u.consentIssue.SetActive(false)
		u.consentReport.SetActive(false)
	}
}

// syncReview enables Review once the snapshot is ready and the mode's
// required field is filled.
func (u *ui) syncReview() {
	if u.reviewBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := strings.TrimSpace(u.title.GetText()) != ""
	msg := ""
	if u.mode == publish.Confirm {
		ref := strings.TrimSpace(u.issueRef.GetText())
		_, err := publish.ParseIssueRef(u.repoName, ref)
		ok = err == nil
		if err != nil && ref != "" {
			msg = "Enter an issue number or a link to an issue in " + u.repoName
		}
	}
	u.refErr.SetText(msg)
	u.refErr.SetVisible(msg != "")
	if msg != "" {
		u.issueRef.AddCssClass("error")
	} else {
		u.issueRef.RemoveCssClass("error")
	}
	u.reviewBtn.SetSensitive(u.snap != nil && ok)
}

// openCompose shows the form for m. A half-written report survives Back or
// Escape; only switching modes (or publishing it) starts a fresh one.
func (u *ui) openCompose(m publish.Mode) {
	if m != u.mode {
		u.resetCompose()
	}
	u.mode = m
	newIssue := m == publish.NewIssue
	for _, w := range u.newOnly {
		w.SetVisible(newIssue)
	}
	u.issueRef.SetVisible(!newIssue)
	desc, page := "What Happened?", "New Issue"
	if !newIssue {
		desc, page = "What You’re Seeing (Optional)", "Confirm Existing Issue"
	}
	u.descGroup.SetTitle(desc)
	a11yLabel(&u.descView.Widget, desc)
	u.composePage.SetTitle(page)
	u.repo.SetSubtitle(u.repoName)
	u.syncReview()
	u.nav.PushByTag("compose")
}

// resetCompose empties the Compose form to its defaults.
func (u *ui) resetCompose() {
	u.title.SetText("")
	u.issueRef.SetText("")
	u.desc.SetText("", -1)
	u.steps.SetText("", -1)
	u.attach.SetActive(true)
	u.includeUser.SetActive(false)
	u.queue.SetSelected(0)
	u.area.SetSelected(0)
	for _, r := range u.optIn {
		r.SetActive(false)
	}
	u.shots = nil
	u.syncShots()
}

// ---------- Review ----------

func monoView(buf *gtk.TextBuffer, editable bool, label string) *gtk.Widget {
	tv := gtk.NewTextViewWithBuffer(buf)
	tv.SetMonospace(true)
	tv.SetEditable(editable)
	tv.SetAcceptsTab(false) // Tab moves focus instead of inserting a tab
	tv.SetWrapMode(gtk.WrapWordCharValue)
	tv.SetTopMargin(12)
	tv.SetBottomMargin(12)
	tv.SetLeftMargin(12)
	tv.SetRightMargin(12)
	a11yLabel(&tv.Widget, label)
	sw := gtk.NewScrolledWindow()
	sw.SetChild(&tv.Widget)
	sw.SetVexpand(true)
	return &sw.Widget
}

func (u *ui) buildReview() {
	u.issueText = gtk.NewTextBuffer(nil)
	u.issueText.ConnectChanged(hold(func(gtk.TextBuffer) {
		if !u.settingText {
			u.consentIssue.SetActive(false) // edited text needs fresh consent (§6.4)
			u.consentReport.SetActive(false)
		}
	}))
	u.attachText = gtk.NewTextBuffer(nil)
	u.removed = gtk.NewListBox()
	u.removed.SetSelectionMode(gtk.SelectionNoneValue)
	u.removed.AddCssClass("boxed-list")

	stack := adw.NewViewStack()
	stack.AddTitledWithIcon(monoView(u.issueText, true, "Issue text"), "issue", "Issue Text", "document-edit-symbolic")
	stack.AddTitledWithIcon(monoView(u.attachText, false, "Attachment"), "attachment", "Attachment", "mail-attachment-symbolic")
	rg := group("What Was Removed")
	rg.SetDescription("Values scrubbed or dropped before sharing")
	rg.Add(&u.removed.Widget)
	rsw := gtk.NewScrolledWindow() // not scrolled(): the page is already clamped and padded
	rsw.SetChild(&rg.Widget)
	stack.AddTitledWithIcon(&rsw.Widget, "removed", "Removed", "edit-clear-symbolic")
	stack.SetSizeRequest(-1, 280) // outer scroll keeps Consent reachable at 360×294

	// Inline, not in the header: it switches only this panel, and the header keeps the page title.
	sw := adw.NewInlineViewSwitcher()
	sw.SetStack(stack)
	sw.SetHalign(gtk.AlignCenterValue)

	u.shotsGroup = group("Attachments")
	u.consentGroup = group("Consent")
	cg := u.consentGroup
	u.consentIssue = adw.NewSwitchRow()
	u.consentIssue.SetTitle("I understand the issue text will be public")
	u.consentReport = adw.NewSwitchRow()
	u.consentReport.SetTitle("I understand the full report will be attached publicly to the issue")
	u.consentReport.SetSubtitle("Anyone who can see the issue can read it")
	sync := hold(func(gobject.Object, uintptr) { u.syncPublish() })
	u.consentIssue.ConnectNotify(sync)
	u.consentReport.ConnectNotify(sync)
	cg.Add(&u.consentIssue.Widget)
	cg.Add(&u.consentReport.Widget)

	u.saveDialog = gtk.NewFileDialog()
	u.saveDialog.SetTitle("Export Report")
	saved := hold(gio.AsyncReadyCallback(func(_, res, _ uintptr) {
		f, err := u.saveDialog.SaveFinish(gio.SimpleAsyncResultNewFromInternalPtr(res))
		if err != nil || f == nil {
			return // cancelled
		}
		path := f.GetPath()
		if err := os.WriteFile(path, u.payload.JSON, 0o600); err != nil {
			u.showError("Couldn't Export Report", err.Error())
			return
		}
		log.Printf("ui: exported %d bytes to %s", len(u.payload.JSON), path)
		u.toast("Report exported")
	}))
	export := pill("Export Report…", func() {
		u.saveDialog.SetInitialName("bluefin-report.json")
		u.saveDialog.Save(&u.win.Window, nil, saved, 0)
	})
	u.publishBtn = pill("Publish", u.publish)
	u.publishBtn.AddCssClass("suggested-action")
	btns := gtk.NewBox(gtk.OrientationHorizontalValue, 12) // not homogeneous: two Export-width pills overflow 360 px
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&export.Widget)
	btns.Append(&u.publishBtn.Widget)

	btns.SetMarginTop(6)
	btns.SetMarginBottom(6)

	tv := u.toolbar(nil, scrolled(&sw.Widget, &stack.Widget, &u.shotsGroup.Widget, &cg.Widget))
	tv.AddBottomBar(&btns.Widget)
	p := adw.NewNavigationPage(&tv.Widget, "Review")
	p.SetTag("review")
	u.nav.Add(p)
}

func (u *ui) syncPublish() {
	if u.publishBtn == nil {
		return // notify fires while the page is still being built
	}
	ok := !u.publishing && u.consentIssue.GetActive() && (!u.attach.GetActive() || u.consentReport.GetActive())
	u.publishBtn.SetSensitive(ok)
	if u.discardAct != nil {
		u.discardAct.SetEnabled(!u.publishing) // greys out every Discard button
	}
}

func bufText(b *gtk.TextBuffer) string {
	var s, e gtk.TextIter
	b.GetBounds(&s, &e)
	return b.GetText(&s, &e, false)
}

// openReview collects the opted-in diagnostics off the main thread, then shows Review.
// With none opted in, nothing is collected.
func (u *ui) openReview() {
	var keys []string
	for i, s := range extras.Sections {
		if u.optIn[i].GetActive() {
			keys = append(keys, s.Key)
		}
	}
	if len(keys) == 0 {
		u.showReview(nil)
		return
	}
	u.reviewBtn.SetSensitive(false)
	gen := u.gen
	go func() {
		vals := extras.Collect(context.Background(), keys)
		post(func() {
			u.syncReview()
			// A refresh during collection cleared or replaced the snapshot these belong to.
			if gen != u.gen || u.snap == nil {
				return
			}
			u.showReview(vals)
		})
	}()
}

func (u *ui) showReview(vals map[string]string) {
	name := ""
	if cur, err := user.Current(); err == nil {
		name = cur.Username
	}
	// Synchronous on purpose: measured ~40 ms (9.5k metrics, 725 KB payload) on a dev host.
	p, err := privacy.Build(*u.snap, privacy.Options{Username: name, IncludeUser: u.includeUser.GetActive(), Extras: vals})
	if err != nil {
		u.showError("Couldn't Prepare Report", err.Error())
		return
	}
	u.payload = p
	u.reportID = publish.NewReportID()
	log.Printf("ui: payload %d bytes, %d rules, %d omitted", len(p.JSON), len(p.Removed), len(p.Omitted))

	u.settingText = true
	u.issueText.SetText(u.markdown(), -1)
	u.settingText = false

	preview := string(p.JSON)
	if len(preview) > previewBytes {
		preview = strings.ToValidUTF8(preview[:previewBytes], "") +
			fmt.Sprintf("\n\n… preview truncated at 200 KB of %d KB. Export and Publish use the full report.\n", len(p.JSON)>>10)
	}
	if !u.attach.GetActive() {
		preview = "No attachment: “Attach system report” is off."
	}
	u.attachText.SetText(preview, -1)

	u.removed.RemoveAll()
	row := func(t, s string) {
		r := adw.NewActionRow()
		r.SetTitle(t)
		r.SetSubtitle(s)
		u.removed.Append(&r.Widget)
	}
	for rule, n := range p.Removed {
		row(rule, fmt.Sprintf("%d values", n))
	}
	for _, f := range p.Omitted {
		row(f, "Omitted to fit the size limit")
	}
	if len(p.Removed)+len(p.Omitted) == 0 {
		row("Nothing", "No values needed scrubbing")
	}
	names := make([]string, len(u.shots))
	for i, p := range u.shots {
		names[i] = filepath.Base(p)
	}
	u.shotsGroup.SetDescription(strings.Join(names, "\n"))
	u.shotsGroup.SetVisible(len(u.shots) > 0)
	u.consentGroup.SetDescription("")
	if len(u.shots) > 0 {
		n := "1 screenshot becomes"
		if len(u.shots) > 1 {
			n = fmt.Sprintf("%d screenshots become", len(u.shots))
		}
		u.consentGroup.SetDescription(n + " public. Metadata is stripped, but image content is not scrubbed.")
	}
	u.consentIssue.SetActive(false)
	u.consentReport.SetActive(false)
	u.consentReport.SetVisible(u.attach.GetActive())
	u.syncPublish()
	u.nav.PushByTag("review")
}

func (u *ui) markdown() string {
	var b strings.Builder
	if u.mode == publish.Confirm {
		b.WriteString("Same problem here.\n\n")
	}
	if d := strings.TrimSpace(bufText(u.desc)); d != "" {
		b.WriteString(d + "\n\n")
	}
	if s := strings.TrimSpace(bufText(u.steps)); s != "" && u.mode == publish.NewIssue {
		b.WriteString("### Steps to reproduce\n\n" + s + "\n\n")
	}
	b.WriteString("| | |\n|---|---|\n")
	for _, kv := range [][2]string{
		{"System", u.sum.Name}, {"Version", u.sum.ImageVersion}, {"Image", u.sum.ImageID},
		{"Kernel", u.sum.Kernel}, {"Arch", u.sum.Arch}, {"State", u.sum.SystemState},
		{"Image ref", u.ident.Ref}, {"Tag", u.ident.Tag}, {"Digest", u.ident.Digest},
		{"CPU", u.ident.CPU}, {"GPU", u.ident.GPU}, {"GNOME", u.ident.GNOME},
		{"Failed units", fmt.Sprint(len(u.sum.FailedUnits))},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", kv[0], kv[1])
	}
	for _, f := range u.sum.FailedUnits {
		fmt.Fprintf(&b, "\n- `%s`", f)
	}
	b.WriteString(extras.Markdown(u.payload.Extras))
	return b.String()
}

func (u *ui) publish() {
	if u.publishing {
		return
	}
	d := publish.Draft{
		ReportID: u.reportID,
		Mode:     u.mode,
		Repo:     u.repoName,
		Body:     bufText(u.issueText),
	}
	if d.Mode == publish.NewIssue {
		d.Title = strings.TrimSpace(u.title.GetText())
		d.Queue = queues[u.queue.GetSelected()]
		d.Area = areas[u.area.GetSelected()]
	} else {
		d.IssueRef = strings.TrimSpace(u.issueRef.GetText())
	}
	if u.attach.GetActive() {
		d.Attachment = u.payload.JSON
	}
	d.Screenshots = slices.Clone(u.shots)
	log.Printf("ui: publishing %s to %s (mode %d, %d attachment bytes, %d screenshots)", d.ReportID, d.Repo, d.Mode, len(d.Attachment), len(d.Screenshots))
	u.run(d.ReportID, func() { u.publish() }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Submit(ctx, gh, u.stateDir, d)
	})
}

func (u *ui) resume(id string) {
	if u.publishing {
		return
	}
	log.Printf("ui: resuming %s", id)
	u.run(id, func() { u.resume(id) }, func(ctx context.Context, gh publish.Exec) (publish.Result, error) {
		return publish.Resume(ctx, gh, u.stateDir, id)
	})
}

// run does one publish step for report id off the main thread; retry repeats it after sign-in.
func (u *ui) run(id string, retry func(), step func(context.Context, publish.Exec) (publish.Result, error)) {
	gh := publish.Exec(publish.RunGH)
	if u.dryRun {
		gh = fakeGH
	}
	u.publishing = true
	u.syncPublish()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		res, err := step(ctx, gh)
		post(func() {
			u.publishing = false
			u.syncPublish()
			log.Printf("ui: publish finished: issue=%q err=%v", res.IssueURL, err)
			switch {
			case errors.Is(err, publish.ErrNotSignedIn):
				u.retry = retry
				u.signInDlg.Present(&u.win.Widget)
			case errors.Is(err, publish.ErrDraftChanged), errors.Is(err, publish.ErrBodyTooLarge):
				u.refreshDrafts()
				u.showError("Couldn't Publish Report", err.Error())
			case err != nil:
				u.refreshDrafts()
				body := err.Error()
				if u.resumable(id) {
					body += "\n\nYour draft was kept. Resume it from Drafts."
				}
				u.showError("Couldn't Publish Report", body)
			default:
				u.refreshDrafts()
				u.doneURL = res.IssueURL
				u.donePage.SetDescription(res.IssueURL)
				if id == u.reportID { // the Compose report, not some other resumed draft
					u.resetCompose()
				}
				u.nav.ReplaceWithTags([]string{"overview", "done"}, 2) // Back from Done goes home, not to Publish
			}
		})
	}()
}

// resumable reports whether Drafts holds report id in a form Resume can still publish.
func (u *ui) resumable(id string) bool {
	ds, _ := publish.Pending(u.stateDir)
	for _, s := range ds {
		if s.ReportID == id {
			return slices.Contains(publish.Repos, s.Draft.Repo)
		}
	}
	return false
}

// ---------- Drafts ----------

func (u *ui) buildDrafts() {
	u.draftList = gtk.NewListBox()
	u.draftList.SetSelectionMode(gtk.SelectionNoneValue)
	u.draftList.AddCssClass("boxed-list")
	g := group("Unfinished Reports")
	g.SetDescription("Discarding only deletes the local copy, never anything already on GitHub")
	g.Add(&u.draftList.Widget)
	empty := adw.NewStatusPage()
	empty.SetIconName("document-edit-symbolic")
	empty.SetTitle("No Unfinished Reports")
	empty.SetDescription("Reports that couldn't be published appear here")
	u.draftsStack = gtk.NewStack()
	u.draftsStack.AddNamed(scrolled(&g.Widget), "list")
	u.draftsStack.AddNamed(&empty.Widget, "empty")
	u.nav.Add(u.page("Drafts", "drafts", nil, &u.draftsStack.Widget))

	u.discardDlg = adw.NewAlertDialog("Discard Draft?", "The local copy is deleted. Anything already published stays on GitHub.")
	u.discardDlg.Ref() // reused: keep it alive between presentations
	u.discardDlg.AddResponse("cancel", "_Cancel")
	u.discardDlg.AddResponse("discard", "_Discard")
	u.discardDlg.SetResponseAppearance("discard", adw.ResponseDestructiveValue)
	u.discardDlg.SetCloseResponse("cancel")
	u.discardDlg.ConnectResponse(hold(func(_ adw.AlertDialog, r string) {
		if r != "discard" || u.publishing {
			return
		}
		log.Printf("ui: discarding draft %s", u.discardID)
		if err := publish.Discard(u.stateDir, u.discardID); err != nil {
			u.showError("Couldn't Discard Draft", err.Error())
		}
		u.refreshDrafts()
		u.toast("Draft discarded")
	}))
}

// refreshDrafts rebuilds the Drafts list and the Overview row from disk.
func (u *ui) refreshDrafts() {
	ds, err := publish.Pending(u.stateDir)
	if err != nil {
		log.Print("ui: drafts: ", err)
	}
	log.Printf("ui: %d unfinished drafts", len(ds))
	u.draftsCount.SetText(fmt.Sprint(len(ds)))
	u.draftsGrp.SetVisible(len(ds) > 0)
	u.draftList.RemoveAll()
	for _, s := range ds {
		r := adw.NewActionRow()
		t := s.Draft.Title
		if t == "" {
			t = "Comment on #" + s.Draft.IssueRef
		}
		r.SetTitle(t)
		r.SetSubtitle(s.Draft.Repo + " · " + stageText(s))
		resume := gtk.NewButtonWithLabel("Resume")
		resume.SetValign(gtk.AlignCenterValue)
		resume.AddCssClass("flat")
		resume.SetActionName("win.draft-resume")
		resume.SetActionTargetValue(glib.NewVariantString(s.ReportID))
		r.AddSuffix(&resume.Widget)
		discard := gtk.NewButtonFromIconName("user-trash-symbolic")
		discard.SetTooltipText("Discard")
		discard.SetValign(gtk.AlignCenterValue)
		discard.AddCssClass("flat")
		discard.SetActionName("win.draft-discard")
		discard.SetActionTargetValue(glib.NewVariantString(s.ReportID))
		r.AddSuffix(&discard.Widget)
		u.draftList.Append(&r.Widget)
	}
	if err != nil { // the readable drafts are still listed above
		r := adw.NewActionRow()
		r.SetTitle("Some saved reports couldn't be read")
		r.SetSubtitle(err.Error())
		r.SetSubtitleSelectable(true)
		r.AddPrefix(&gtk.NewImageFromIconName("dialog-warning-symbolic").Widget)
		u.draftList.Append(&r.Widget)
	}
	if len(ds) == 0 && err == nil {
		u.draftsStack.SetVisibleChildName("empty")
	} else {
		u.draftsStack.SetVisibleChildName("list")
	}
}

// stageText describes a draft's publish stage in plain words.
func stageText(s publish.Saved) string {
	switch {
	case s.Stage == publish.Reviewed:
		return "Not published yet"
	case s.IssueURL != "":
		return "Partly published"
	}
	return "Interrupted while publishing"
}

// ---------- Sign-in ----------

func (u *ui) buildSignIn() {
	u.signInDlg = adw.NewAlertDialog("Sign In to GitHub",
		"Publishing needs a GitHub account. Sign in with a one-time code in your browser; your draft is kept.")
	u.signInDlg.Ref()
	u.signInDlg.AddResponse("cancel", "_Cancel")
	u.signInDlg.AddResponse("signin", "_Sign In")
	u.signInDlg.SetDefaultResponse("signin")
	u.signInDlg.SetResponseAppearance("signin", adw.ResponseSuggestedValue)
	u.signInDlg.SetCloseResponse("cancel")
	u.signInDlg.ConnectResponse(hold(func(_ adw.AlertDialog, r string) {
		if r == "signin" {
			u.signIn()
		}
	}))

	u.codeLabel = gtk.NewLabel("…")
	u.codeLabel.AddCssClass("title-1")
	u.codeLabel.SetSelectable(true)
	cp := gtk.NewButtonFromIconName("edit-copy-symbolic")
	cp.SetTooltipText("Copy Code")
	cp.SetValign(gtk.AlignCenterValue)
	cp.AddCssClass("flat")
	cp.ConnectClicked(hold(func(gtk.Button) {
		u.win.GetClipboard().SetText(u.codeLabel.GetText())
		u.toast("Code copied")
	}))
	code := gtk.NewBox(gtk.OrientationHorizontalValue, 6)
	code.SetHalign(gtk.AlignCenterValue)
	code.Append(&u.codeLabel.Widget)
	code.Append(&cp.Widget)
	open := pill("Open Browser", func() {
		gtk.NewUriLauncher(u.codeURL).Launch(&u.win.Window, nil, nil, 0)
	})
	open.AddCssClass("suggested-action")
	open.SetHalign(gtk.AlignCenterValue)
	box := gtk.NewBox(gtk.OrientationVerticalValue, 12)
	box.Append(&code.Widget)
	box.Append(&open.Widget)

	u.codeDlg = adw.NewAlertDialog("Enter this code on GitHub", "Copy the code, open the browser, and paste it there. This closes by itself once GitHub confirms.")
	u.codeDlg.Ref()
	u.codeDlg.SetExtraChild(&box.Widget)
	u.codeDlg.AddResponse("cancel", "_Cancel")
	u.codeDlg.SetCloseResponse("cancel")
	u.codeDlg.ConnectResponse(hold(func(adw.AlertDialog, string) {
		if u.signCancel != nil {
			u.signCancel()
		}
	}))
}

func (u *ui) signIn() {
	gh := publish.StreamExec(publish.RunGHStream)
	if u.dryRun {
		gh = fakeGHStream
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	u.signCancel = cancel
	u.codeLabel.SetText("…")
	u.codeURL = "https://github.com/login/device"
	u.codeDlg.Present(&u.win.Widget)
	log.Print("ui: signing in to GitHub")
	go func() {
		defer cancel()
		err := publish.SignIn(ctx, gh, func(code, url string) {
			post(func() {
				log.Printf("ui: sign-in code received (%s)", url)
				u.codeLabel.SetText(code)
				u.codeURL = url
			})
		})
		post(func() {
			u.signCancel = nil
			u.codeDlg.ForceClose()
			log.Printf("ui: sign-in finished: err=%v", err)
			if err != nil {
				if ctx.Err() == nil { // not cancelled by the user
					u.showError("Couldn't Sign In", err.Error())
				}
				return
			}
			u.toast("Signed in to GitHub")
			if u.retry != nil {
				u.retry()
			}
		})
	}()
}

// ---------- Shortcuts ----------

func (u *ui) shortcuts() {
	s := adw.NewShortcutsSection("")
	for _, kv := range [][2]string{
		{"Refresh snapshot", "<Control>r F5"},
		{"Search metrics", "<Control>f"},
		{"Back", "<Alt>Left Escape"},
		{"Continue to Review or Publish", "<Control>Return"},
		{"Keyboard shortcuts", "<Control>question"},
		{"Help", "F1"},
		{"Close window", "<Control>w"},
		{"Quit", "<Control>q"},
	} {
		s.Add(adw.NewShortcutsItem(kv[0], kv[1]))
	}
	d := adw.NewShortcutsDialog()
	d.Add(s)
	d.Present(&u.win.Widget)
}

func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "bluefin-report", "drafts")
}

// fakeGH records gh calls on stderr and returns plausible URLs (--dry-run).
func fakeGH(_ context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	log.Printf("dry-run gh %q (stdin %d bytes)", args, len(stdin))
	switch {
	case len(args) > 1 && args[0] == "api" && args[1] == "--method":
		return []byte("https://github.com/projectbluefin/dry-run/issues/1 kind/bug\n"), nil, nil
	case len(args) > 1 && args[0] == "issue":
		return []byte("https://github.com/projectbluefin/dry-run/issues/1\n"), nil, nil
	}
	return nil, nil, nil
}

// fakeGHStream signs in instantly (--dry-run).
func fakeGHStream(_ context.Context, _ []byte, onLine func(string), args ...string) error {
	log.Printf("dry-run gh %q", args)
	onLine("! First copy your one-time code: DRY0-RUN0")
	onLine("Open this URL to continue in your web browser: https://github.com/login/device")
	return nil
}

// ---------- Done ----------

func (u *ui) buildDone() {
	u.donePage = adw.NewStatusPage()
	u.donePage.SetIconName("object-select-symbolic") // emblem-ok-symbolic is gone from the Adwaita icon theme
	u.donePage.SetTitle("Report published")
	open := pill("Open Issue", func() {
		gtk.NewUriLauncher(u.doneURL).Launch(&u.win.Window, nil, nil, 0)
	})
	open.AddCssClass("suggested-action")
	cp := pill("Copy Link", func() {
		u.win.GetClipboard().SetText(u.doneURL)
		u.toast("Link copied")
	})
	btns := gtk.NewBox(gtk.OrientationHorizontalValue, 12)
	btns.SetHomogeneous(true) // HIG: adjacent buttons share a width
	btns.SetHalign(gtk.AlignCenterValue)
	btns.Append(&open.Widget)
	btns.Append(&cp.Widget)
	u.donePage.SetChild(&btns.Widget)
	u.nav.Add(u.page("Done", "done", nil, &u.donePage.Widget))
}

func (u *ui) about() {
	a := adw.NewAboutDialog()
	a.SetApplicationName(appName)
	a.SetApplicationIcon(appIcon)
	a.SetDeveloperName("Project Bluefin")
	a.SetVersion(Version)
	a.SetWebsite("https://github.com/projectbluefin/bluefin-report")
	a.SetIssueUrl("https://github.com/projectbluefin/bluefin-report/issues")
	a.SetLicenseType(gtk.LicenseApache20Value)
	a.Present(&u.win.Widget)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// a11yLabel names w for assistive technologies; a placeholder or a nearby heading is not a name.
func a11yLabel(w *gtk.Widget, s string) {
	var v gobject.Value
	v.Init(gobject.TypeStringVal)
	v.SetString(s)
	w.UpdatePropertyValue(1, []gtk.AccessibleProperty{gtk.AccessiblePropertyLabelValue}, []gobject.Value{v})
}
