package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bresam/gitlab-copy-tool/internal/config"
	"github.com/bresam/gitlab-copy-tool/internal/gitlabapi"
	"github.com/bresam/gitlab-copy-tool/internal/migrate"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// formFieldCount is the number of focusable fields on the connect screen:
// srcURL, srcToken, srcTransport, tgtURL, tgtToken, tgtTransport.
const formFieldCount = 6

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.logView = newViewport(msg.Width, msg.Height)
		if w := msg.Width - 20; w > 10 {
			if w > 60 {
				w = 60
			}
			m.prog.Width = w
		}
		return m, nil
	}

	// Route by screen.
	switch m.screen {
	case screenSession:
		return m.updateSession(msg)
	case screenConnect:
		return m.updateConnect(msg)
	case screenDiscover:
		return m.updateDiscover(msg)
	case screenMap:
		return m.updateMap(msg)
	case screenTarget:
		return m.updateTarget(msg)
	case screenRun:
		return m.updateRun(msg)
	case screenDone:
		return m.updateDone(msg)
	case screenDryRun:
		return m.updateDryRun(msg)
	}
	return m, nil
}

// --- session picker ------------------------------------------------------

func (m *model) updateSession(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "up", "k":
			if m.sessCursor > 0 {
				m.sessCursor--
			}
		case "down", "j":
			if m.sessCursor < len(m.sessions) {
				m.sessCursor++
			}
		case "d":
			if m.sessCursor < len(m.sessions) {
				_ = config.Remove(m.sessions[m.sessCursor])
				m.sessions, _ = config.List()
				if m.sessCursor > len(m.sessions) {
					m.sessCursor = len(m.sessions)
				}
			}
		case "c":
			// Clear run state (selection/assignments/force/path map) but keep
			// endpoints, tokens and options. In dry-run mode the clear is only
			// temporary (in-memory) and not written to disk.
			if m.sessCursor < len(m.sessions) {
				name := m.sessions[m.sessCursor]
				if m.dryRun {
					m.tempCleared[name] = true
				} else if s, err := config.Load(name); err == nil {
					s.ClearState()
					if err := config.Save(s, time.Now()); err == nil {
						m.cleared[name] = true
					}
				}
			}
		case "enter":
			// Start from a clean working set for the chosen session.
			m.selected = map[int64]bool{}
			m.assign = map[int64]string{}
			m.forced = map[int64]bool{}
			m.optOverride = map[int64]map[int]bool{}
			if m.sessCursor < len(m.sessions) {
				name := m.sessions[m.sessCursor]
				if s, err := config.Load(name); err == nil {
					if m.tempCleared[name] {
						s.ClearState() // in-memory only
					}
					m.session = *s
				}
			} else {
				// New session.
				m.session = config.Session{
					Source:      config.Endpoint{Transport: config.TransportAuto},
					Target:      config.Endpoint{Transport: config.TransportAuto},
					Assignments: map[int64]string{},
					Options:     config.Options{Issues: true, CIVariables: true, Settings: true, URLRewrite: true, Releases: true},
				}
			}
			m.buildInputs()
			m.screen = screenConnect
		}
	}
	return m, nil
}

// --- connect form --------------------------------------------------------

func (m *model) updateConnect(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case connectedMsg:
		m.testing = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		srcClient, tgtClient = msg.src, msg.tgt
		m.srcInfo, m.tgtInfo = msg.srcInfo, msg.tgtInfo
		m.screen = screenDiscover
		m.discovering = true
		return m, tea.Batch(m.spin.Tick, discoverCmd(srcClient, tgtClient))

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.screen = screenSession
			return m, nil
		case "ctrl+s":
			m.syncFormToSession()
			m.testing = true
			m.err = nil
			return m, tea.Batch(m.spin.Tick, connectCmd(m.session.Source, m.session.Target))
		case "tab", "down":
			m.focusField((m.formField + 1) % formFieldCount)
			return m, nil
		case "shift+tab", "up":
			m.focusField((m.formField - 1 + formFieldCount) % formFieldCount)
			return m, nil
		case "left", "right", " ":
			if idx := transportFieldIndex(m.formField); idx >= 0 {
				m.transport[idx] = cycleTransport(m.transport[idx], msg.String() != "left")
				return m, nil
			}
		}
	}

	// Feed key events to the focused text input.
	if inputIdx := inputForField(m.formField); inputIdx >= 0 {
		var cmd tea.Cmd
		m.inputs[inputIdx], cmd = m.inputs[inputIdx].Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) focusField(f int) {
	m.formField = f
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	if idx := inputForField(f); idx >= 0 {
		m.inputs[idx].Focus()
	}
}

// inputForField maps a form field index to a text input index, or -1 for the
// transport selectors.
func inputForField(f int) int {
	switch f {
	case 0:
		return 0 // src url
	case 1:
		return 1 // src token
	case 3:
		return 2 // tgt url
	case 4:
		return 3 // tgt token
	}
	return -1
}

func transportFieldIndex(f int) int {
	switch f {
	case 2:
		return 0
	case 5:
		return 1
	}
	return -1
}

func cycleTransport(cur string, forward bool) string {
	order := []string{config.TransportAuto, config.TransportSSH, config.TransportHTTPS}
	i := 0
	for k, v := range order {
		if v == cur {
			i = k
		}
	}
	if forward {
		i = (i + 1) % len(order)
	} else {
		i = (i - 1 + len(order)) % len(order)
	}
	return order[i]
}

func (m *model) syncFormToSession() {
	m.session.Source.URL = m.inputs[0].Value()
	m.session.Source.Token = m.inputs[1].Value()
	m.session.Target.URL = m.inputs[2].Value()
	m.session.Target.Token = m.inputs[3].Value()
	m.session.Source.Transport = m.transport[0]
	m.session.Target.Transport = m.transport[1]
}

// --- discovery -----------------------------------------------------------

func (m *model) updateDiscover(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case discoveredMsg:
		m.discovering = false
		if msg.err != nil {
			m.err = msg.err
			m.screen = screenConnect
			return m, nil
		}
		m.roots = msg.roots
		m.namespaces = msg.namespaces
		m.rows = flatten(msg.roots, nil, nil)
		m.applySavedSelection()
		m.screen = screenMap
		return m, nil
	case tea.KeyMsg:
		if s := msg.String(); s == "ctrl+c" || s == "q" {
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}

// flatten linearises the tree and precomputes each row's tree-connector prefix.
// ancestorsLast[i] tells whether the ancestor at level i was the last of its
// siblings (→ blank spacer) or not (→ a continuing vertical line).
func flatten(nodes []*gitlabapi.Node, ancestorsLast []bool, acc []treeRow) []treeRow {
	for i, n := range nodes {
		isLast := i == len(nodes)-1
		var sb strings.Builder
		for _, al := range ancestorsLast {
			if al {
				sb.WriteString("   ")
			} else {
				sb.WriteString("│  ")
			}
		}
		if isLast {
			sb.WriteString("└─ ")
		} else {
			sb.WriteString("├─ ")
		}
		acc = append(acc, treeRow{node: n, depth: len(ancestorsLast), prefix: sb.String()})
		child := append(append([]bool{}, ancestorsLast...), isLast)
		acc = flatten(n.Children, child, acc)
	}
	return acc
}

// applySavedSelection restores selection, per-node target assignments and the
// force flags from the session.
func (m *model) applySavedSelection() {
	for _, id := range m.session.Selected {
		m.selected[id] = true
	}
	for _, id := range m.session.Force {
		m.forced[id] = true
	}
	for id, path := range m.session.Assignments {
		m.assign[id] = path
	}
	for id, ov := range m.session.OptionOverrides {
		cp := make(map[int]bool, len(ov))
		for k, v := range ov {
			cp[k] = v
		}
		m.optOverride[id] = cp
	}

	// Auto-enable the container-registry option for repos that actually have
	// images, where the baseline leaves it off and the user hasn't set it.
	for _, r := range m.rows {
		n := r.node
		if n.Kind != "project" || !n.HasContainers {
			continue
		}
		if m.session.Options.Get(config.OptContainerRegistry) {
			continue // already on by baseline
		}
		ov := m.optOverride[n.ID]
		if ov == nil {
			ov = map[int]bool{}
			m.optOverride[n.ID] = ov
		}
		if _, set := ov[config.OptContainerRegistry]; !set {
			ov[config.OptContainerRegistry] = true
		}
	}
}

// --- mapping -------------------------------------------------------------

func (m *model) updateMap(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	if k.String() != "ctrl+s" {
		m.notice = ""
	}
	switch k.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "esc":
		m.screen = screenConnect
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case " ":
		m.toggleRow(m.cursor)
	case "left":
		m.cycleTarget(m.cursor, false)
	case "right":
		m.cycleTarget(m.cursor, true)
	case "a":
		m.setAll(true)
	case "N":
		m.setAll(false)
	case "1":
		m.cycleOption(config.OptIssues)
	case "2":
		m.cycleOption(config.OptCIVariables)
	case "3":
		m.cycleOption(config.OptSettings)
	case "4":
		m.cycleOption(config.OptURLRewrite)
	case "5":
		m.cycleOption(config.OptReleases)
	case "6":
		m.cycleOption(config.OptContainerRegistry)
	case "f":
		m.toggleForce(m.cursor)
	case "enter", "t":
		m.openTargetPicker()
	case "ctrl+s":
		m.persistSession()
		m.notice = "Konfiguration gespeichert"
	case "ctrl+p":
		return m.startRun()
	}
	return m, nil
}

func (m *model) toggleRow(i int) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	n := m.rows[i].node
	if n.Kind == "project" {
		m.selected[n.ID] = !m.selected[n.ID]
		return
	}
	// Group: if any descendant project unselected, select all; else clear.
	ids := descendantProjectIDs(n)
	anyOff := false
	for _, id := range ids {
		if !m.selected[id] {
			anyOff = true
			break
		}
	}
	for _, id := range ids {
		m.selected[id] = anyOff
	}
}

// cycleOption cycles the option override of the highlighted node through
// inherit → on → off → inherit. Setting it on a group cascades to descendants.
func (m *model) cycleOption(idx int) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return
	}
	id := m.rows[m.cursor].node.ID
	ov := m.optOverride[id]
	if ov == nil {
		ov = map[int]bool{}
		m.optOverride[id] = ov
	}
	cur, set := ov[idx]
	switch {
	case !set:
		ov[idx] = true // inherit → on
	case cur:
		ov[idx] = false // on → off
	default:
		delete(ov, idx) // off → inherit
		if len(ov) == 0 {
			delete(m.optOverride, id)
		}
	}
}

// toggleForce flips the per-project force-overwrite flag for the highlighted
// project (or every descendant project of a highlighted group).
func (m *model) toggleForce(i int) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	n := m.rows[i].node
	if n.Kind == "project" {
		m.forced[n.ID] = !m.forced[n.ID]
		return
	}
	ids := descendantProjectIDs(n)
	anyOff := false
	for _, id := range ids {
		if !m.forced[id] {
			anyOff = true
			break
		}
	}
	for _, id := range ids {
		m.forced[id] = anyOff
	}
}

func (m *model) setAll(v bool) {
	for _, r := range m.rows {
		if r.node.Kind == "project" {
			m.selected[r.node.ID] = v
		}
	}
}

// cycleTarget cycles the target-namespace assignment of the highlighted node
// through the candidate list: none -> cand[0] -> cand[1] -> … -> none. A group
// assignment cascades to descendants (see gitlabapi.ResolveTargets).
func (m *model) cycleTarget(i int, forward bool) {
	if i < 0 || i >= len(m.rows) {
		return
	}
	n := m.rows[i].node
	cands := m.candidatesForNode(n)
	if len(cands) == 0 {
		return
	}
	cur, ok := m.assign[n.ID]
	idx := -1 // -1 == none
	if ok {
		for k, c := range cands {
			if c == cur {
				idx = k
				break
			}
		}
		if idx == -1 {
			// current is a manual value not in the list: treat as its own slot
			cands = append([]string{cur}, cands...)
			idx = 0
		}
	}
	last := len(cands) - 1
	next := idx
	if forward {
		next++
	} else {
		next--
	}
	switch {
	case next < 0:
		delete(m.assign, n.ID) // none
	case next > last:
		delete(m.assign, n.ID) // wrap to none
	default:
		m.assign[n.ID] = cands[next]
	}
}

// candidatesForNode builds the ordered target-namespace suggestions for a node:
// the existing writable target namespaces, plus source-derived paths — the
// node's own source location (bottom-up) and that location under each existing
// top-level target account.
func (m *model) candidatesForNode(n *gitlabapi.Node) []string {
	set := map[string]struct{}{}
	var order []string
	add := func(s string) {
		s = strings.Trim(strings.TrimSpace(s), "/")
		if s == "" {
			return
		}
		if _, ok := set[s]; ok {
			return
		}
		set[s] = struct{}{}
		order = append(order, s)
	}

	// Source location of the node (bottom-up): for a project its parent group,
	// for a group its own full path.
	src := n.FullPath
	if n.Kind == "project" {
		src = n.NamespacePath
	}
	add(src) // replicate the source structure as-is

	// The same source path placed under each existing top-level target account.
	tops := map[string]struct{}{}
	for _, ns := range m.namespaces {
		tops[firstSeg(ns.FullPath)] = struct{}{}
	}
	for top := range tops {
		add(top + "/" + src)
	}

	// All existing writable target namespaces.
	for _, ns := range m.namespaces {
		add(ns.FullPath)
	}
	return order
}

func firstSeg(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

// assignmentPaths returns the node ID -> chosen target namespace path map.
func (m *model) assignmentPaths() map[int64]string {
	out := make(map[int64]string, len(m.assign))
	for id, path := range m.assign {
		out[id] = path
	}
	return out
}

// --- target picker (screenTarget) ----------------------------------------

func (m *model) openTargetPicker() {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return
	}
	n := m.rows[m.cursor].node
	m.pickerNode = n.ID
	m.pickerCands = m.candidatesForNode(n)
	ti := textinput.New()
	ti.Prompt = "Ziel/Suche: "
	ti.Placeholder = "tippen zum Filtern oder freien Pfad eingeben"
	ti.CharLimit = 256
	ti.Width = 60
	ti.SetValue(m.assign[n.ID])
	ti.CursorEnd()
	ti.Focus()
	m.pickerInput = ti
	m.pickerCursor = 0
	m.screen = screenTarget
}

// pickerFiltered returns the candidates matching the current filter text.
func (m *model) pickerFiltered() []string {
	q := strings.ToLower(strings.TrimSpace(m.pickerInput.Value()))
	if q == "" {
		return m.pickerCands
	}
	var out []string
	for _, c := range m.pickerCands {
		if strings.Contains(strings.ToLower(c), q) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) updateTarget(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch k.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.screen = screenMap
		return m, nil
	case "up":
		if m.pickerCursor > 0 {
			m.pickerCursor--
		}
		return m, nil
	case "down":
		if m.pickerCursor < len(m.pickerFiltered()) {
			m.pickerCursor++
		}
		return m, nil
	case "ctrl+u": // clear assignment (inherit / none)
		delete(m.assign, m.pickerNode)
		m.screen = screenMap
		return m, nil
	case "enter":
		filtered := m.pickerFiltered()
		manual := strings.Trim(strings.TrimSpace(m.pickerInput.Value()), "/")
		// Cursor at the end selects the manual entry (free path); otherwise the
		// highlighted candidate.
		if m.pickerCursor < len(filtered) {
			m.assign[m.pickerNode] = filtered[m.pickerCursor]
		} else if manual != "" {
			m.assign[m.pickerNode] = manual
		}
		m.screen = screenMap
		return m, nil
	}
	var cmd tea.Cmd
	m.pickerInput, cmd = m.pickerInput.Update(msg)
	// Keep the cursor within the (possibly shrunk) filtered range + manual row.
	if max := len(m.pickerFiltered()); m.pickerCursor > max {
		m.pickerCursor = max
	}
	return m, cmd
}

// effectiveTargets resolves the target namespace for every selected project.
func (m *model) effectiveTargets() map[int64]string {
	all := map[int64]bool{}
	for _, r := range m.rows {
		if r.node.Kind == "project" {
			all[r.node.ID] = true
		}
	}
	targets, _ := gitlabapi.ResolveTargets(m.roots, m.assignmentPaths(), all)
	return targets
}

func descendantProjectIDs(n *gitlabapi.Node) []int64 {
	var ids []int64
	for _, c := range n.Children {
		if c.Kind == "project" {
			ids = append(ids, c.ID)
		} else {
			ids = append(ids, descendantProjectIDs(c)...)
		}
	}
	return ids
}

func (m *model) startRun() (tea.Model, tea.Cmd) {
	if len(m.namespaces) == 0 {
		m.err = errNoNamespaces
		return m, nil
	}
	targets, unmapped := gitlabapi.ResolveTargets(m.roots, m.assignmentPaths(), m.selected)
	if len(unmapped) > 0 {
		m.err = fmt.Errorf("%d selektierte(s) Projekt(e) ohne Ziel-Namespace — setze ein Ziel (auch auf Gruppen-Ebene möglich)", len(unmapped))
		return m, nil
	}
	optMap := gitlabapi.ResolveOptions(m.roots, m.optOverride, m.session.Options, m.selected)
	var items []migrate.Item
	for _, r := range m.rows {
		if r.node.Kind != "project" || !m.selected[r.node.ID] {
			continue
		}
		items = append(items, migrate.Item{
			Node:            r.node,
			TargetNamespace: targets[r.node.ID],
			Force:           m.forced[r.node.ID],
			Options:         optMap[r.node.ID],
		})
	}
	if len(items) == 0 {
		m.err = errNothingSelected
		return m, nil
	}

	// Dry run: show the resolved plan, persist nothing, touch nothing.
	if m.dryRun {
		m.dryItems = items
		m.screen = screenDryRun
		return m, nil
	}
	m.persistSession()

	workDir, _ := os.MkdirTemp("", "gitlab-copy-run-")
	m.runWorkDir = workDir
	plan := migrate.Plan{
		Source:           m.session.Source,
		Target:           m.session.Target,
		Options:          m.session.Options,
		Items:            items,
		WorkDir:          workDir,
		OldBaseURL:       m.session.Source.URL,
		NewBaseURL:       m.session.Target.URL,
		ExtraPaths:       m.session.PathMap,
		Roots:            m.roots,
		LastFingerprints: m.session.Transferred,
	}
	m.events = make(chan tea.Msg, 128)
	m.logs = nil
	m.running = true
	m.progActive = false
	m.projDone, m.projTotal = 0, 0
	m.screen = screenRun
	return m, tea.Batch(startRunCmd(plan, m.events), waitEvent(m.events), m.spin.Tick)
}

func (m *model) persistSession() {
	var sel []int64
	for id, on := range m.selected {
		if on {
			sel = append(sel, id)
		}
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i] < sel[j] })
	var forced []int64
	for id, on := range m.forced {
		if on && m.selected[id] {
			forced = append(forced, id)
		}
	}
	sort.Slice(forced, func(i, j int) bool { return forced[i] < forced[j] })
	m.session.Selected = sel
	m.session.Assignments = m.assignmentPaths()
	m.session.Force = forced
	m.session.OptionOverrides = m.optOverride
	if m.session.Name == "" {
		m.session.Name = deriveName(m.session.Source.URL, m.session.Target.URL)
	}
	_ = config.Save(&m.session, time.Now())
}

// persistPathMap merges the just-migrated repos into the session's path map so
// later runs can rewrite references to them.
func (m *model) persistPathMap() {
	if m.session.PathMap == nil {
		m.session.PathMap = map[string]string{}
	}
	for old, neu := range migrate.RecordPathMappings(m.results) {
		m.session.PathMap[old] = neu
	}
	if m.session.Transferred == nil {
		m.session.Transferred = map[int64]string{}
	}
	for id, fp := range migrate.RecordTransferred(m.results) {
		m.session.Transferred[id] = fp
	}
	_ = config.Save(&m.session, time.Now())
}

// --- run -----------------------------------------------------------------

func (m *model) updateRun(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case migrate.Event:
		m.appendEvent(msg)
		return m, waitEvent(m.events)
	case runDoneMsg:
		m.running = false
		m.results = msg.results
		m.persistPathMap()
		if m.runWorkDir != "" {
			_ = os.RemoveAll(m.runWorkDir)
			m.runWorkDir = ""
		}
		m.screen = screenDone
		return m, nil
	case tea.KeyMsg:
		if s := msg.String(); s == "ctrl+c" || s == "q" {
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.spin, cmd = m.spin.Update(msg)
	return m, cmd
}

func (m *model) appendEvent(ev migrate.Event) {
	switch ev.Type {
	case "project_start":
		m.projDone, m.projTotal = ev.Done, ev.Total
		m.progActive = false
		m.logs = append(m.logs, fmt.Sprintf("▶ [%d/%d] %s", ev.Done, ev.Total, ev.Project))
	case "log":
		m.logs = append(m.logs, "    "+ev.Message)
	case "progress":
		m.progActive = true
		if ev.Total > 0 {
			m.progFrac = float64(ev.Done) / float64(ev.Total)
		} else {
			m.progFrac = 0
		}
		m.progLabel = fmt.Sprintf("%d/%d  %s", ev.Done, ev.Total, ev.Message)
		return // don't spam the log; just update the bar
	case "project_done":
		m.progActive = false
		if ev.Result != nil {
			m.logs = append(m.logs, "  "+statusGlyph(ev.Result.Status)+" "+ev.Project+" "+resultDetail(*ev.Result))
		}
	}
	m.logView.SetContent(joinLines(m.logs))
	m.logView.GotoBottom()
}

func (m *model) updateDryRun(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "b", "esc":
			m.screen = screenMap
		}
	}
	return m, nil
}

func (m *model) updateDone(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "ctrl+c", "q", "enter":
			return m, tea.Quit
		case "b":
			m.screen = screenMap
		}
	}
	return m, nil
}
