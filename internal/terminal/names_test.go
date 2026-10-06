package terminal

import (
	"strings"
	"testing"

	"github.com/AbhinavSingh95/mica-mesh/internal/config"
	meshv1 "github.com/AbhinavSingh95/mica-mesh/protocol/mesh/v1"
	"github.com/charmbracelet/x/ansi"
)

func TestAgentNamesKeepIdentityAcrossViewsAndReconnects(t *testing.T) {
	a, b := idleWorker(), idleWorker()
	a.WorkerId, b.WorkerId = "b018efb7-e340-4651-a5ac-0ac1a650de51", "1bbd0230-fbbb-4f73-886b-09967f9a6603"
	a.Hardware, b.Hardware = &meshv1.HardwareInfo{Hostname: "same-mac"}, &meshv1.HardwareInfo{Hostname: "same-mac"}
	first, second := agentName(a.WorkerId), agentName(b.WorkerId)
	if first == "" || first == second || first == a.WorkerId {
		t.Fatal("same-host Agents need distinct readable names")
	}
	for _, workers := range [][]*meshv1.WorkerInfo{{a, b}, {b, a}, {a}} {
		m := readyScreen()
		m.state.status.Workers = workers
		m.activateCommand("agents")
		if text := ansi.Strip(m.View().Content); !strings.Contains(text, first) || !strings.Contains(text, a.WorkerId) {
			t.Fatalf("name or exact identity changed after refresh: %s", text)
		}
	}
	m := newScreen(Options{Config: config.Default(), Role: config.RoleWorker})
	m.state.agent.WorkerID = a.WorkerId
	if !strings.Contains(ansi.Strip(m.View().Content), first) {
		t.Fatal("Agent and Controller disagree about the display name")
	}
	m = readyScreen()
	m.state.request = requestView{id: "request", workerID: a.WorkerId, hostname: "same-mac", joined: true}
	m.consume()
	if len(m.records) != 1 || m.records[0].worker != first {
		t.Fatal("inference result did not use its Agent name")
	}
}

func TestAgentNamesDoNotRenderRemoteControlText(t *testing.T) {
	name := agentName("\x1b]52;hostile\a\n\t")
	if strings.ContainsAny(name, "\x1b\a\n\t") || name == "" {
		t.Fatalf("unsafe name %q", name)
	}
}
