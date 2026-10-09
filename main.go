package main

import (
	"fmt"
	"os"

	"github.com/evelez/ecsconnect/internal/logger"
	"github.com/evelez/ecsconnect/internal/ui"
	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	logger.Init()

	p := tea.NewProgram(
		ui.New(),
		tea.WithAltScreen(),
	)
	if _, err := p.Run(); err != nil {
		logger.Error("program exited with error", "error", err)
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
