package toolchain_test

import (
	"testing"

	toolchain "github.com/AbhinavSingh95/mica-mesh/tools/toolchain"
	"google.golang.org/protobuf/proto"
)

// These assignments ensure the gRPC generator ran alongside the message generator.
var _ toolchain.SmokeServer = (*toolchain.UnimplementedSmokeServer)(nil)
var _ = toolchain.NewSmokeClient

func TestGeneratedProtocol(t *testing.T) {
	message := &toolchain.Echo{Text: "toolchain smoke test"}
	wire, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var decoded toolchain.Echo
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetText() != message.GetText() {
		t.Fatalf("generated message round trip = %q, want %q", decoded.GetText(), message.GetText())
	}
}
