package terminal

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/AbhinavSingh95/mica-mesh/internal/app"
	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	"github.com/AbhinavSingh95/mica-mesh/internal/setup"
)

// Options carries parsed settings into the guided session. A nil Release means
// no installed package; Controller does not require managed runtime assets.
type Options struct {
	Config  config.Config
	Assets  config.AssetFields
	Layout  setup.Layout
	Release *setup.Release
	Role    config.Role
	Network app.NetworkMode
}

// Run owns one guided session. The role screens and application effects are
// added in U4; this boundary screen makes no runtime or service readiness claim.
func Run(ctx context.Context, options Options, console *Console) error {
	if options.Role != 0 && options.Role != config.RoleController && options.Role != config.RoleWorker {
		return errors.New("guided mode needs one role; choose Controller or Agent")
	}
	exit := make(chan struct{}, 1)
	return console.run(ctx, boundaryModel{exit: exit, console: console}, func(ctx context.Context, p *tea.Program) error {
		select {
		case <-exit:
			return nil
		case <-ctx.Done():
			return nil
		}
	})
}

type boundaryModel struct {
	exit    chan<- struct{}
	console *Console
	notice  string
}

func (m boundaryModel) Init() tea.Cmd { return nil }
func (m boundaryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	leave := false
	switch v := msg.(type) {
	case diagnosticsMsg:
		m.notice = strings.TrimSpace(m.console.diagnostics.text())
	case interruptMsg:
		leave = true
	case tea.KeyPressMsg:
		leave = v.String() == "ctrl+c" || v.String() == "q"
	}
	if leave {
		select {
		case m.exit <- struct{}{}:
		default:
		}
	}
	return m, nil
}
func (m boundaryModel) View() tea.View {
	v := tea.NewView(lipgloss.NewStyle().Padding(1, 2).Render("Mica Mesh\n\nController and Agent screens are not available in this checkpoint.\nUse --plain to run a prepared role.\n\nPress Ctrl-C or q to exit.\n\n" + m.notice))
	v.AltScreen = true
	return v
}
