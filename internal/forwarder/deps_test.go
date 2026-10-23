package forwarder

import (
	"os/exec"
	"strings"
	"testing"
)

// SP1: the forwarder imports nothing from the tap, API, UI or harness
// packages. depguard enforces this in lint; this test enforces it in
// `go test` too, transitively.
func TestForwarderDependencies(t *testing.T) {
	t.Parallel()

	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const mod = "github.com/wolffseb/ocpp-tap/"
	forbidden := []string{mod + "internal/tap", mod + "internal/api", mod + "internal/ui", mod + "harness"}
	for _, dep := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if dep == f || strings.HasPrefix(dep, f+"/") {
				t.Errorf("forwarder depends on %s", dep)
			}
		}
	}
}
