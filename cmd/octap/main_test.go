package main

import "testing"

func TestRun(t *testing.T) {
	t.Parallel()

	if got := run([]string{"version"}); got != 0 {
		t.Errorf("run(version) = %d, want 0", got)
	}
	if got := run(nil); got != 2 {
		t.Errorf("run() = %d, want 2", got)
	}
}
