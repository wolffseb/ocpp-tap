package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestREADMELabelsUnitAssumption: OCPP 1.6 §7.43 defaults the unit only for
// energy, so the W, A and Percent defaults are octap's own and the README must
// say so rather than attribute them to the spec.
func TestREADMELabelsUnitAssumption(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(schemaDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "octap's own assumption") {
		t.Error("README M2 doesn't label the W, A and Percent default units as octap's own assumption")
	}
}

type conformance struct {
	MeterSamples []struct {
		Name          string          `json:"name"`
		Rules         []string        `json:"rules"`
		ConnectorID   int             `json:"connector_id"`
		SampledValues []SampledValue  `json:"sampled_values"`
		Expect        json.RawMessage `json:"expect"`
	} `json:"meter_samples"`
	Timestamps []struct {
		Name   string   `json:"name"`
		Rules  []string `json:"rules"`
		Input  string   `json:"input"`
		Expect *string  `json:"expect"`
	} `json:"timestamps"`
}

func readConformance(t *testing.T) conformance {
	t.Helper()
	var c conformance
	readJSON(t, filepath.Join(schemaDir, "conformance.json"), &c)
	return c
}

// TestSelectMeterSample runs the README's meter.sample rules (M1-M7) as
// written in docs/events/v1/conformance.json.
func TestSelectMeterSample(t *testing.T) {
	c := readConformance(t)
	if len(c.MeterSamples) == 0 {
		t.Fatal("no meter_samples cases")
	}
	_, byType := compileSchemas(t)
	for _, tc := range c.MeterSamples {
		t.Run(tc.Name, func(t *testing.T) {
			got, ok := SelectMeterSample(tc.ConnectorID, tc.SampledValues)
			if string(tc.Expect) == "null" {
				if ok {
					t.Fatalf("made an event, want none: %+v", got)
				}
				return
			}
			if !ok {
				t.Fatal("made no event")
			}
			var want MeterSample
			if err := json.Unmarshal(tc.Expect, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				g, _ := json.Marshal(got)
				t.Fatalf("got %s, want %s", g, tc.Expect)
			}
			// What the rules produce is a valid v1 event.
			ev := NewMeterSample("CP1", tc.ConnectorID, frameTS, received, got)
			if err := validate(t, byType[TypeMeterSample], marshal(t, ev)); err != nil {
				t.Fatalf("result does not validate: %v", err)
			}
		})
	}
}

// TestParseFrameTime runs rule T1.
func TestParseFrameTime(t *testing.T) {
	for _, tc := range readConformance(t).Timestamps {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := ParseFrameTime(tc.Input)
			if tc.Expect == nil {
				if err == nil {
					t.Fatalf("parsed %q as %s, want an error", tc.Input, formatTime(got))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s := formatTime(got); s != *tc.Expect {
				t.Fatalf("got %s, want %s", s, *tc.Expect)
			}
			if got.Location().String() != "UTC" {
				t.Fatalf("location %s, want UTC", got.Location())
			}
		})
	}
}

// TestRulesHaveCases ties README.md's rule ids to conformance.json: every
// rule the README states has a case, and every case names a stated rule. A
// rule added to the README without a case fails here.
func TestRulesHaveCases(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join(schemaDir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var stated []string
	for _, m := range regexp.MustCompile(`(?m)^- \*\*([MT][0-9]+)\*\*`).FindAllSubmatch(readme, -1) {
		stated = append(stated, string(m[1]))
	}
	if len(stated) == 0 {
		t.Fatal("README states no rules")
	}
	c := readConformance(t)
	var used []string
	for _, tc := range c.MeterSamples {
		used = append(used, tc.Rules...)
	}
	for _, tc := range c.Timestamps {
		used = append(used, tc.Rules...)
	}
	for _, r := range stated {
		if !slices.Contains(used, r) {
			t.Errorf("README rule %s has no case in conformance.json", r)
		}
	}
	for _, r := range used {
		if !slices.Contains(stated, r) {
			t.Errorf("conformance.json names rule %s that the README doesn't state", r)
		}
	}
}

// TestBootedIntervalAnyStatus: interval_s is valid with every status, zero
// or not (OCPP 1.6 4.2: with a status other than Accepted it is the minimum
// wait before the next BootNotification, and 0 lets the charger choose).
func TestBootedIntervalAnyStatus(t *testing.T) {
	_, byType := compileSchemas(t)
	for _, st := range []RegistrationStatus{RegistrationAccepted, RegistrationPending, RegistrationRejected} {
		for _, n := range []int{0, 60} {
			ev := NewChargerBooted("CP1", received, ChargerBooted{Vendor: "V", Model: "M", RegistrationStatus: st, IntervalS: n})
			if err := validate(t, byType[TypeChargerBooted], marshal(t, ev)); err != nil {
				t.Errorf("%s/%d: %v", st, n, err)
			}
		}
	}
}
