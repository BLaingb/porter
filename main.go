package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	refreshInterval = 3 * time.Second
	sigkillDelay    = 2 * time.Second

	colPort      = 7
	colPID       = 9
	colName      = 15
	colRepo      = 16
	colTree      = 24
	colContainer = 14
)

type Process struct {
	Port            int
	PID             int
	Name            string
	Command         string
	Repo            string
	Worktree        string
	DockerContainer string // non-empty if this port is published by a Docker container
}

type viewMode int

const (
	modeNormal  viewMode = iota
	modeFilter
	modeConfirm
)

type model struct {
	processes    []Process
	displayed    []Process
	cursor       int
	scrollOffset int
	mode         viewMode
	filterInput  string
	statusMsg    string
	statusIsErr  bool
	width        int
	height       int
	lastRefresh  time.Time
	hiddenNames  map[string]bool
	showHidden   bool
}

// messages
type tickMsg struct{}
type processesMsg []Process
type killDoneMsg struct {
	pid       int
	container string
	err       error
}
type openDoneMsg struct {
	url string
	err error
}

// styles
var (
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#5C5CFF"))

	colHeaderStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888")).
			Bold(true)

	selectedStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("#3C3C6E")).
			Foreground(lipgloss.Color("#FFFFFF"))

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC"))

	portStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#5C5CFF")).
			Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#555555"))

	hiddenRowStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#444444")).
			Italic(true)

	confirmStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFB86C")).
			Bold(true)

	filterStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#50FA7B"))

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF5555"))

	successStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#50FA7B"))
)

// configPath returns the path to the hidden-names config file.
func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".porter", "hidden.json")
}

func loadHidden() map[string]bool {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return make(map[string]bool)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return make(map[string]bool)
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func saveHidden(hidden map[string]bool) {
	names := make([]string, 0, len(hidden))
	for n := range hidden {
		names = append(names, n)
	}
	sort.Strings(names)
	data, _ := json.Marshal(names)
	path := configPath()
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	_ = os.WriteFile(path, data, 0644)
}

func main() {
	p := tea.NewProgram(newModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newModel() model {
	return model{
		displayed:   []Process{},
		lastRefresh: time.Now(),
		hiddenNames: loadHidden(),
	}
}

func batchGetCwds(pids []int) map[int]string {
	if len(pids) == 0 {
		return nil
	}
	pidList := make([]string, len(pids))
	for i, p := range pids {
		pidList[i] = strconv.Itoa(p)
	}
	out, err := exec.Command("lsof", "-a", "-p", strings.Join(pidList, ","), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return nil
	}
	cwds := make(map[int]string)
	var curPID int
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") {
			pid, err := strconv.Atoi(line[1:])
			if err == nil {
				curPID = pid
			}
		} else if strings.HasPrefix(line, "n") && curPID != 0 {
			cwds[curPID] = line[1:]
			curPID = 0
		}
	}
	return cwds
}

type gitCtx struct {
	repo     string
	worktree string
}

func gitContextForCwd(cwd string) gitCtx {
	if cwd == "" {
		return gitCtx{}
	}
	// --git-common-dir always points to the main repo's .git, even from a linked worktree.
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return gitCtx{}
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(cwd, commonDir)
	}
	repo := filepath.Base(filepath.Dir(commonDir))

	out, err = exec.Command("git", "-C", cwd, "rev-parse", "--git-dir").Output()
	if err != nil {
		return gitCtx{repo: repo}
	}
	gitDir := strings.TrimSpace(string(out))
	var worktree string
	if idx := strings.LastIndex(gitDir, "/worktrees/"); idx >= 0 {
		remainder := gitDir[idx+len("/worktrees/"):]
		if slashIdx := strings.Index(remainder, "/"); slashIdx >= 0 {
			worktree = remainder[:slashIdx]
		} else {
			worktree = remainder
		}
	}
	return gitCtx{repo: repo, worktree: worktree}
}

func (m model) clampScroll() int {
	rowArea := m.height - 5
	if rowArea < 1 {
		rowArea = 1
	}
	off := m.scrollOffset
	if m.cursor < off {
		off = m.cursor
	}
	if m.cursor >= off+rowArea {
		off = m.cursor - rowArea + 1
	}
	maxOff := len(m.displayed) - rowArea
	if maxOff < 0 {
		maxOff = 0
	}
	if off > maxOff {
		off = maxOff
	}
	if off < 0 {
		off = 0
	}
	return off
}

func (m model) Init() tea.Cmd {
	return tea.Batch(cmdLoadProcesses(), cmdTick())
}

func cmdTick() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func cmdLoadProcesses() tea.Cmd {
	return func() tea.Msg {
		return processesMsg(getListeningProcesses())
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tickMsg:
		return m, tea.Batch(cmdLoadProcesses(), cmdTick())

	case processesMsg:
		m.processes = []Process(msg)
		m.lastRefresh = time.Now()
		m.applyFilter()
		if m.cursor >= len(m.displayed) {
			m.cursor = max(0, len(m.displayed)-1)
		}
		m.scrollOffset = m.clampScroll()
		return m, nil

	case killDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("kill failed: %v", msg.err)
			m.statusIsErr = true
		} else if msg.container != "" {
			m.statusMsg = fmt.Sprintf("stopped container %s", msg.container)
			m.statusIsErr = false
		} else {
			m.statusMsg = fmt.Sprintf("killed PID %d", msg.pid)
			m.statusIsErr = false
		}
		return m, cmdLoadProcesses()

	case openDoneMsg:
		if msg.err != nil {
			m.statusMsg = fmt.Sprintf("open failed: %v", msg.err)
			m.statusIsErr = true
		} else {
			m.statusMsg = fmt.Sprintf("opened %s", msg.url)
			m.statusIsErr = false
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeFilter:
		return m.handleFilterKey(msg)
	case modeConfirm:
		return m.handleConfirmKey(msg)
	default:
		return m.handleNormalKey(msg)
	}
}

func (m model) handleNormalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up":
		if m.cursor > 0 {
			m.cursor--
			m.statusMsg = ""
			m.scrollOffset = m.clampScroll()
		}
	case "down":
		if m.cursor < len(m.displayed)-1 {
			m.cursor++
			m.statusMsg = ""
			m.scrollOffset = m.clampScroll()
		}
	case "pgup":
		rowArea := m.height - 5
		m.cursor = max(0, m.cursor-rowArea)
		m.statusMsg = ""
		m.scrollOffset = m.clampScroll()
	case "pgdown":
		rowArea := m.height - 5
		m.cursor = min(len(m.displayed)-1, m.cursor+rowArea)
		m.statusMsg = ""
		m.scrollOffset = m.clampScroll()
	case "k":
		if len(m.displayed) > 0 {
			m.mode = modeConfirm
			m.statusMsg = ""
		}
	case "r":
		return m, cmdLoadProcesses()
	case "/":
		m.mode = modeFilter
		m.statusMsg = ""
	case "h":
		if len(m.displayed) > 0 {
			proc := m.displayed[m.cursor]
			m.hiddenNames[proc.Name] = true
			saveHidden(m.hiddenNames)
			m.applyFilter()
			if m.cursor >= len(m.displayed) {
				m.cursor = max(0, len(m.displayed)-1)
			}
			m.scrollOffset = m.clampScroll()
			m.statusMsg = fmt.Sprintf("hidden %q — press H to show hidden", proc.Name)
			m.statusIsErr = false
		}
	case "H":
		m.showHidden = !m.showHidden
		m.applyFilter()
		m.cursor = 0
		m.scrollOffset = 0
		if m.showHidden {
			m.statusMsg = "showing hidden processes — press u to unhide, H to hide again"
			m.statusIsErr = false
		} else {
			m.statusMsg = ""
		}
	case "u":
		if len(m.displayed) > 0 {
			proc := m.displayed[m.cursor]
			if m.hiddenNames[proc.Name] {
				delete(m.hiddenNames, proc.Name)
				saveHidden(m.hiddenNames)
				m.applyFilter()
				if m.cursor >= len(m.displayed) {
					m.cursor = max(0, len(m.displayed)-1)
				}
				m.scrollOffset = m.clampScroll()
				m.statusMsg = fmt.Sprintf("unhidden %q", proc.Name)
				m.statusIsErr = false
			}
		}
	case "enter":
		if len(m.displayed) > 0 {
			proc := m.displayed[m.cursor]
			return m, cmdOpenBrowser(proc.Port)
		}
	}
	return m, nil
}

func (m model) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.filterInput = ""
		m.applyFilter()
		m.cursor = 0
		m.scrollOffset = 0
	case "enter":
		m.mode = modeNormal
	case "backspace":
		if len(m.filterInput) > 0 {
			m.filterInput = m.filterInput[:len(m.filterInput)-1]
			m.applyFilter()
			m.cursor = 0
			m.scrollOffset = m.clampScroll()
		}
	default:
		if len(msg.Runes) == 1 && msg.Runes[0] >= 32 {
			m.filterInput += string(msg.Runes[0])
			m.applyFilter()
			m.cursor = 0
			m.scrollOffset = m.clampScroll()
		}
	}
	return m, nil
}

func (m model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		if m.cursor < len(m.displayed) {
			proc := m.displayed[m.cursor]
			m.mode = modeNormal
			if proc.DockerContainer != "" {
				m.statusMsg = fmt.Sprintf("stopping container %s...", proc.DockerContainer)
			} else {
				m.statusMsg = fmt.Sprintf("killing PID %d (%s)...", proc.PID, proc.Name)
			}
			m.statusIsErr = false
			return m, cmdKill(proc)
		}
	case "n", "N", "esc":
		m.mode = modeNormal
		m.statusMsg = ""
	}
	return m, nil
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (m *model) applyFilter() {
	// Build source slice: all processes, or only non-hidden ones.
	source := m.processes
	if !m.showHidden {
		visible := make([]Process, 0, len(m.processes))
		for _, p := range m.processes {
			if !m.hiddenNames[p.Name] {
				visible = append(visible, p)
			}
		}
		source = visible
	}

	if m.filterInput == "" {
		if source == nil {
			source = []Process{}
		}
		m.displayed = source
		return
	}

	var out []Process
	if isAllDigits(m.filterInput) {
		for _, p := range source {
			if strings.Contains(strconv.Itoa(p.Port), m.filterInput) {
				out = append(out, p)
			}
		}
	} else {
		needle := strings.ToLower(m.filterInput)
		for _, p := range source {
			if strings.Contains(strings.ToLower(p.Repo), needle) {
				out = append(out, p)
			}
		}
	}
	if out == nil {
		out = []Process{}
	}
	m.displayed = out
}

func cmdOpenBrowser(port int) tea.Cmd {
	url := fmt.Sprintf("http://localhost:%d", port)
	return func() tea.Msg {
		err := exec.Command("open", url).Run()
		return openDoneMsg{url: url, err: err}
	}
}

func cmdKill(proc Process) tea.Cmd {
	return func() tea.Msg {
		if proc.DockerContainer != "" {
			err := exec.Command("docker", "stop", proc.DockerContainer).Run()
			return killDoneMsg{pid: proc.PID, container: proc.DockerContainer, err: err}
		}

		p, err := os.FindProcess(proc.PID)
		if err != nil {
			return killDoneMsg{pid: proc.PID, err: err}
		}

		if err := p.Signal(syscall.SIGTERM); err != nil {
			// Already dead or no permission — try SIGKILL
			_ = p.Signal(syscall.SIGKILL)
			return killDoneMsg{pid: proc.PID, err: nil}
		}

		// Wait up to sigkillDelay for graceful exit
		deadline := time.Now().Add(sigkillDelay)
		for time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
			if err := p.Signal(syscall.Signal(0)); err != nil {
				// Process is gone
				return killDoneMsg{pid: proc.PID, err: nil}
			}
		}

		// Still alive — force kill
		_ = p.Signal(syscall.SIGKILL)
		return killDoneMsg{pid: proc.PID, err: nil}
	}
}

// getDockerPorts returns a map of host port → container name for all running Docker containers.
func getDockerPorts() map[int]string {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Ports}}").Output()
	if err != nil {
		return nil
	}
	portToContainer := make(map[int]string)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		name, ports := parts[0], parts[1]
		for _, mapping := range strings.Split(ports, ", ") {
			mapping = strings.TrimSpace(mapping)
			// format: "0.0.0.0:3000->3000/tcp" or ":::3000->3000/tcp"
			arrowIdx := strings.Index(mapping, "->")
			if arrowIdx < 0 {
				continue
			}
			hostPart := mapping[:arrowIdx]
			colonIdx := strings.LastIndex(hostPart, ":")
			if colonIdx < 0 {
				continue
			}
			port, err := strconv.Atoi(hostPart[colonIdx+1:])
			if err != nil || port <= 0 {
				continue
			}
			portToContainer[port] = name
		}
	}
	return portToContainer
}

func (m model) View() string {
	if m.width == 0 {
		return "Loading..."
	}

	header := m.renderHeader()
	colHeaders := m.renderColHeaders()
	divider := dimStyle.Render(strings.Repeat("─", m.width))

	// rows area: total height minus header(1) + colheaders(1) + divider(1) + footer(1) + blank(1)
	rowArea := m.height - 5
	if rowArea < 1 {
		rowArea = 1
	}
	rows := m.renderRows(rowArea)
	footer := m.renderFooter()

	return strings.Join([]string{header, colHeaders, divider, rows, footer}, "\n")
}

func (m model) renderHeader() string {
	since := time.Since(m.lastRefresh).Round(time.Second)
	right := fmt.Sprintf("refreshed %s ago  ", since)
	left := " porter"

	// Show hidden count when any names are hidden and not currently showing them.
	if n := len(m.hiddenNames); n > 0 && !m.showHidden {
		left += fmt.Sprintf("  [%d hidden]", n)
	}

	pad := m.width - len(left) - len(right)
	if pad < 0 {
		pad = 0
	}
	line := left + strings.Repeat(" ", pad) + right
	return headerStyle.Width(m.width).Render(line)
}

func (m model) renderColHeaders() string {
	cmdWidth := m.cmdColWidth()
	return colHeaderStyle.Render(
		fmt.Sprintf(" %-*s %-*s %-*s %s %s %s %s",
			colPort-1, "PORT",
			colPID-1, "PID",
			colName-1, "PROCESS",
			truncate("REPO", colRepo-1),
			truncate("TREE", colTree-1),
			truncate("CONTAINER", colContainer-1),
			truncate("COMMAND", cmdWidth),
		),
	)
}

func (m model) renderRows(maxRows int) string {
	var b strings.Builder
	cmdWidth := m.cmdColWidth()

	if len(m.displayed) == 0 {
		msg := "  no listening processes found"
		if m.filterInput != "" {
			if isAllDigits(m.filterInput) {
				msg = fmt.Sprintf("  no processes matching port %q", m.filterInput)
			} else {
				msg = fmt.Sprintf("  no processes matching repo %q", m.filterInput)
			}
		}
		b.WriteString(dimStyle.Render(msg))
		b.WriteString("\n")
		// pad remaining
		for i := 1; i < maxRows; i++ {
			b.WriteString("\n")
		}
		return b.String()
	}

	rendered := 0
	for i := m.scrollOffset; i < len(m.displayed); i++ {
		if rendered >= maxRows {
			break
		}
		p := m.displayed[i]
		isHidden := m.hiddenNames[p.Name]

		portStr := portStyle.Render(fmt.Sprintf("%-*d", colPort-1, p.Port))
		pidStr := fmt.Sprintf("%-*d", colPID-1, p.PID)
		nameStr := fmt.Sprintf("%-*s", colName-1, p.Name)
		repoStr := truncateEnd(p.Repo, colRepo-1)
		treeStr := truncateEnd(p.Worktree, colTree-1)
		containerStr := truncateEnd(p.DockerContainer, colContainer-1)
		cmdStr := truncateEnd(p.Command, cmdWidth)

		plainRow := fmt.Sprintf(" %-*d %-*d %-*s %s %s %s %s",
			colPort-1, p.Port,
			colPID-1, p.PID,
			colName-1, p.Name,
			repoStr,
			treeStr,
			containerStr,
			cmdStr,
		)

		if i == m.cursor {
			b.WriteString(selectedStyle.Width(m.width).Render(plainRow))
		} else if isHidden {
			b.WriteString(hiddenRowStyle.Render(plainRow))
		} else {
			row := " " + portStr + " " + normalStyle.Render(pidStr+" "+nameStr+repoStr+" "+treeStr+" "+containerStr+" "+cmdStr)
			b.WriteString(row)
		}
		b.WriteString("\n")
		rendered++
	}

	for rendered < maxRows {
		b.WriteString("\n")
		rendered++
	}

	return b.String()
}

func (m model) renderFooter() string {
	switch m.mode {
	case modeFilter:
		kind := "port"
		if m.filterInput != "" && !isAllDigits(m.filterInput) {
			kind = "repo"
		}
		return filterStyle.Render(fmt.Sprintf("/ %s filter: %s▌  esc to clear", kind, m.filterInput))
	case modeConfirm:
		if m.cursor < len(m.displayed) {
			p := m.displayed[m.cursor]
			if p.DockerContainer != "" {
				return confirmStyle.Render(fmt.Sprintf("stop container %s (:%d)? [Y/n]", p.DockerContainer, p.Port))
			}
			return confirmStyle.Render(fmt.Sprintf("kill PID %d (%s) on :%d? [Y/n]", p.PID, p.Name, p.Port))
		}
	}

	if m.statusMsg != "" {
		if m.statusIsErr {
			return errorStyle.Render(m.statusMsg)
		}
		return successStyle.Render(m.statusMsg)
	}

	help := "↑/↓ navigate   enter open   pgup/pgdn page   k kill   h hide   H show hidden   r refresh   / filter   q quit"
	if len(m.displayed) > 0 {
		help += fmt.Sprintf("   [%d/%d]", m.cursor+1, len(m.displayed))
	}
	return dimStyle.Render(help)
}

func (m model) cmdColWidth() int {
	w := m.width - colPort - colPID - colName - colRepo - colTree - colContainer - 2
	if w < 10 {
		w = 10
	}
	return w
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return fmt.Sprintf("%-*s", n, s)
	}
	return s[:n-1] + "…"
}

// truncateEnd shows the tail of s when it exceeds n, prefixing with "…".
func truncateEnd(s string, n int) string {
	if len(s) <= n {
		return fmt.Sprintf("%-*s", n, s)
	}
	return "…" + s[len(s)-(n-1):]
}

// getListeningProcesses runs lsof and returns all TCP LISTEN processes.
func getListeningProcesses() []Process {
	out, _ := exec.Command("lsof", "-iTCP", "-P", "-n", "-sTCP:LISTEN").Output()
	if len(out) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	var procs []Process

	for _, line := range strings.Split(string(out), "\n")[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 9 {
			continue
		}

		name := strings.ReplaceAll(fields[0], `\x20`, " ")
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}

		port, ok := extractPort(fields[8:])
		if !ok {
			continue
		}

		key := fmt.Sprintf("%d:%d", pid, port)
		if seen[key] {
			continue
		}
		seen[key] = true

		procs = append(procs, Process{
			Port:    port,
			PID:     pid,
			Name:    name,
			Command: fullCommand(pid),
		})
	}

	sort.Slice(procs, func(i, j int) bool {
		return procs[i].Port < procs[j].Port
	})

	// Batch-fetch cwds then resolve git repo names, deduplicating per cwd.
	uniquePIDs := make([]int, 0, len(procs))
	pidSet := make(map[int]bool)
	for _, p := range procs {
		if !pidSet[p.PID] {
			pidSet[p.PID] = true
			uniquePIDs = append(uniquePIDs, p.PID)
		}
	}
	cwds := batchGetCwds(uniquePIDs)
	ctxCache := make(map[string]gitCtx)
	for i, p := range procs {
		cwd := cwds[p.PID]
		if cwd == "" {
			continue
		}
		ctx, cached := ctxCache[cwd]
		if !cached {
			ctx = gitContextForCwd(cwd)
			ctxCache[cwd] = ctx
		}
		procs[i].Repo = ctx.repo
		procs[i].Worktree = ctx.worktree
	}

	// Annotate processes whose port is published by a Docker container.
	dockerPorts := getDockerPorts()
	for i, p := range procs {
		if container, ok := dockerPorts[p.Port]; ok {
			procs[i].DockerContainer = container
		}
	}

	return procs
}

// extractPort finds the port number in the NAME fields of lsof output.
// Handles formats: *:3000, 127.0.0.1:8080, [::1]:9000
func extractPort(fields []string) (int, bool) {
	for _, f := range fields {
		if strings.HasPrefix(f, "(") {
			continue
		}
		i := strings.LastIndex(f, ":")
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(f[i+1:])
		if err == nil && port > 0 && port <= 65535 {
			return port, true
		}
	}
	return 0, false
}

// fullCommand returns the full argv for a PID via ps.
func fullCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "args=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
