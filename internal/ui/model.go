package ui

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	awsclient "github.com/evelez/ecsconnect/internal/aws"
	"github.com/evelez/ecsconnect/internal/logger"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── State machine ─────────────────────────────────────────────────────────────

type appState int

const (
	stateInit appState = iota
	stateProfileSelect
	stateDetectingRegion
	stateRegionSelect
	stateLoadingClusters
	stateClusterList
	stateLoadingTasks
	stateTaskList
	stateActionMenu
	stateContainerSelect
	stateLoadingLogs
	stateLogView
	stateDetailView
	stateError
)

type pendingAction int

const (
	actionNone pendingAction = iota
	actionShell
	actionLogs
)

// ── Key bindings ──────────────────────────────────────────────────────────────

var (
	keyChangeRegion = key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "change region"),
	)
	keyRefresh = key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "refresh"),
	)
	keyHome = key.NewBinding(
		key.WithKeys("ctrl+g"),
		key.WithHelp("ctrl+g", "go home"),
	)
)

// ── Messages ──────────────────────────────────────────────────────────────────

type profilesReadyMsg []string
type regionDetectedMsg struct{ region string }
type clientReadyMsg struct {
	client *awsclient.Client
	err    error
}
type clustersReadyMsg struct {
	clusters []awsclient.Cluster
	err      error
}
type tasksReadyMsg struct {
	tasks []awsclient.Task
	err   error
}
type logsReadyMsg struct {
	lines     []string
	stream    awsclient.LogStream
	nextToken string
	err       error
}
type logsPollMsg struct {
	lines     []string
	nextToken string
	err       error
}
type logTickMsg time.Time
type shellExitMsg struct{ err error }

// ── List item ─────────────────────────────────────────────────────────────────

type item struct {
	title string
	desc  string
}

func (i item) Title() string       { return i.title }
func (i item) Description() string { return i.desc }
func (i item) FilterValue() string { return i.title }

// ── Model ─────────────────────────────────────────────────────────────────────

type Model struct {
	state  appState
	width  int
	height int

	profiles        []string
	selectedProfile string
	selectedRegion  string
	client          *awsclient.Client

	clusters          []awsclient.Cluster
	tasks             []awsclient.Task
	selectedCluster   awsclient.Cluster
	selectedTask      awsclient.Task
	selectedContainer awsclient.Container
	pendingAct        pendingAction

	list     list.Model
	spinner  spinner.Model
	viewport viewport.Model

	viewportContent string
	logStream       awsclient.LogStream
	logToken        string
	err             error
}

// New creates the initial model.
func New() Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = spinnerStyle

	return Model{
		state:   stateInit,
		list:    list.New(nil, list.NewDefaultDelegate(), 0, 0),
		spinner: s,
	}
}

// newList creates a fresh list for each navigation step — no stale filter state.
func (m Model) newList(title string, items []list.Item, extraKeys ...key.Binding) list.Model {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = delegate.Styles.SelectedTitle.
		Foreground(white).
		BorderLeftForeground(purple)
	delegate.Styles.SelectedDesc = delegate.Styles.SelectedDesc.
		Foreground(lightGray).
		BorderLeftForeground(purple)

	// Reserve lines: 2 banner + 1 footer hints + 1 margin
	h := m.height - 7
	if h < 1 {
		h = 14
	}
	l := list.New(items, delegate, m.width, h)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(true)
	l.SetShowHelp(false) // we render our own help footer
	l.Styles.Title = titleStyle
	l.Title = title

	return l
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg {
		profiles := awsclient.LoadProfiles()
		logger.Debug("profiles loaded", "count", len(profiles), "profiles", profiles)
		return profilesReadyMsg(profiles)
	}
}

// ── Update ────────────────────────────────────────────────────────────────────

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.list.SetSize(msg.Width, msg.Height-4)
		m.viewport = viewport.New(msg.Width, msg.Height-5)
		if m.viewportContent != "" {
			m.viewport.SetContent(m.viewportContent)
		}
		return m, nil

	case tea.KeyMsg:
		if m.list.FilterState() == list.Filtering {
			var cmd tea.Cmd
			m.list, cmd = m.list.Update(msg)
			// Enter while filtering confirms the filter and selects immediately —
			// no second Enter required.
			if msg.String() == "enter" {
				return m.handleSelect()
			}
			return m, cmd
		}

		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit

		case "q":
			if m.state == stateClusterList || m.state == stateProfileSelect ||
				m.state == stateRegionSelect || m.state == stateError {
				return m, tea.Quit
			}

		case "esc":
			// If a filter is applied, clear it first; a second esc navigates back.
			if isListState(m.state) && m.list.FilterState() == list.FilterApplied {
				var cmd tea.Cmd
				m.list, cmd = m.list.Update(msg)
				return m, cmd
			}
			return m.goBack()

		case "backspace":
			return m.goBack()

		case "ctrl+g":
			if m.state != stateProfileSelect && m.state != stateRegionSelect {
				return m.goHome()
			}

		case "c":
			if m.state == stateClusterList || m.state == stateTaskList {
				logger.Info("user requested region change", "current_region", m.selectedRegion)
				m.clusters = nil
				m.tasks = nil
				return m.setRegionList()
			}

		case "r":
			return m.refresh()

		case "enter", " ":
			return m.handleSelect()
		}

	// ── AWS messages ──────────────────────────────────────────────────────────

	case profilesReadyMsg:
		m.profiles = []string(msg)
		if len(m.profiles) == 1 {
			m.selectedProfile = m.profiles[0]
			logger.Info("single profile, skipping selection", "profile", m.selectedProfile)
			return m.detectRegion()
		}
		logger.Debug("multiple profiles found", "count", len(m.profiles))
		m.state = stateProfileSelect
		m.list.Title = "AWS Profile"
		items := make([]list.Item, len(m.profiles))
		for i, p := range m.profiles {
			items[i] = item{title: p, desc: "AWS profile"}
		}
		m.list.SetItems(items)
		return m, nil

	case regionDetectedMsg:
		logger.Debug("region detected", "profile", m.selectedProfile, "region", msg.region)
		m.selectedRegion = msg.region
		return m.setRegionList()

	case clientReadyMsg:
		if msg.err != nil {
			logger.Error("failed to create AWS client",
				"profile", m.selectedProfile,
				"region", m.selectedRegion,
				"error", msg.err)
			return m.setError(msg.err)
		}
		logger.Info("AWS client ready", "profile", m.selectedProfile, "region", m.selectedRegion)
		m.client = msg.client
		m.state = stateLoadingClusters
		return m, tea.Batch(m.spinner.Tick, m.cmdLoadClusters())

	case clustersReadyMsg:
		if msg.err != nil {
			logger.Error("failed to list clusters",
				"profile", m.selectedProfile,
				"region", m.selectedRegion,
				"error", msg.err)
			return m.setError(msg.err)
		}
		logger.Info("clusters loaded", "count", len(msg.clusters), "region", m.selectedRegion)
		m.clusters = msg.clusters
		return m.setClusterList()

	case tasksReadyMsg:
		if msg.err != nil {
			logger.Error("failed to list tasks",
				"cluster", m.selectedCluster.Name,
				"error", msg.err)
			return m.setError(msg.err)
		}
		logger.Info("tasks loaded", "cluster", m.selectedCluster.Name, "count", len(msg.tasks))
		m.tasks = msg.tasks
		return m.setTaskList()

	case logsReadyMsg:
		if msg.err != nil {
			logger.Error("failed to fetch logs",
				"task", m.selectedTask.ShortID,
				"container", m.selectedContainer.Name,
				"error", msg.err)
			return m.setError(msg.err)
		}
		logger.Info("logs fetched", "lines", len(msg.lines),
			"task", m.selectedTask.ShortID, "container", m.selectedContainer.Name)
		m.state = stateLogView
		m.logStream = msg.stream
		m.logToken = msg.nextToken
		m.viewportContent = m.buildLogContent(msg.lines)
		m.viewport.SetContent(m.viewportContent)
		m.viewport.GotoBottom()
		return m, logTick()

	case logsPollMsg:
		if msg.err != nil {
			logger.Warn("log poll error", "error", msg.err)
			// don't error out — just retry on next tick
			return m, logTick()
		}
		if len(msg.lines) > 0 {
			logger.Debug("log poll: new events", "count", len(msg.lines))
			m.logToken = msg.nextToken
			m.viewportContent = m.viewportContent + "\n" + strings.Join(msg.lines, "\n")
			m.viewport.SetContent(m.viewportContent)
			m.viewport.GotoBottom()
		}
		if m.state == stateLogView {
			return m, logTick()
		}
		return m, nil

	case logTickMsg:
		if m.state == stateLogView {
			return m, m.cmdPollLogs()
		}
		return m, nil

	case shellExitMsg:
		if msg.err != nil {
			logger.Error("shell session ended with error",
				"task", m.selectedTask.ShortID,
				"container", m.selectedContainer.Name,
				"error", msg.err)
		} else {
			logger.Info("shell session ended",
				"task", m.selectedTask.ShortID,
				"container", m.selectedContainer.Name)
		}
		return m.setActionMenu()

	// ── Spinner ───────────────────────────────────────────────────────────────

	case spinner.TickMsg:
		if isLoading(m.state) {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	// Delegate to active component
	if isListState(m.state) {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}
	if isViewportState(m.state) {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

// ── View ──────────────────────────────────────────────────────────────────────

func (m Model) View() string {
	switch {
	case isLoading(m.state):
		return m.viewLoading()
	case m.state == stateError:
		return m.viewError()
	case isListState(m.state):
		return banner() + m.list.View() + "\n" + m.viewListHelp()
	case isViewportState(m.state):
		return m.viewViewport()
	}
	return ""
}

// banner returns the persistent app title shown on every screen.
func banner() string {
	return lipgloss.NewStyle().
		Foreground(orange).
		Bold(true).
		Padding(0, 1).
		Render("ECSConnect") +
		lipgloss.NewStyle().Foreground(lightGray).Render(" — By v0id0100") + "\n\n"
}

func (m Model) viewListHelp() string {
	var parts []string

	// always-present
	parts = append(parts, "↑/↓ navigate", "enter select", "/ filter", "esc back")

	switch m.state {
	case stateClusterList, stateTaskList:
		parts = append(parts, "c region", "r refresh", "ctrl+g home", "q quit")
	case stateActionMenu, stateContainerSelect:
		parts = append(parts, "ctrl+g home", "q quit")
	case stateProfileSelect, stateRegionSelect:
		parts = append(parts, "q quit")
	}

	return hintStyle.Render(strings.Join(parts, "  •  "))
}

func (m Model) viewLoading() string {
	var label string
	switch m.state {
	case stateDetectingRegion:
		label = "Detecting region for " + m.selectedProfile
	case stateLoadingClusters:
		label = fmt.Sprintf("Loading clusters  [%s / %s]", m.selectedProfile, m.selectedRegion)
	case stateLoadingTasks:
		label = fmt.Sprintf("Loading tasks for %s", m.selectedCluster.Name)
	case stateLoadingLogs:
		label = fmt.Sprintf("Fetching logs for %s", m.selectedContainer.Name)
	default:
		label = "Loading"
	}
	return banner() + "\n  " + spinnerStyle.Render(m.spinner.View()) + "  " + loadingStyle.Render(label+"...")
}

func (m Model) viewError() string {
	errMsg := "unknown error"
	if m.err != nil {
		errMsg = m.err.Error()
	}

	logHint := ""
	if p := logger.Path(); p != "" {
		logHint = "\n\n  " + hintStyle.Render("log: "+p)
	}

	return banner() +
		"\n  " + errorStyle.Render("Error: ") + errMsg +
		logHint +
		"\n\n  " + hintStyle.Render("esc back  •  ctrl+g home  •  q quit")
}

func (m Model) viewViewport() string {
	keys := hintStyle.Render("↑/↓ scroll • esc back • ctrl+g home • q quit")
	return m.viewport.View() + "\n" + keys
}

// ── Navigation ────────────────────────────────────────────────────────────────

func (m Model) goHome() (Model, tea.Cmd) {
	logger.Info("go home", "from", m.state, "profile", m.selectedProfile)
	m.selectedCluster = awsclient.Cluster{}
	m.selectedTask = awsclient.Task{}
	m.selectedContainer = awsclient.Container{}
	m.clusters = nil
	m.tasks = nil
	m.client = nil
	m.err = nil

	if len(m.profiles) > 1 {
		return m.setProfileList()
	}
	return m.setRegionList()
}

func (m Model) goBack() (Model, tea.Cmd) {
	logger.Debug("navigate back", "from", m.state)
	switch m.state {
	case stateProfileSelect:
		return m, tea.Quit
	case stateRegionSelect:
		if len(m.profiles) > 1 {
			return m.setProfileList()
		}
		return m, tea.Quit
	case stateClusterList:
		return m.setRegionList()
	case stateTaskList:
		return m.setClusterList()
	case stateActionMenu:
		return m.setTaskList()
	case stateContainerSelect:
		return m.setActionMenu()
	case stateLogView, stateDetailView:
		return m.setActionMenu()
	case stateError:
		if len(m.tasks) > 0 {
			return m.setTaskList()
		}
		if len(m.clusters) > 0 {
			return m.setClusterList()
		}
		return m.setRegionList()
	}
	return m, nil
}

func (m Model) refresh() (Model, tea.Cmd) {
	switch m.state {
	case stateClusterList:
		logger.Info("refreshing clusters", "region", m.selectedRegion)
		m.state = stateLoadingClusters
		return m, tea.Batch(m.spinner.Tick, m.cmdLoadClusters())
	case stateTaskList:
		logger.Info("refreshing tasks", "cluster", m.selectedCluster.Name)
		m.state = stateLoadingTasks
		return m, tea.Batch(m.spinner.Tick, m.cmdLoadTasks())
	}
	return m, nil
}

func (m Model) handleSelect() (Model, tea.Cmd) {
	selected, ok := m.list.SelectedItem().(item)
	if !ok {
		return m, nil
	}

	switch m.state {
	case stateProfileSelect:
		m.selectedProfile = selected.title
		logger.Info("profile selected", "profile", m.selectedProfile)
		return m.detectRegion()

	case stateRegionSelect:
		m.selectedRegion = selected.title
		logger.Info("region selected", "region", m.selectedRegion, "profile", m.selectedProfile)
		m.state = stateLoadingClusters
		return m, tea.Batch(m.spinner.Tick, m.cmdCreateClient())

	case stateClusterList:
		for _, cl := range m.clusters {
			if cl.Name == selected.title {
				m.selectedCluster = cl
				break
			}
		}
		logger.Info("cluster selected", "cluster", m.selectedCluster.Name)
		m.state = stateLoadingTasks
		return m, tea.Batch(m.spinner.Tick, m.cmdLoadTasks())

	case stateTaskList:
		for _, t := range m.tasks {
			if t.ShortID == selected.title || strings.HasPrefix(t.ShortID, selected.title) {
				m.selectedTask = t
				break
			}
		}
		logger.Info("task selected", "task", m.selectedTask.ShortID, "status", m.selectedTask.Status)
		return m.setActionMenu()

	case stateActionMenu:
		switch selected.title {
		case "Shell":
			m.pendingAct = actionShell
			return m.resolveContainerAction()
		case "Logs":
			m.pendingAct = actionLogs
			return m.resolveContainerAction()
		case "Details":
			return m.setDetailView()
		}

	case stateContainerSelect:
		for _, c := range m.selectedTask.Containers {
			if c.Name == selected.title {
				m.selectedContainer = c
				break
			}
		}
		logger.Info("container selected", "container", m.selectedContainer.Name)
		return m.performPendingAction()
	}

	return m, nil
}

func (m Model) detectRegion() (Model, tea.Cmd) {
	m.state = stateDetectingRegion
	profile := m.selectedProfile
	return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		region := awsclient.DetectRegion(ctx, profile)
		return regionDetectedMsg{region: region}
	})
}

func (m Model) resolveContainerAction() (Model, tea.Cmd) {
	if len(m.selectedTask.Containers) == 1 {
		m.selectedContainer = m.selectedTask.Containers[0]
		return m.performPendingAction()
	}
	return m.setContainerSelect()
}

func (m Model) performPendingAction() (Model, tea.Cmd) {
	switch m.pendingAct {
	case actionShell:
		return m.execShell()
	case actionLogs:
		m.state = stateLoadingLogs
		return m, tea.Batch(m.spinner.Tick, m.cmdLoadLogs())
	}
	return m, nil
}

func (m Model) execShell() (Model, tea.Cmd) {
	taskID := m.selectedTask.ARN
	if idx := strings.LastIndex(taskID, "/"); idx >= 0 {
		taskID = taskID[idx+1:]
	}

	args := []string{
		"ecs", "execute-command",
		"--cluster", m.selectedCluster.Name,
		"--task", taskID,
		"--container", m.selectedContainer.Name,
		"--interactive",
		"--command", "/bin/sh",
		"--region", m.selectedRegion,
	}
	if m.selectedProfile != "" && m.selectedProfile != "default" {
		args = append(args, "--profile", m.selectedProfile)
	}

	logger.Info("starting shell session",
		"cluster", m.selectedCluster.Name,
		"task", taskID,
		"container", m.selectedContainer.Name,
		"region", m.selectedRegion,
	)

	cmd := exec.Command("aws", args...)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return shellExitMsg{err: err}
	})
}

// ── List builders ─────────────────────────────────────────────────────────────

func (m Model) setProfileList() (Model, tea.Cmd) {
	m.state = stateProfileSelect
	items := make([]list.Item, len(m.profiles))
	for i, p := range m.profiles {
		items[i] = item{title: p, desc: "AWS profile"}
	}
	m.list = m.newList("AWS Profile", items)
	return m, nil
}

func (m Model) setRegionList() (Model, tea.Cmd) {
	m.state = stateRegionSelect
	regions := awsclient.CommonRegions()
	items := make([]list.Item, len(regions))
	for i, r := range regions {
		desc := ""
		if r == m.selectedRegion {
			desc = "← configured in profile"
		}
		items[i] = item{title: r, desc: desc}
	}
	m.list = m.newList(fmt.Sprintf("Region  [profile: %s]", m.selectedProfile), items)
	for i, r := range regions {
		if r == m.selectedRegion {
			m.list.Select(i)
			break
		}
	}
	return m, nil
}

func (m Model) setClusterList() (Model, tea.Cmd) {
	m.state = stateClusterList
	items := make([]list.Item, len(m.clusters))
	for i, c := range m.clusters {
		desc := fmt.Sprintf("%s  •  Running: %d  •  Pending: %d",
			statusColor(c.Status), c.RunningTasks, c.PendingTasks)
		items[i] = item{title: c.Name, desc: desc}
	}
	if len(m.clusters) == 0 {
		items = []list.Item{item{title: "(no clusters found)", desc: "press c to try a different region"}}
	}
	m.list = m.newList(
		fmt.Sprintf("Clusters  [%s / %s]", m.selectedProfile, m.selectedRegion),
		items,
		keyChangeRegion, keyRefresh,
	)
	return m, nil
}

func (m Model) setTaskList() (Model, tea.Cmd) {
	m.state = stateTaskList
	items := make([]list.Item, len(m.tasks))
	for i, t := range m.tasks {
		var started string
		if t.StartedAt != nil {
			started = t.StartedAt.Format("Jan 02 15:04")
		} else {
			started = "—"
		}
		containers := make([]string, len(t.Containers))
		for j, c := range t.Containers {
			containers[j] = c.Name
		}
		desc := fmt.Sprintf("%s  •  %s  •  Started: %s  •  [%s]",
			statusColor(t.Status),
			t.TaskDefinition,
			started,
			strings.Join(containers, ", "),
		)
		items[i] = item{title: t.ShortID, desc: desc}
	}
	if len(m.tasks) == 0 {
		items = []list.Item{item{title: "(no tasks running)", desc: ""}}
	}
	m.list = m.newList(
		fmt.Sprintf("Tasks — %s", m.selectedCluster.Name),
		items,
		keyChangeRegion, keyRefresh, keyHome,
	)
	return m, nil
}

func (m Model) setActionMenu() (Model, tea.Cmd) {
	m.state = stateActionMenu
	shortID := m.selectedTask.ShortID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	m.list = m.newList(
		fmt.Sprintf("Actions — %s", shortID),
		[]list.Item{
			item{title: "Shell", desc: "Open interactive shell in container"},
			item{title: "Logs", desc: "View CloudWatch logs"},
			item{title: "Details", desc: "Show task details"},
		},
		keyHome,
	)
	return m, nil
}

func (m Model) setContainerSelect() (Model, tea.Cmd) {
	m.state = stateContainerSelect
	items := make([]list.Item, len(m.selectedTask.Containers))
	for i, c := range m.selectedTask.Containers {
		items[i] = item{title: c.Name, desc: fmt.Sprintf("%s  •  %s", statusColor(c.Status), c.Image)}
	}
	m.list = m.newList("Select container", items, keyHome)
	return m, nil
}

func (m Model) setDetailView() (Model, tea.Cmd) {
	m.state = stateDetailView
	t := m.selectedTask

	var sb strings.Builder
	sb.WriteString(breadcrumbStyle.Render(m.breadcrumb()) + "\n")
	sb.WriteString(titleStyle.Render("Task Details") + "\n\n")

	field := func(label, value string) string {
		return lipgloss.NewStyle().Bold(true).Foreground(purple).Render(label+": ") + value + "\n"
	}

	sb.WriteString(field("Task ID", t.ARN))
	sb.WriteString(field("Status", statusColor(t.Status)))
	sb.WriteString(field("Task Definition", t.TaskDefinition))
	sb.WriteString(field("Launch Type", t.LaunchType))
	sb.WriteString(field("CPU", t.CPU))
	sb.WriteString(field("Memory", t.Memory+" MiB"))
	if t.StartedAt != nil {
		sb.WriteString(field("Started At", t.StartedAt.Format(time.RFC1123)))
	}

	sb.WriteString("\n" + lipgloss.NewStyle().Bold(true).Foreground(purple).Render("Containers:") + "\n")
	for _, c := range t.Containers {
		sb.WriteString(fmt.Sprintf("  • %s  %s  %s\n",
			lipgloss.NewStyle().Bold(true).Render(c.Name),
			statusColor(c.Status),
			loadingStyle.Render(c.Image),
		))
	}

	m.viewportContent = sb.String()
	m.viewport.SetContent(m.viewportContent)
	m.viewport.GotoTop()
	return m, nil
}

func (m Model) setError(err error) (Model, tea.Cmd) {
	m.state = stateError
	m.err = err
	return m, nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (m Model) cmdCreateClient() tea.Cmd {
	profile := m.selectedProfile
	region := m.selectedRegion
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		logger.Debug("creating AWS client", "profile", profile, "region", region)
		client, err := awsclient.NewClient(ctx, profile, region)
		return clientReadyMsg{client: client, err: err}
	}
}

func (m Model) cmdLoadClusters() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		logger.Debug("listing clusters", "region", m.selectedRegion)
		clusters, err := m.client.ListClusters(ctx)
		return clustersReadyMsg{clusters: clusters, err: err}
	}
}

func (m Model) cmdLoadTasks() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		logger.Debug("listing tasks", "cluster", m.selectedCluster.Name)
		tasks, err := m.client.ListTasks(ctx, m.selectedCluster.Name)
		return tasksReadyMsg{tasks: tasks, err: err}
	}
}

func (m Model) cmdLoadLogs() tea.Cmd {
	task := m.selectedTask
	container := m.selectedContainer
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		logger.Debug("fetching logs", "task", task.ShortID, "container", container.Name, "taskDef", task.TaskDefARN)
		lines, stream, token, err := client.GetContainerLogs(ctx, task.TaskDefARN, task.ARN, container.Name, 200)
		return logsReadyMsg{lines: lines, stream: stream, nextToken: token, err: err}
	}
}

func (m Model) cmdPollLogs() tea.Cmd {
	client := m.client
	stream := m.logStream
	token := m.logToken
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		lines, nextToken, err := client.PollLogs(ctx, stream, token, 100)
		return logsPollMsg{lines: lines, nextToken: nextToken, err: err}
	}
}

func logTick() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg {
		return logTickMsg(t)
	})
}

func (m Model) buildLogContent(lines []string) string {
	shortID := m.selectedTask.ShortID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	header := breadcrumbStyle.Render(m.breadcrumb()) + "\n" +
		titleStyle.Render(fmt.Sprintf("Logs — %s / %s", shortID, m.selectedContainer.Name)) +
		lipgloss.NewStyle().Foreground(green).Render("  ● LIVE") + "\n\n"
	if len(lines) == 0 {
		return header + loadingStyle.Render("(waiting for log events...)")
	}
	return header + strings.Join(lines, "\n")
}

// ── Utility ───────────────────────────────────────────────────────────────────

func (m Model) breadcrumb() string {
	parts := []string{m.selectedProfile, m.selectedRegion}
	if m.selectedCluster.Name != "" {
		parts = append(parts, m.selectedCluster.Name)
	}
	if m.selectedTask.ShortID != "" {
		id := m.selectedTask.ShortID
		if len(id) > 8 {
			id = id[:8]
		}
		parts = append(parts, id)
	}
	if m.selectedContainer.Name != "" {
		parts = append(parts, m.selectedContainer.Name)
	}
	return strings.Join(parts, " › ")
}

func isLoading(s appState) bool {
	return s == stateInit || s == stateDetectingRegion ||
		s == stateLoadingClusters || s == stateLoadingTasks || s == stateLoadingLogs
}

func isListState(s appState) bool {
	return s == stateProfileSelect || s == stateRegionSelect || s == stateClusterList ||
		s == stateTaskList || s == stateActionMenu || s == stateContainerSelect
}

func isViewportState(s appState) bool {
	return s == stateLogView || s == stateDetailView
}
