package ui

import "github.com/charmbracelet/lipgloss"

var (
	purple    = lipgloss.Color("#7C3AED")
	orange    = lipgloss.Color("#F97316")
	green     = lipgloss.Color("#10B981")
	red       = lipgloss.Color("#EF4444")
	yellow    = lipgloss.Color("#F59E0B")
	gray      = lipgloss.Color("#6B7280")
	lightGray = lipgloss.Color("#9CA3AF")
	white     = lipgloss.Color("#F9FAFB")
)

var (
	titleStyle = lipgloss.NewStyle().
			Foreground(white).
			Background(purple).
			Bold(true).
			Padding(0, 2)

	breadcrumbStyle = lipgloss.NewStyle().
			Foreground(lightGray).
			Italic(true)

	spinnerStyle = lipgloss.NewStyle().
			Foreground(purple)

	loadingStyle = lipgloss.NewStyle().
			Foreground(lightGray)

	errorStyle = lipgloss.NewStyle().
			Foreground(red).
			Bold(true)

	hintStyle = lipgloss.NewStyle().
			Foreground(gray)

	statusRunning = lipgloss.NewStyle().Foreground(green).Bold(true)
	statusStopped = lipgloss.NewStyle().Foreground(red).Bold(true)
	statusPending = lipgloss.NewStyle().Foreground(yellow).Bold(true)
)

func statusColor(status string) string {
	switch status {
	case "RUNNING", "ACTIVE":
		return statusRunning.Render(status)
	case "STOPPED", "INACTIVE", "DRAINING":
		return statusStopped.Render(status)
	default:
		return statusPending.Render(status)
	}
}
