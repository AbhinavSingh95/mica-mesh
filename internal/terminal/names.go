package terminal

import (
	"crypto/sha256"
	"fmt"
)

// Display labels use the existing process identity. They need no registry or
// persistence, agree across terminals, and survive Controller reconnects. The
// full Agent ID remains authoritative and is visible in the details view.
func agentName(id string) string {
	if id == "" {
		return "Starting"
	}
	names := [...]string{"Atlas", "Birch", "Cedar", "Comet", "Coral", "Dune", "Ember", "Fern", "Iris", "Lumen", "Maple", "Nova", "Orion", "Sage", "Willow", "Wren"}
	hash := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%s-%x", names[int(hash[0])%len(names)], hash[1:5])
}
