package tui

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bresam/gitlab-copy-tool/internal/gitlabapi"
	"github.com/bresam/gitlab-copy-tool/internal/migrate"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var (
	errNoNamespaces    = errors.New("no writable target namespaces found (need Maintainer access to a group)")
	errNothingSelected = errors.New("nothing selected")

	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	cursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Bold(true)
	groupStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)
)

func newViewport(w, h int) viewport.Model {
	if w <= 0 {
		w = 80
	}
	if h <= 6 {
		h = 20
	}
	vp := viewport.New(w-2, h-8)
	return vp
}

func joinLines(lines []string) string { return strings.Join(lines, "\n") }

func statusGlyph(s migrate.Status) string {
	switch s {
	case migrate.StatusOK:
		return okStyle.Render("✓")
	case migrate.StatusWarn:
		return warnStyle.Render("!")
	case migrate.StatusSkipped:
		return dimStyle.Render("↷")
	case migrate.StatusFailed:
		return errStyle.Render("✗")
	}
	return "?"
}

func resultDetail(r migrate.ProjectResult) string {
	switch r.Status {
	case migrate.StatusSkipped:
		return dimStyle.Render("skipped: " + r.Reason)
	case migrate.StatusFailed:
		return errStyle.Render(r.Reason)
	case migrate.StatusWarn:
		return warnStyle.Render(fmt.Sprintf("%d warning(s)", len(r.Warnings)))
	}
	return dimStyle.Render("→ " + r.Target)
}

func deriveName(srcURL, tgtURL string) string {
	clean := func(s string) string {
		s = strings.TrimPrefix(s, "https://")
		s = strings.TrimPrefix(s, "http://")
		return strings.TrimRight(s, "/")
	}
	return clean(srcURL) + "→" + clean(tgtURL)
}

func (m *model) View() string {
	switch m.screen {
	case screenSession:
		return m.viewSession()
	case screenConnect:
		return m.viewConnect()
	case screenDiscover:
		return m.viewDiscover()
	case screenMap:
		return m.viewMap()
	case screenRun:
		return m.viewRun()
	case screenDone:
		return m.viewDone()
	case screenDryRun:
		return m.viewDryRun()
	}
	return ""
}

func (m *model) viewDryRun() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Dry-Run — Plan") + dimStyle.Render("   (nichts wird gepusht)") + "\n\n")
	for _, it := range m.dryItems {
		force := ""
		if it.Force {
			force = " " + warnStyle.Render("[force]")
		}
		b.WriteString(okStyle.Render("•") + " " + it.Node.FullPath +
			dimStyle.Render("  →  ") + it.TargetNamespace + "/" + it.Node.Path + force + "\n")
	}
	o := m.session.Options
	b.WriteString("\n" + dimStyle.Render(fmt.Sprintf(
		"Optionen: Issues/MRs=%v CI-Vars=%v Settings=%v URL-Rewrite=%v Releases=%v Container-Reg=%v · bekannte Pfad-Remaps: %d",
		o.Issues, o.CIVariables, o.Settings, o.URLRewrite, o.Releases, o.ContainerRegistry, len(m.session.PathMap))) + "\n\n")
	b.WriteString(dimStyle.Render("b/esc zurück zum Mapping · q beenden"))
	return b.String()
}

func (m *model) viewSession() string {
	var b strings.Builder
	title := "gitlab-copy-tool — Session wählen"
	if m.dryRun {
		title += "  " + warnStyle.Render("[DRY-RUN]")
	}
	b.WriteString(titleStyle.Render(title) + "\n\n")
	for i, name := range m.sessions {
		cursor := "  "
		if i == m.sessCursor {
			cursor = cursorStyle.Render("▸ ")
		}
		marker := ""
		switch {
		case m.tempCleared[name]:
			marker = " " + warnStyle.Render("[temporarily cleared]")
		case m.cleared[name]:
			marker = " " + warnStyle.Render("[cleared]")
		}
		b.WriteString(cursor + name + marker + "\n")
	}
	cursor := "  "
	if m.sessCursor == len(m.sessions) {
		cursor = cursorStyle.Render("▸ ")
	}
	b.WriteString(cursor + okStyle.Render("＋ Neue Session") + "\n\n")
	b.WriteString(dimStyle.Render("↑/↓ wählen · enter öffnen · c State leeren · d löschen · q beenden"))
	return b.String()
}

func (m *model) viewConnect() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Verbindungen") + "\n\n")

	render := func(fieldIdx, inputIdx int) string {
		line := m.inputs[inputIdx].View()
		if m.formField == fieldIdx {
			return cursorStyle.Render("▸ ") + line
		}
		return "  " + line
	}
	renderTransport := func(fieldIdx, tIdx int, label string) string {
		val := m.transport[tIdx]
		s := fmt.Sprintf("%s: < %s >", label, val)
		if m.formField == fieldIdx {
			return cursorStyle.Render("▸ ") + s
		}
		return "  " + dimStyle.Render(s)
	}

	b.WriteString(groupStyle.Render("Quelle (self-hosted)") + "\n")
	b.WriteString(render(0, 0) + "\n")
	b.WriteString(render(1, 1) + "\n")
	b.WriteString(renderTransport(2, 0, "Transport") + "\n\n")

	b.WriteString(groupStyle.Render("Ziel (SaaS)") + "\n")
	b.WriteString(render(3, 2) + "\n")
	b.WriteString(render(4, 3) + "\n")
	b.WriteString(renderTransport(5, 1, "Transport") + "\n\n")

	if m.testing {
		b.WriteString(m.spin.View() + " teste Verbindungen…\n")
	}
	if m.err != nil {
		b.WriteString(errStyle.Render("Fehler: "+m.err.Error()) + "\n")
	}
	b.WriteString("\n" + dimStyle.Render("tab wechseln · ←/→ Transport · ctrl+s verbinden · esc zurück"))
	return b.String()
}

func (m *model) viewDiscover() string {
	return "\n" + m.spin.View() + " lade Struktur von Quelle & Ziel…\n\n" +
		dimStyle.Render("Quelle: "+m.srcInfo+"   Ziel: "+m.tgtInfo)
}

func (m *model) viewMap() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Mapping") + dimStyle.Render("   (Quelle → Ziel-Namespace, Gruppen-Ziel vererbt sich)") + "\n\n")

	m.renderTgts = m.effectiveTargets()

	// Visible window around cursor.
	start, end := windowRange(m.cursor, len(m.rows), m.visibleRows())
	for i := start; i < end; i++ {
		b.WriteString(m.renderRow(i) + "\n")
	}
	b.WriteString("\n")
	b.WriteString(m.renderOptions() + "\n")
	if m.err != nil {
		b.WriteString(errStyle.Render(m.err.Error()) + "\n")
	}
	startHint := "ctrl+s starten"
	if m.dryRun {
		startHint = "ctrl+s " + warnStyle.Render("Plan zeigen (Dry-Run)")
	}
	b.WriteString(dimStyle.Render("↑/↓ · space wählen · ←/→ Ziel setzen (Gruppe = vererbt) · f force · a alle · N keine · 1-6 Optionen · ") + startHint + dimStyle.Render(" · esc zurück"))
	return b.String()
}

func (m *model) visibleRows() int {
	h := m.height - 12
	if h < 5 {
		h = 5
	}
	return h
}

func windowRange(cursor, total, size int) (int, int) {
	if total <= size {
		return 0, total
	}
	start := cursor - size/2
	if start < 0 {
		start = 0
	}
	end := start + size
	if end > total {
		end = total
		start = end - size
	}
	return start, end
}

func (m *model) renderRow(i int) string {
	r := m.rows[i]
	prefix := dimStyle.Render(r.prefix)
	cursor := "  "
	if i == m.cursor {
		cursor = cursorStyle.Render("▸ ")
	}

	if r.node.Kind == "group" {
		assigned := ""
		if idx, ok := m.assign[r.node.ID]; ok && idx < len(m.namespaces) {
			assigned = dimStyle.Render("  ⇒ " + m.namespaces[idx].FullPath + "/… (vererbt)")
		}
		return cursor + m.groupBox(r.node) + " " + prefix + groupStyle.Render("📁 "+r.node.Name) + assigned
	}

	box := "[ ]"
	if m.selected[r.node.ID] {
		box = okStyle.Render("[x]")
	}
	name := prefix + r.node.Name
	target := ""
	if m.selected[r.node.ID] {
		if ns, ok := m.renderTgts[r.node.ID]; ok {
			target = dimStyle.Render("  → " + ns + "/" + r.node.Path)
		} else {
			target = errStyle.Render("  → (kein Ziel)")
		}
	}
	force := ""
	if m.forced[r.node.ID] {
		force = " " + warnStyle.Render("[force]")
	}
	done := ""
	if _, ok := m.session.PathMap[r.node.FullPath]; ok {
		done = " " + okStyle.Render("✓ übertragen")
	}
	return cursor + box + " " + name + target + force + done
}

// groupBox renders a tri-state checkbox for a group based on how many of its
// descendant projects are selected.
func (m *model) groupBox(n *gitlabapi.Node) string {
	ids := descendantProjectIDs(n)
	if len(ids) == 0 {
		return dimStyle.Render("[ ]")
	}
	var on int
	for _, id := range ids {
		if m.selected[id] {
			on++
		}
	}
	switch {
	case on == 0:
		return "[ ]"
	case on == len(ids):
		return okStyle.Render("[x]")
	default:
		return warnStyle.Render("[~]")
	}
}

func (m *model) renderOptions() string {
	o := m.session.Options
	mark := func(b bool) string {
		if b {
			return okStyle.Render("[x]")
		}
		return "[ ]"
	}
	return "Optionen (failsafe):  " +
		"1 " + mark(o.Issues) + " Issues/MRs/Labels/Milestones   " +
		"2 " + mark(o.CIVariables) + " CI-Vars   " +
		"3 " + mark(o.Settings) + " Settings   " +
		"4 " + mark(o.URLRewrite) + " URL-Rewrite   " +
		"5 " + mark(o.Releases) + " Releases   " +
		"6 " + mark(o.ContainerRegistry) + " Container-Registry (skopeo)"
}

func (m *model) viewRun() string {
	var b strings.Builder
	head := titleStyle.Render("Migration läuft")
	if !m.running {
		head = titleStyle.Render("Migration")
	}
	b.WriteString(head + "\n")
	if m.running {
		b.WriteString(m.spin.View() + " arbeite…\n")
	}
	b.WriteString(m.logView.View() + "\n")
	b.WriteString(dimStyle.Render("ctrl+c abbrechen"))
	return b.String()
}

func (m *model) viewDone() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Fertig") + "  " + migrate.Summary(m.results) + "\n\n")
	for _, r := range m.results {
		b.WriteString(statusGlyph(r.Status) + " " + r.Source + " " + resultDetail(r) + "\n")
		for _, w := range r.Warnings {
			b.WriteString("      " + warnStyle.Render("• "+w) + "\n")
		}
	}
	b.WriteString("\n" + dimStyle.Render("q beenden · b zurück zum Mapping"))
	return b.String()
}
