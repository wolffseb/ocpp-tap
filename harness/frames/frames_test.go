package frames

import (
	"strings"
	"sync"
	"testing"
)

func f(charger string, dir Direction, data string) Frame {
	return Frame{Charger: charger, Dir: dir, Data: []byte(data)}
}

func TestLogKeepsOrderAndCopies(t *testing.T) {
	t.Parallel()

	var l Log
	buf := []byte(`[2,"1","Heartbeat",{}]`)
	l.Add("CP1", Upstream, buf)
	buf[0] = 'X' // the caller reusing its buffer must not change the log
	l.Add("CP1", Downstream, []byte(`[3,"1",{}]`))
	l.Add("CP2", Upstream, []byte(`[2,"9","Heartbeat",{}]`))

	got := l.Frames()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if string(got[0].Data) != `[2,"1","Heartbeat",{}]` {
		t.Errorf("first frame = %q, the log did not copy the caller's buffer", got[0].Data)
	}
	if got[1].Dir != Downstream || got[2].Charger != "CP2" {
		t.Errorf("order or fields wrong: %+v", got)
	}
	if n := len(l.For("CP1", Upstream)); n != 1 {
		t.Errorf("For(CP1, Upstream) = %d frames, want 1", n)
	}
}

func TestLogConcurrentAdd(t *testing.T) {
	t.Parallel()

	var l Log
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); l.Add("CP", Upstream, []byte("x")) }()
	}
	wg.Wait()
	if n := len(l.Frames()); n != 50 {
		t.Fatalf("len = %d, want 50", n)
	}
}

// The adversarial payloads are the ones a "helpful" re-encode would change.
var adversarial = []string{
	`[2,"1","Heartbeat",{}]`,
	"[2, \"2\" ,\"BootNotification\" , {\"b\":1,\"a\":2}]",
	`[2,"3","DataTransfer",{"vendorId":"é ","data":"<>&"}]`,
	`[3,"4",{"n":1e400,"m":0.10000000000000000001,"k":-0}]`,
	`[2,"5","MeterValues",{"x":"` + strings.Repeat("A", 70000) + `"}]`,
	"[2,\"6\",\"Heartbeat\",{}]\n",
}

func TestCompareIdentical(t *testing.T) {
	t.Parallel()

	var a, b []Frame
	for _, p := range adversarial {
		a = append(a, f("CP", Upstream, p))
		b = append(b, f("CP", Upstream, p))
	}
	a = append(a, f("CP", Downstream, `[3,"1",{}]`))
	b = append(b, f("CP", Downstream, `[3,"1",{}]`))

	if diffs := Compare(a, b); len(diffs) != 0 {
		t.Fatalf("identical logs reported %d diffs: %v", len(diffs), diffs)
	}
}

// Frames of different chargers may interleave differently on the two sides;
// only the order within one charger and direction is meaningful.
func TestCompareIgnoresInterleavingAcrossChargersAndDirections(t *testing.T) {
	t.Parallel()

	charger := []Frame{
		f("A", Upstream, "a1"), f("B", Upstream, "b1"), f("A", Downstream, "r1"), f("A", Upstream, "a2"),
	}
	cpms := []Frame{
		f("B", Upstream, "b1"), f("A", Downstream, "r1"), f("A", Upstream, "a1"), f("A", Upstream, "a2"),
	}
	if diffs := Compare(charger, cpms); len(diffs) != 0 {
		t.Fatalf("got diffs %v", diffs)
	}
}

func TestCompareCatchesAlterations(t *testing.T) {
	t.Parallel()

	base := []Frame{
		f("CP", Upstream, `[2,"1","Heartbeat",{}]`),
		f("CP", Upstream, `[2,"2","Heartbeat",{}]`),
		f("CP", Downstream, `[3,"1",{}]`),
	}
	clone := func() []Frame { return append([]Frame(nil), base...) }

	cases := map[string]func() []Frame{
		"one byte changed": func() []Frame {
			c := clone()
			c[0] = f("CP", Upstream, `[2,"1","Heartbeat",{ }]`)
			return c
		},
		"message id changed": func() []Frame {
			c := clone()
			c[2] = f("CP", Downstream, `[3,"9",{}]`)
			return c
		},
		"reordered": func() []Frame {
			c := clone()
			c[0], c[1] = c[1], c[0]
			return c
		},
		"dropped": func() []Frame { return clone()[1:] },
		"extra":   func() []Frame { return append(clone(), f("CP", Upstream, `[2,"3","Heartbeat",{}]`)) },
		"wrong charger": func() []Frame {
			c := clone()
			c[1] = f("OTHER", Upstream, `[2,"2","Heartbeat",{}]`)
			return c
		},
		"wrong direction": func() []Frame {
			c := clone()
			c[2] = f("CP", Upstream, `[3,"1",{}]`)
			return c
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if diffs := Compare(base, mutate()); len(diffs) == 0 {
				t.Fatal("alteration not detected")
			}
		})
	}
}

func TestDiffNamesWhereAndWhat(t *testing.T) {
	t.Parallel()

	diffs := Compare(
		[]Frame{f("CP", Upstream, "abc")},
		[]Frame{f("CP", Upstream, "abd")},
	)
	if len(diffs) != 1 {
		t.Fatalf("diffs = %v", diffs)
	}
	d := diffs[0]
	if d.Charger != "CP" || d.Dir != Upstream || d.Index != 0 {
		t.Errorf("diff = %+v", d)
	}
	s := d.String()
	for _, want := range []string{"CP", "upstream", "#0", `"abc"`, `"abd"`} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
}
