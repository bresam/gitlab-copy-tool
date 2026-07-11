// Package tui implements the interactive terminal UI: session picker,
// connection form, structure discovery, the mapping screen (checkbox tree +
// target namespace selection), option toggles and the live run view.
package tui

import (
	"context"
	"fmt"

	"github.com/bresam/gitlab-copy-tool/internal/config"
	"github.com/bresam/gitlab-copy-tool/internal/gitlabapi"
	"github.com/bresam/gitlab-copy-tool/internal/migrate"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

type screen int

const (
	screenSession screen = iota
	screenConnect
	screenDiscover
	screenMap
	screenRun
	screenDone
	screenDryRun
)

type model struct {
	screen screen
	width  int
	height int
	err    error
	dryRun bool

	// session picker
	sessions    []string
	sessCursor  int
	cleared     map[string]bool // sessions cleared + persisted this run
	tempCleared map[string]bool // sessions cleared in-memory only (dry-run)

	// connection form
	session   config.Session
	inputs    []textinput.Model
	transport [2]string // [0]=source, [1]=target
	formField int
	testing   bool
	srcInfo   string
	tgtInfo   string

	// discovery
	spin        spinner.Model
	discovering bool
	roots       []*gitlabapi.Node
	rows        []treeRow
	namespaces  []gitlabapi.Namespace
	selected    map[int64]bool   // project ID -> processed
	assign      map[int64]int    // node ID (group/project) -> namespace index
	forced      map[int64]bool   // project ID -> force overwrite target
	renderTgts  map[int64]string // transient: resolved targets for rendering
	cursor      int

	// run
	events   chan tea.Msg
	logs     []string
	logView  viewport.Model
	results  []migrate.ProjectResult
	running  bool
	dryItems []migrate.Item // resolved plan shown in dry-run mode
}

type treeRow struct {
	node   *gitlabapi.Node
	depth  int
	prefix string // tree connector lines (├─ └─ │) preceding the label
}

// Run starts the interactive program. When dryRun is true the migration is not
// executed; the resolved plan is shown instead.
func Run(dryRun bool) error {
	m := newModel()
	m.dryRun = dryRun
	p := tea.NewProgram(&m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func newModel() model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot

	sessions, _ := config.List()

	m := model{
		screen:      screenSession,
		sessions:    sessions,
		spin:        sp,
		cleared:     map[string]bool{},
		tempCleared: map[string]bool{},
		selected:    map[int64]bool{},
		assign:      map[int64]int{},
		forced:      map[int64]bool{},
		transport:   [2]string{config.TransportAuto, config.TransportAuto},
		session: config.Session{
			Source:      config.Endpoint{Transport: config.TransportAuto},
			Target:      config.Endpoint{Transport: config.TransportAuto},
			Assignments: map[int64]string{},
			Options:     config.Options{Issues: true, CIVariables: true, Settings: true, URLRewrite: true, Releases: true},
		},
	}
	m.buildInputs()
	return m
}

func (m *model) buildInputs() {
	// Fields: 0 srcURL, 1 srcToken, 2 tgtURL, 3 tgtToken, 4 rewriteName? keep 4.
	labels := []string{"Source URL", "Source token", "Target URL", "Target token"}
	placeholders := []string{
		"https://gitlab.internal.example.com",
		"glpat-… or ${ENV_VAR}",
		"https://gitlab.com",
		"glpat-… or ${ENV_VAR}",
	}
	vals := []string{m.session.Source.URL, m.session.Source.Token, m.session.Target.URL, m.session.Target.Token}
	m.inputs = make([]textinput.Model, len(labels))
	for i := range labels {
		ti := textinput.New()
		ti.Placeholder = placeholders[i]
		ti.Prompt = labels[i] + ": "
		ti.SetValue(vals[i])
		ti.CharLimit = 512
		ti.Width = 60
		if i == 1 || i == 3 {
			ti.EchoMode = textinput.EchoPassword
		}
		m.inputs[i] = ti
	}
	m.transport = [2]string{
		orAuto(m.session.Source.Transport),
		orAuto(m.session.Target.Transport),
	}
	if len(m.inputs) > 0 {
		m.inputs[0].Focus()
	}
	m.formField = 0
}

func orAuto(s string) string {
	if s == "" {
		return config.TransportAuto
	}
	return s
}

func (m *model) Init() tea.Cmd { return m.spin.Tick }

// --- async commands ------------------------------------------------------

type connectedMsg struct {
	src, tgt *gitlabapi.Client
	srcInfo  string
	tgtInfo  string
	err      error
}

type discoveredMsg struct {
	roots      []*gitlabapi.Node
	namespaces []gitlabapi.Namespace
	err        error
}

type runDoneMsg struct{ results []migrate.ProjectResult }

var (
	srcClient *gitlabapi.Client
	tgtClient *gitlabapi.Client
)

func connectCmd(src, tgt config.Endpoint) tea.Cmd {
	return func() tea.Msg {
		sc, err := gitlabapi.New(src)
		if err != nil {
			return connectedMsg{err: fmt.Errorf("source: %w", err)}
		}
		sl, sv, err := sc.Ping()
		if err != nil {
			return connectedMsg{err: fmt.Errorf("source ping: %w", err)}
		}
		tc, err := gitlabapi.New(tgt)
		if err != nil {
			return connectedMsg{err: fmt.Errorf("target: %w", err)}
		}
		tl, tv, err := tc.Ping()
		if err != nil {
			return connectedMsg{err: fmt.Errorf("target ping: %w", err)}
		}
		return connectedMsg{
			src: sc, tgt: tc,
			srcInfo: fmt.Sprintf("%s @ GitLab %s", sl, sv),
			tgtInfo: fmt.Sprintf("%s @ GitLab %s", tl, tv),
		}
	}
}

func discoverCmd(src, tgt *gitlabapi.Client) tea.Cmd {
	return func() tea.Msg {
		roots, err := src.SourceTree()
		if err != nil {
			return discoveredMsg{err: fmt.Errorf("discover source: %w", err)}
		}
		ns, err := tgt.TargetNamespaces()
		if err != nil {
			return discoveredMsg{err: fmt.Errorf("discover target namespaces: %w", err)}
		}
		return discoveredMsg{roots: roots, namespaces: ns}
	}
}

// waitEvent reads one streamed run event from the channel.
func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func startRunCmd(plan migrate.Plan, ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		eng, err := migrate.NewEngine(plan)
		if err != nil {
			return runDoneMsg{results: []migrate.ProjectResult{{Status: migrate.StatusFailed, Reason: err.Error()}}}
		}
		results := eng.Run(context.Background(), plan, func(ev migrate.Event) {
			ch <- ev
		})
		return runDoneMsg{results: results}
	}
}
