package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"trace-ui/jaeger"
	"trace-ui/logger"
)

// ── colour palette ────────────────────────────────────────────────────────────

var (
	colorHeader    = tcell.ColorDodgerBlue
	colorSelected  = tcell.ColorDodgerBlue
	colorBorder    = tcell.ColorDarkSlateGray
	colorTitle     = tcell.ColorAqua
	colorError     = tcell.ColorRed
	colorOK        = tcell.ColorGreen
	colorWarn      = tcell.ColorYellow
	colorDim       = tcell.ColorDarkGray
	colorHighlight = tcell.ColorGold
)

// spanColours cycles through distinct colours for service names in waterfall
var spanColours = []tcell.Color{
	tcell.ColorDodgerBlue,
	tcell.ColorMediumSeaGreen,
	tcell.ColorOrange,
	tcell.ColorOrchid,
	tcell.ColorTomato,
	tcell.ColorDeepSkyBlue,
	tcell.ColorGoldenrod,
	tcell.ColorMediumPurple,
}

// ── App ───────────────────────────────────────────────────────────────────────

type App struct {
	tviewApp *tview.Application
	client   *jaeger.Client

	// state
	mu            sync.Mutex
	services      []string
	operations    []string
	traces        []jaeger.Trace
	selectedTrace *jaeger.Trace
	selectedSpan  *jaeger.Span
	flatSpans     []*jaeger.SpanNode
	collapsed     map[string]bool
	treeOffset    int // how many indent levels are scrolled off the left edge
	labelW        int // cached label column width from last render, used for offset calc
	serviceColMap map[string]tcell.Color
	showBar       bool

	// search params
	searchService   string
	searchOperation string
	searchTags      string
	searchMinDur    string
	searchMaxDur    string
	searchLimit     int
	searchLookback  string

	// widgets
	pages          *tview.Pages
	layout         *tview.Flex
	serviceList    *tview.List
	operationList  *tview.List
	traceTable     *tview.Table
	waterfallView  *tview.TextView
	spanDetailView *tview.TextView
	statusBar      *tview.TextView
	searchBar      *tview.InputField
	helpModal      *tview.Modal
	configModal    *tview.Form

	focusOrder []tview.Primitive
	focusIdx   int
}

func NewApp(client *jaeger.Client) *App {
	a := &App{
		client:         client,
		searchLimit:    20,
		searchLookback: "1h",
		serviceColMap:  map[string]tcell.Color{},
		collapsed:      map[string]bool{},
		showBar:        false,
	}
	a.build()
	return a
}

func (a *App) Run() error {
	return a.tviewApp.Run()
}

// ── UI construction ───────────────────────────────────────────────────────────

func (a *App) build() {
	a.tviewApp = tview.NewApplication()
	a.tviewApp.EnableMouse(true)

	// ── panels ────────────────────────────────────────────────────────────────

	a.serviceList = tview.NewList().
		ShowSecondaryText(false).
		SetHighlightFullLine(true).
		SetSelectedFocusOnly(false)
	a.styleBox(a.serviceList.Box, "Services  [::d](j/k)[white]")
	a.serviceList.SetChangedFunc(func(i int, _ string, _ string, _ rune) {
		a.mu.Lock()
		if i < len(a.services) {
			a.searchService = a.services[i]
		}
		a.mu.Unlock()
		go a.loadOperations() // must be a goroutine — callbacks run on the event loop,
		// and loadOperations calls QueueUpdateDraw which blocks until the event loop
		// is free; calling it directly would deadlock.
	})
	a.serviceList.SetInputCapture(listVimKeys(a.serviceList))

	a.operationList = tview.NewList().
		ShowSecondaryText(false).
		SetHighlightFullLine(true).
		SetSelectedFocusOnly(false)
	a.styleBox(a.operationList.Box, "Operations  [::d](j/k)[white]")
	a.operationList.SetChangedFunc(func(i int, main string, _ string, _ rune) {
		a.mu.Lock()
		if main == "all" {
			a.searchOperation = ""
		} else {
			a.searchOperation = main
		}
		a.mu.Unlock()
	})
	a.operationList.SetInputCapture(listVimKeys(a.operationList))

	a.traceTable = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	a.styleBox(a.traceTable.Box, "Traces  [::d](Enter=open, r=refresh)[white]")
	a.traceTable.SetSelectedFunc(func(row, _ int) {
		if row < 1 || row-1 >= len(a.traces) {
			return
		}
		a.openTrace(&a.traces[row-1])
	})
	a.renderTraceTableHeader()

	a.waterfallView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false)
	a.styleBox(a.waterfallView.Box, "Waterfall  [::d](j/k=span  Space=collapse  h=bar)[white]")
	a.waterfallView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyUp:
			a.selectSpanDeltaInLoop(-1)
			return nil
		case tcell.KeyDown:
			a.selectSpanDeltaInLoop(1)
			return nil
		case tcell.KeyRune:
			switch event.Rune() {
			case 'j':
				a.selectSpanDeltaInLoop(1)
				return nil
			case 'k':
				a.selectSpanDeltaInLoop(-1)
				return nil
			case ' ':
				a.toggleCollapseInLoop()
				return nil
			case 'h':
				a.showBar = !a.showBar
				a.renderWaterfall()
				return nil
			}
		}
		return event
	})

	a.spanDetailView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true)
	a.styleBox(a.spanDetailView.Box, "Span Detail")

	a.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetTextAlign(tview.AlignLeft)
	a.statusBar.SetBackgroundColor(tcell.ColorDarkSlateGray)
	// Set text directly — QueueUpdateDraw can't be used before Run() starts the event loop
	a.statusBar.SetText(" [aqua]trace-ui[white]  [::d]Tab=focus  ?=help  q=quit  /=search  r=refresh  c=config[white]")

	a.searchBar = tview.NewInputField().
		SetLabel(" Search tags (key=value): ").
		SetFieldWidth(40).
		SetPlaceholder("http.status_code=200").
		SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEnter {
				a.mu.Lock()
				a.searchTags = a.searchBar.GetText()
				a.mu.Unlock()
				go a.loadTraces() // goroutine — SetDoneFunc runs on the event loop
			}
			a.pages.HidePage("search")
			a.tviewApp.SetFocus(a.traceTable)
		})

	// ── layout ────────────────────────────────────────────────────────────────

	// Top bar: services + operations side by side.
	topBar := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(a.serviceList, 0, 1, true).
		AddItem(a.operationList, 0, 1, false)

	// Bottom: waterfall + span detail side by side.
	detailPanel := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(a.waterfallView, 0, 3, false).
		AddItem(a.spanDetailView, 0, 2, false)

	mainFlex := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(topBar, 8, 0, true).
		AddItem(a.traceTable, 0, 1, false).
		AddItem(detailPanel, 0, 2, false)

	a.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainFlex, 0, 1, true).
		AddItem(a.statusBar, 1, 0, false)

	// ── modals ────────────────────────────────────────────────────────────────

	a.buildHelpModal()
	a.buildConfigModal()

	a.pages = tview.NewPages().
		AddPage("main", a.layout, true, true).
		AddPage("help", centeredModal(a.helpModal, 70, 30), true, false).
		AddPage("config", centeredModal(a.configModal, 60, 22), true, false).
		AddPage("search", a.floatingSearch(), true, false)

	// ── focus order ───────────────────────────────────────────────────────────

	a.focusOrder = []tview.Primitive{
		a.serviceList, a.operationList, a.traceTable, a.waterfallView, a.spanDetailView,
	}

	// ── global key handler ────────────────────────────────────────────────────

	a.tviewApp.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		// don't intercept when a modal is on top
		if name, _ := a.pages.GetFrontPage(); name != "main" {
			if event.Key() == tcell.KeyEscape || event.Rune() == 'q' {
				a.pages.HidePage(name)
				a.tviewApp.SetFocus(a.focusOrder[a.focusIdx])
			}
			return event
		}
		switch event.Rune() {
		case 'q', 'Q':
			a.tviewApp.Stop()
			return nil
		case '?':
			a.pages.ShowPage("help")
			a.tviewApp.SetFocus(a.helpModal)
			return nil
		case 'c':
			a.pages.ShowPage("config")
			a.tviewApp.SetFocus(a.configModal)
			return nil
		case '/':
			a.pages.ShowPage("search")
			a.tviewApp.SetFocus(a.searchBar)
			return nil
		case 'r':
			go a.loadTraces()
			return nil
		case 'R':
			go a.loadServices()
			return nil
		case 'b':
			// back to trace list from detail view
			a.selectedTrace = nil
			a.selectedSpan = nil
			a.waterfallView.Clear()
			a.spanDetailView.Clear()
			a.tviewApp.SetFocus(a.traceTable)
			return nil
		}
		switch event.Key() {
		case tcell.KeyTab:
			a.rotateFocus(1)
			return nil
		case tcell.KeyBacktab:
			a.rotateFocus(-1)
			return nil
		case tcell.KeyEscape:
			a.selectedTrace = nil
			a.waterfallView.Clear()
			a.spanDetailView.Clear()
			a.setFocusTo(a.traceTable)
			return nil
		}
		return event
	})

	a.tviewApp.SetRoot(a.pages, true)
	a.tviewApp.SetFocus(a.serviceList)
}

func (a *App) styleBox(box *tview.Box, title string) {
	box.SetBorder(true).
		SetBorderColor(colorBorder).
		SetTitleColor(colorTitle).
		SetTitle(" " + title + " ").
		SetTitleAlign(tview.AlignLeft).
		SetBorderPadding(0, 0, 1, 1)
	box.SetFocusFunc(func() {
		box.SetBorderColor(colorSelected)
	})
	box.SetBlurFunc(func() {
		box.SetBorderColor(colorBorder)
	})
}

func (a *App) buildHelpModal() {
	help := `[aqua]trace-ui[white] — Jaeger TUI Explorer

[yellow]Navigation[white]
  Tab / Shift-Tab   cycle focus between panels
  j / k             move up/down in lists
  ↑ / ↓             move in waterfall
  Enter             open trace
  Escape / b        back to trace list

[yellow]Actions[white]
  r                 refresh traces
  R                 reload services
  /                 tag-search (key=value)
  c                 config (host, limit, lookback)
  ?                 toggle this help
  q                 quit

[yellow]Panels[white]
  Services          select a service
  Operations        filter by operation
  Traces            list of matching traces
  Waterfall         span tree with timing bars
  Span Detail       tags, logs for selected span`

	a.helpModal = tview.NewModal().
		SetText(help).
		AddButtons([]string{"Close"}).
		SetDoneFunc(func(_ int, _ string) {
			a.pages.HidePage("help")
			a.tviewApp.SetFocus(a.focusOrder[a.focusIdx])
		})
	a.helpModal.SetBackgroundColor(tcell.ColorDarkSlateBlue)
}

func (a *App) buildConfigModal() {
	a.configModal = tview.NewForm()
	a.configModal.SetBorder(true).
		SetTitle(" Config ").
		SetTitleColor(colorTitle).
		SetBorderColor(colorBorder)

	limitStr := fmt.Sprintf("%d", a.searchLimit)
	a.configModal.
		AddInputField("Jaeger URL", a.client.BaseURL, 36, nil, func(v string) {
			a.client.BaseURL = strings.TrimRight(v, "/")
		}).
		AddInputField("Limit", limitStr, 8, tview.InputFieldInteger, func(v string) {
			n := 0
			fmt.Sscanf(v, "%d", &n)
			if n > 0 {
				a.searchLimit = n
			}
		}).
		AddDropDown("Lookback", []string{"15m", "30m", "1h", "3h", "6h", "12h", "24h", "48h", "7d"}, 2, func(opt string, _ int) {
			a.searchLookback = opt
		}).
		AddButton("Apply & Refresh", func() {
			a.pages.HidePage("config")
			a.tviewApp.SetFocus(a.focusOrder[a.focusIdx])
			go a.loadTraces()
		}).
		AddButton("Cancel", func() {
			a.pages.HidePage("config")
			a.tviewApp.SetFocus(a.focusOrder[a.focusIdx])
		})
}

func (a *App) floatingSearch() tview.Primitive {
	box := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().
			AddItem(nil, 0, 1, false).
			AddItem(a.searchBar, 70, 0, true).
			AddItem(nil, 0, 1, false), 3, 0, true).
		AddItem(nil, 0, 1, false)
	return box
}

func centeredModal(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 0, true).
			AddItem(nil, 0, 1, false), width, 0, true).
		AddItem(nil, 0, 1, false)
}

// ── focus helpers ─────────────────────────────────────────────────────────────

func (a *App) rotateFocus(delta int) {
	n := len(a.focusOrder)
	a.focusIdx = ((a.focusIdx + delta) % n + n) % n
	a.tviewApp.SetFocus(a.focusOrder[a.focusIdx])
}

func (a *App) setFocusTo(p tview.Primitive) {
	for i, pp := range a.focusOrder {
		if pp == p {
			a.focusIdx = i
			break
		}
	}
	a.tviewApp.SetFocus(p)
}

func (a *App) setStatus(msg string) {
	a.tviewApp.QueueUpdateDraw(func() {
		a.statusBar.SetText(" " + msg)
	})
}

func (a *App) setError(msg string) {
	a.setStatus(fmt.Sprintf("[red]✗ %s[white]  [::d]Tab=focus  ?=help  q=quit[white]", msg))
}

// ── data loading ──────────────────────────────────────────────────────────────

func (a *App) LoadInitial() {
	logger.Log("LoadInitial: start")
	a.setStatus("[yellow]Connecting to Jaeger…[white]")
	a.loadServices()
}

func (a *App) loadServices() {
	logger.Log("loadServices: calling GetServices url=%s", a.client.BaseURL)
	a.setStatus("[yellow]Loading services…[white]")
	services, err := a.client.GetServices()
	if err != nil {
		logger.Log("loadServices: error: %v", err)
		a.setError(fmt.Sprintf("GetServices: %v", err))
		return
	}
	logger.Log("loadServices: got %d services: %v", len(services), services)
	a.mu.Lock()
	a.services = services
	a.mu.Unlock()

	a.tviewApp.QueueUpdateDraw(func() {
		logger.Log("loadServices: QueueUpdateDraw fired")
		a.serviceList.Clear()
		for _, svc := range services {
			svc := svc
			a.serviceList.AddItem(svc, "", 0, nil)
		}
		if len(services) > 0 {
			a.serviceList.SetCurrentItem(0)
		}
	})

	if len(services) == 0 {
		logger.Log("loadServices: no services returned")
		a.setStatus("[yellow]No services found.[white]  Is Jaeger running?")
	}
	// loadOperations is triggered via SetCurrentItem(0) → SetChangedFunc → go loadOperations()
}

func (a *App) loadOperations() {
	a.mu.Lock()
	svc := a.searchService
	a.mu.Unlock()
	logger.Log("loadOperations: service=%q", svc)
	if svc == "" {
		logger.Log("loadOperations: empty service, skipping")
		return
	}
	ops, err := a.client.GetOperations(svc)
	if err != nil {
		logger.Log("loadOperations: error: %v", err)
		a.setError(fmt.Sprintf("GetOperations: %v", err))
		return
	}
	logger.Log("loadOperations: got %d operations", len(ops))
	a.mu.Lock()
	a.operations = ops
	a.mu.Unlock()

	a.tviewApp.QueueUpdateDraw(func() {
		logger.Log("loadOperations: QueueUpdateDraw fired")
		a.operationList.Clear()
		a.operationList.AddItem("all", "", 0, nil)
		for _, op := range ops {
			op := op
			a.operationList.AddItem(op, "", 0, nil)
		}
		a.operationList.SetCurrentItem(0)
	})
	a.loadTraces()
}

func (a *App) loadTraces() {
	a.mu.Lock()
	params := jaeger.SearchParams{
		Service:   a.searchService,
		Operation: a.searchOperation,
		Tags:      a.searchTags,
		MinDur:    a.searchMinDur,
		MaxDur:    a.searchMaxDur,
		Limit:     a.searchLimit,
		Lookback:  a.searchLookback,
	}
	a.mu.Unlock()

	logger.Log("loadTraces: params=%+v", params)
	if params.Service == "" {
		logger.Log("loadTraces: empty service, skipping")
		return
	}

	a.setStatus(fmt.Sprintf("[yellow]Loading traces for [aqua]%s[yellow]…[white]", params.Service))
	traces, err := a.client.SearchTraces(params)
	if err != nil {
		logger.Log("loadTraces: error: %v", err)
		a.setError(fmt.Sprintf("SearchTraces: %v", err))
		return
	}
	logger.Log("loadTraces: got %d traces", len(traces))
	a.mu.Lock()
	a.traces = traces
	a.mu.Unlock()
	a.tviewApp.QueueUpdateDraw(func() {
		logger.Log("loadTraces: QueueUpdateDraw fired")
		a.renderTraceTable()
	})
	a.setStatus(fmt.Sprintf(
		"[aqua]%s[white] — [green]%d traces[white]  lookback:[yellow]%s[white]  limit:[yellow]%d[white]  [::d]Tab=focus  ?=help  q=quit  r=refresh  c=config[white]",
		params.Service, len(traces), params.Lookback, params.Limit,
	))
}

// ── trace table ───────────────────────────────────────────────────────────────

func (a *App) renderTraceTableHeader() {
	headers := []string{"Trace ID", "Root Operation", "Service", "Spans", "Duration", "Start"}
	for col, h := range headers {
		a.traceTable.SetCell(0, col, tview.NewTableCell(" "+h+" ").
			SetTextColor(colorHeader).
			SetSelectable(false).
			SetAttributes(tcell.AttrBold))
	}
}

func (a *App) renderTraceTable() {
	// clear data rows
	rc := a.traceTable.GetRowCount()
	for r := 1; r < rc; r++ {
		a.traceTable.RemoveRow(1)
	}

	a.mu.Lock()
	traces := a.traces
	a.mu.Unlock()

	for i, t := range traces {
		t := t
		root := t.RootSpan()
		opName := "-"
		if root != nil {
			opName = root.OperationName
		}
		traceIDShort := t.TraceID
		if len(traceIDShort) > 16 {
			traceIDShort = traceIDShort[:16]
		}
		dur := t.DurationString()
		start := t.StartTime().Format("15:04:05.000")
		svc := t.ServiceName()
		spanCount := fmt.Sprintf("%d", len(t.Spans))

		durColor := colorOK
		d := t.Duration()
		if d > 500*time.Millisecond {
			durColor = colorError
		} else if d > 100*time.Millisecond {
			durColor = colorWarn
		}

		row := i + 1
		a.traceTable.SetCell(row, 0, tview.NewTableCell(" "+traceIDShort).SetTextColor(tcell.ColorAqua))
		a.traceTable.SetCell(row, 1, tview.NewTableCell(" "+truncate(opName, 35)).SetTextColor(tcell.ColorWhite))
		a.traceTable.SetCell(row, 2, tview.NewTableCell(" "+svc).SetTextColor(tcell.ColorMediumSeaGreen))
		a.traceTable.SetCell(row, 3, tview.NewTableCell(" "+spanCount).SetTextColor(tcell.ColorLightGray))
		a.traceTable.SetCell(row, 4, tview.NewTableCell(" "+dur).SetTextColor(durColor))
		a.traceTable.SetCell(row, 5, tview.NewTableCell(" "+start).SetTextColor(colorDim))
	}

	if len(traces) > 0 {
		a.traceTable.ScrollToBeginning()
		a.traceTable.Select(1, 0)
	}
}

// ── trace detail / waterfall ──────────────────────────────────────────────────

func (a *App) openTrace(t *jaeger.Trace) {
	go func() {
		a.setStatus("[yellow]Loading trace detail…[white]")
		full, err := a.client.GetTrace(t.TraceID)
		if err != nil {
			a.setError(fmt.Sprintf("GetTrace: %v", err))
			return
		}
		a.mu.Lock()
		a.selectedTrace = full
		a.selectedSpan = nil
		a.collapsed = map[string]bool{}
		a.treeOffset = 0
		a.flatSpans = a.buildFlatSpans(full)
		a.mu.Unlock()
		a.tviewApp.QueueUpdateDraw(func() {
			a.renderWaterfall()
			a.renderSpanDetail()
			a.setFocusTo(a.waterfallView)
		})
		a.setStatus(fmt.Sprintf(
			"[aqua]%s[white]  [green]%d spans[white]  [yellow]%s[white]  [::d]↑/↓=select span  Esc/b=back[white]",
			t.TraceID[:16], len(full.Spans), full.DurationString(),
		))
	}()
}

func (a *App) serviceColor(name string) tcell.Color {
	if c, ok := a.serviceColMap[name]; ok {
		return c
	}
	c := spanColours[len(a.serviceColMap)%len(spanColours)]
	a.serviceColMap[name] = c
	return c
}

func (a *App) renderWaterfall() {
	a.mu.Lock()
	t := a.selectedTrace
	flat := a.flatSpans
	selected := a.selectedSpan
	a.mu.Unlock()
	if t == nil {
		return
	}

	// Build a set of spanIDs that have children (used for collapse indicator).
	hasChildren := map[string]bool{}
	for _, n := range flat {
		if len(n.Children) > 0 {
			hasChildren[n.Span.SpanID] = true
		}
	}

	// Determine time range across all spans.
	var minStart, maxEnd int64
	for i, s := range t.Spans {
		end := s.StartTime + s.Duration
		if i == 0 || s.StartTime < minStart {
			minStart = s.StartTime
		}
		if end > maxEnd {
			maxEnd = end
		}
	}
	totalDur := maxEnd - minStart
	if totalDur == 0 {
		totalDur = 1
	}

	// Use actual panel width so the bar fits regardless of split layout.
	_, _, panelW, _ := a.waterfallView.GetInnerRect()
	if panelW < 40 {
		panelW = 80
	}

	const svcW = 10
	const durW = 7

	// When bar is hidden: label takes all remaining space minus service/dur columns.
	// When bar is visible: label=32 fixed, bar takes the rest.
	var labelW, barWidth int
	if a.showBar {
		labelW = 32
		barWidth = panelW - labelW - 1 - svcW - 1 - durW - 1
		if barWidth < 8 {
			barWidth = 8
		}
	} else {
		labelW = panelW - 1 - svcW - 1 - durW - 1
		if labelW < 20 {
			labelW = 20
		}
		barWidth = 0
	}
	a.labelW = labelW // cache for offset calculations in input handlers

	var sb strings.Builder

	// Header
	if a.showBar {
		sb.WriteString(fmt.Sprintf("[::b]%-*s %-*s %-*s %s[-:-:-]\n",
			labelW, "Span",
			svcW, "Service",
			barWidth, strings.Repeat("─", barWidth),
			"Dur",
		))
	} else {
		sb.WriteString(fmt.Sprintf("[::b]%-*s %-*s %s[-:-:-]\n",
			labelW, "Span",
			svcW, "Service",
			"Dur",
		))
	}

	selectedRow := -1

	for rowIdx, node := range flat {
		s := node.Span
		svcName := s.ServiceName(t.Processes)
		colHex := colorToHex(a.serviceColor(svcName))
		isSelected := selected != nil && selected.SpanID == s.SpanID
		hasErr := spanHasError(s)

		// Collapse/expand indicator.
		var arrowRune string
		if hasChildren[s.SpanID] {
			if a.collapsed[s.SpanID] {
				arrowRune = "▶"
			} else {
				arrowRune = "▾"
			}
		} else {
			arrowRune = " "
		}

		// Apply tree offset: shift all depths left by treeOffset.
		// Spans shallower than the offset are clamped to depth 0 and get a
		// "‹" prefix to signal they're scrolled past.
		effectiveDepth := node.Depth - a.treeOffset
		var scrolledPast bool
		if effectiveDepth < 0 {
			effectiveDepth = 0
			scrolledPast = true
		}

		indent := strings.Repeat(" ", effectiveDepth)
		prefix := arrowRune
		if scrolledPast {
			prefix = "‹" // ancestor that's been panned past
		}

		rawLabel := indent + prefix + " " + s.OperationName
		labelPlain := runeLimit(rawLabel, labelW)

		// Only the arrow/prefix character is service-colored; name stays white.
		arrowColor := colHex
		if hasErr {
			arrowColor = "red"
		}
		labelRunes := []rune(labelPlain)
		// The colored char is the arrow, which sits at effectiveDepth position.
		arrowPos := effectiveDepth
		if arrowPos >= len(labelRunes) {
			arrowPos = len(labelRunes) - 1
		}
		before := string(labelRunes[:arrowPos])
		arrowCh := string(labelRunes[arrowPos : arrowPos+1])
		after := string(labelRunes[arrowPos+1:])
		coloredLabel := fmt.Sprintf("%s[%s]%s[white]%s", before, arrowColor, arrowCh, after)

		if isSelected {
			selectedRow = rowIdx + 1
		}

		durStr := runePad(s.DurationString(), durW)

		if a.showBar {
			spanStartFrac := float64(s.StartTime-minStart) / float64(totalDur)
			spanWidthFrac := float64(s.Duration) / float64(totalDur)
			leftPad := clamp(int(spanStartFrac*float64(barWidth)), 0, barWidth-1)
			barLen := int(spanWidthFrac * float64(barWidth))
			if barLen < 1 {
				barLen = 1
			}
			if leftPad+barLen > barWidth {
				barLen = barWidth - leftPad
			}
			rightPad := barWidth - leftPad - barLen

			barStr := strings.Repeat(" ", leftPad) +
				"[" + colHex + "]" + strings.Repeat("█", barLen) + "[white]" +
				strings.Repeat(" ", rightPad)

			if isSelected {
				fmt.Fprintf(&sb, "[::r]%s [%s]%-*s[white] %s [yellow]%s[-:-:-]\n",
					coloredLabel,
					colHex, svcW, truncate(svcName, svcW),
					barStr, durStr,
				)
			} else {
				fmt.Fprintf(&sb, "%s[white] [%s]%-*s[white] %s %s[-:-:-]\n",
					coloredLabel,
					colHex, svcW, truncate(svcName, svcW),
					barStr, durStr,
				)
			}
		} else {
			if isSelected {
				fmt.Fprintf(&sb, "[::r]%s [%s]%-*s[white] [yellow]%s[-:-:-]\n",
					coloredLabel,
					colHex, svcW, truncate(svcName, svcW),
					durStr,
				)
			} else {
				fmt.Fprintf(&sb, "%s[white] [%s]%-*s[white] %s[-:-:-]\n",
					coloredLabel,
					colHex, svcW, truncate(svcName, svcW),
					durStr,
				)
			}
		}
	}

	a.waterfallView.SetText(sb.String())

	// Scroll vertically to keep selected span visible.
	if selectedRow >= 0 {
		a.waterfallView.ScrollTo(selectedRow, 0)
	}
}

func (a *App) renderSpanDetail() {
	a.mu.Lock()
	t := a.selectedTrace
	s := a.selectedSpan
	a.mu.Unlock()

	if t == nil {
		a.spanDetailView.Clear()
		return
	}

	// default: show root span
	if s == nil {
		s = t.RootSpan()
	}
	if s == nil {
		return
	}

	svcName := s.ServiceName(t.Processes)
	colHex := colorToHex(a.serviceColor(svcName))

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[::b]%s[::B]\n", s.OperationName))
	sb.WriteString(fmt.Sprintf("[%s]%s[white]\n\n", colHex, svcName))
	sb.WriteString(fmt.Sprintf("[aqua]Span ID:[white] %s\n", s.SpanID))
	sb.WriteString(fmt.Sprintf("[aqua]Duration:[white] %s\n", s.DurationString()))
	sb.WriteString(fmt.Sprintf("[aqua]Start:[white] %s\n\n", time.UnixMicro(s.StartTime).Format("15:04:05.000000")))

	if len(s.Tags) > 0 {
		sb.WriteString("[::b][yellow]Tags[white][::B]\n")
		for _, tag := range s.Tags {
			val := fmt.Sprintf("%v", tag.Value)
			tagColor := "[white]"
			if tag.Key == "error" && val == "true" {
				tagColor = "[red]"
			} else if tag.Key == "http.status_code" {
				code := 0
				fmt.Sscanf(val, "%d", &code)
				if code >= 500 {
					tagColor = "[red]"
				} else if code >= 400 {
					tagColor = "[yellow]"
				} else {
					tagColor = "[green]"
				}
			}
			sb.WriteString(fmt.Sprintf("  [aqua]%s:[white] %s%s[white]\n", tag.Key, tagColor, val))
		}
		sb.WriteString("\n")
	}

	if len(s.Logs) > 0 {
		sb.WriteString("[::b][yellow]Logs[white][::B]\n")
		for _, log := range s.Logs {
			ts := time.UnixMicro(log.Timestamp).Format("15:04:05.000")
			sb.WriteString(fmt.Sprintf("  [aqua]%s[white]\n", ts))
			for _, f := range log.Fields {
				sb.WriteString(fmt.Sprintf("    [dim]%s:[white] %v\n", f.Key, f.Value))
			}
		}
		sb.WriteString("\n")
	}

	if p, ok := t.Processes[s.ProcessID]; ok && len(p.Tags) > 0 {
		sb.WriteString("[::b][yellow]Process Tags[white][::B]\n")
		for _, tag := range p.Tags {
			sb.WriteString(fmt.Sprintf("  [aqua]%s:[white] %v\n", tag.Key, tag.Value))
		}
	}

	a.spanDetailView.SetText(sb.String())
	a.spanDetailView.ScrollToBeginning()
}

// selectSpanDeltaInLoop is called from SetInputCapture (runs on the event loop).
// Must NOT call QueueUpdateDraw — render directly instead.
func (a *App) selectSpanDeltaInLoop(delta int) {
	a.mu.Lock()
	flat := a.flatSpans
	selected := a.selectedSpan
	a.mu.Unlock()

	if len(flat) == 0 {
		return
	}

	idx := 0
	if selected != nil {
		for i, n := range flat {
			if n.Span.SpanID == selected.SpanID {
				idx = i
				break
			}
		}
	}
	idx = clamp(idx+delta, 0, len(flat)-1)
	a.mu.Lock()
	a.selectedSpan = flat[idx].Span
	nameLen := len([]rune(flat[idx].Span.OperationName))
	a.treeOffset = computeTreeOffset(flat[idx].Depth, nameLen, a.labelW)
	a.mu.Unlock()
	// Already on the event loop — render directly, tview will redraw after the handler returns.
	a.renderWaterfall()
	a.renderSpanDetail()
}

// toggleCollapseInLoop toggles collapse state of the selected span and rebuilds
// the flat list. Called from SetInputCapture (event loop) — must not use QueueUpdateDraw.
func (a *App) toggleCollapseInLoop() {
	a.mu.Lock()
	if a.selectedTrace == nil || a.selectedSpan == nil {
		a.mu.Unlock()
		return
	}
	id := a.selectedSpan.SpanID
	a.collapsed[id] = !a.collapsed[id]
	a.flatSpans = a.buildFlatSpans(a.selectedTrace)
	// Re-find selected span depth after rebuild to keep offset consistent.
	for _, n := range a.flatSpans {
		if n.Span.SpanID == id {
			nameLen := len([]rune(n.Span.OperationName))
			a.treeOffset = computeTreeOffset(n.Depth, nameLen, a.labelW)
			break
		}
	}
	a.mu.Unlock()
	a.renderWaterfall()
	a.renderSpanDetail()
}

// buildFlatSpans produces a flat ordered list of span nodes, skipping children
// of collapsed spans.
func (a *App) buildFlatSpans(t *jaeger.Trace) []*jaeger.SpanNode {
	tree := t.SpanTree()
	return a.flattenCollapsed(tree, 0)
}

func (a *App) flattenCollapsed(nodes []*jaeger.SpanNode, depth int) []*jaeger.SpanNode {
	var result []*jaeger.SpanNode
	for _, n := range nodes {
		n.Depth = depth
		result = append(result, n)
		if !a.collapsed[n.Span.SpanID] {
			result = append(result, a.flattenCollapsed(n.Children, depth+1)...)
		}
	}
	return result
}

// ── utils ─────────────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// runeLimit truncates s to at most n visible runes and pads to exactly n.
func runeLimit(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = append(r[:n-1], '…')
	}
	for len(r) < n {
		r = append(r, ' ')
	}
	return string(r)
}

// runePad pads s with spaces to at least n runes without truncating.
func runePad(s string, n int) string {
	r := []rune(s)
	for len(r) < n {
		r = append(r, ' ')
	}
	return string(r)
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// listVimKeys returns an input capture handler that maps j/k to down/up for a List.
// tview's List only handles arrow keys natively.
func listVimKeys(l *tview.List) func(*tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'j':
			cur := l.GetCurrentItem()
			if cur < l.GetItemCount()-1 {
				l.SetCurrentItem(cur + 1)
			}
			return nil
		case 'k':
			cur := l.GetCurrentItem()
			if cur > 0 {
				l.SetCurrentItem(cur - 1)
			}
			return nil
		}
		return event
	}
}

// computeTreeOffset returns the minimum indent offset needed so that the span
// at the given depth with the given operation name is fully visible within
// labelW columns. Returns 0 if no shift is needed.
// arrow(1) + space(1) + name = depth+2+nameLen total runes needed.
func computeTreeOffset(depth, nameRuneLen, labelW int) int {
	needed := depth + 2 + nameRuneLen - labelW
	if needed <= 0 {
		return 0
	}
	return needed
}

func spanHasError(s *jaeger.Span) bool {
	for _, tag := range s.Tags {
		if tag.Key == "error" {
			v := fmt.Sprintf("%v", tag.Value)
			return v == "true" || v == "1"
		}
	}
	return false
}

func colorToHex(c tcell.Color) string {
	r, g, b := c.RGB()
	return fmt.Sprintf("#%02x%02x%02x", r, g, b)
}
