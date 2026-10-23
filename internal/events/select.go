package events

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file is the executable form of two rule sets in docs/events/v1/README.md:
// the meter.sample selection rules (M1-M7) and the timestamp rule (T1). The
// cases in docs/events/v1/conformance.json pin them; the decoder calls these
// functions rather than re-implementing the rules.

// SampledValue is one element of MeterValue.sampledValue, with OCPP's own
// field names. Absent fields are empty strings.
type SampledValue struct {
	Value     string `json:"value"`
	Context   string `json:"context,omitempty"`
	Format    string `json:"format,omitempty"`
	Measurand string `json:"measurand,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Location  string `json:"location,omitempty"`
	Unit      string `json:"unit,omitempty"`
}

type measurandSpec struct {
	name string
	// defaultUnit applies when a value has no unit. OCPP 1.6 §7.43 defines it
	// for energy only; for W, A and Percent it is octap's own assumption (M2).
	defaultUnit string
	// units maps each accepted unit to its factor to the base unit.
	units map[string]float64
	// anyLocation: the location is not filtered (SoC, M3).
	anyLocation bool
	// phased: combine phases (M4); SoC has none.
	phased bool
	// sum: phases add up (energy, power); otherwise the maximum (current).
	sum bool
}

var (
	specEnergy  = measurandSpec{"Energy.Active.Import.Register", "Wh", map[string]float64{"Wh": 1, "kWh": 1000}, false, true, true}
	specPower   = measurandSpec{"Power.Active.Import", "W", map[string]float64{"W": 1, "kW": 1000}, false, true, true}
	specCurrent = measurandSpec{"Current.Import", "A", map[string]float64{"A": 1}, false, true, false}
	specSoC     = measurandSpec{"SoC", "Percent", map[string]float64{"Percent": 1}, true, false, false}
)

var readingContexts = []ReadingContext{
	ContextInterruptionBegin, ContextInterruptionEnd, ContextSampleClock, ContextSamplePeriodic,
	ContextTransactionBegin, ContextTransactionEnd, ContextTrigger, ContextOther,
}

type candidate struct {
	idx   int // position in the sampledValue array
	phase string
	loc   string
	v     float64 // normalised to the base unit
}

// SelectMeterSample applies README rules M1-M7 to the sampled values of one
// MeterValue element on connectorID. It leaves TransactionID nil (the
// request's, not the element's). ok is false when no measurand has a usable
// value: that element makes no event (M7).
func SelectMeterSample(connectorID int, values []SampledValue) (s MeterSample, ok bool) {
	firstUsed := math.MaxInt
	pick := func(spec measurandSpec) *float64 {
		v, used := selectValue(connectorID, values, spec)
		if used < firstUsed {
			firstUsed = used
		}
		return v
	}
	s.EnergyWh = pick(specEnergy)
	s.PowerW = pick(specPower)
	s.CurrentA = pick(specCurrent)
	s.SoCPercent = pick(specSoC)
	if s.EnergyWh == nil && s.PowerW == nil && s.CurrentA == nil && s.SoCPercent == nil {
		return MeterSample{}, false
	}
	// M6: the first used value in array order decides the context.
	if c := ReadingContext(values[firstUsed].Context); slices.Contains(readingContexts, c) {
		s.Context = &c
	}
	return s, true
}

// selectValue returns the value for one measurand and the array index of the
// first sampled value it used (math.MaxInt when none).
func selectValue(connectorID int, values []SampledValue, spec measurandSpec) (*float64, int) {
	// M1, M2: usable values of this measurand, in array order.
	var cands []candidate
	for i, sv := range values {
		if sv.Format != "" && sv.Format != "Raw" {
			continue
		}
		m := sv.Measurand
		if m == "" {
			m = specEnergy.name
		}
		if m != spec.name {
			continue
		}
		unit := sv.Unit
		if unit == "" {
			unit = spec.defaultUnit
		}
		factor, known := spec.units[unit]
		if !known {
			continue
		}
		v, err := strconv.ParseFloat(sv.Value, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		loc := sv.Location
		if loc == "" {
			loc = "Outlet"
		}
		cands = append(cands, candidate{idx: i, phase: sv.Phase, loc: loc, v: v * factor})
	}

	// M3: location, per measurand.
	if !spec.anyLocation {
		want := "Outlet"
		if connectorID == 0 && slices.ContainsFunc(cands, func(c candidate) bool { return c.loc == "Inlet" }) {
			want = "Inlet"
		}
		cands = slices.DeleteFunc(cands, func(c candidate) bool { return c.loc != want })
	}
	if len(cands) == 0 {
		return nil, math.MaxInt
	}

	// M5: the first qualifying value in array order wins; cands is in order.
	first := func(match func(candidate) bool) (candidate, bool) {
		if i := slices.IndexFunc(cands, match); i >= 0 {
			return cands[i], true
		}
		return candidate{}, false
	}
	if !spec.phased {
		return &cands[0].v, cands[0].idx
	}
	// M4: an unphased value wins over phases.
	if c, found := first(func(c candidate) bool { return c.phase == "" }); found {
		return &c.v, c.idx
	}
	var total float64
	used := math.MaxInt
	n := 0
	for _, p := range []string{"L1", "L2", "L3"} {
		c, found := first(func(c candidate) bool { return c.phase == p })
		if !found {
			c, found = first(func(c candidate) bool { return c.phase == p+"-N" })
		}
		if !found {
			continue
		}
		switch {
		case spec.sum:
			total += c.v
		case n == 0 || c.v > total:
			total = c.v
		}
		n++
		used = min(used, c.idx)
	}
	if n == 0 {
		return nil, math.MaxInt
	}
	return &total, used
}

// frameTime is the grammar of README rule T1: an ISO 8601 extended date and
// time, optional fractional seconds (decimal sign . or ,, as ISO 8601 allows),
// and an optional zone designator (Z or an offset of the form +hh:mm, +hhmm
// or +hh).
var frameTime = regexp.MustCompile(
	`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:[.,]\d+)?)(?:Z|([+-]\d{2})(?::?(\d{2}))?)?$`)

// ParseFrameTime reads an OCPP timestamp (README rule T1). A time with a zone
// designator (Z or an offset, with or without a colon) is converted to UTC.
// OCPP 1.6 §3.15 requires receivers to handle offsets, and allows a time
// without a zone designator; that is read as UTC, the zone §3.14 recommends,
// so the event is kept. Anything outside the T1 grammar is an error.
func ParseFrameTime(s string) (time.Time, error) {
	m := frameTime.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("events: %q is not an OCPP timestamp (ISO 8601 extended date and time, see README rule T1)", s)
	}
	// No designator reads as UTC, so it parses like Z. An offset is rewritten
	// to the +hh:mm form time.Parse takes; its hours must be 00-23 and its minutes 00-59.
	zone := "Z"
	if m[2] != "" {
		// time.Parse accepts hour 24 and minute 60 in an offset; T1 does not.
		mm := cmp.Or(m[3], "00")
		if m[2][1:] > "23" || mm > "59" {
			return time.Time{}, fmt.Errorf("events: %q has an offset out of range (hours 00-23, minutes 00-59)", s)
		}
		zone = m[2] + ":" + mm
	}
	// time.Parse takes only a point before the fraction.
	t, err := time.Parse(time.RFC3339Nano, strings.Replace(m[1], ",", ".", 1)+zone)
	if err != nil {
		return time.Time{}, fmt.Errorf("events: %q is not an OCPP timestamp: %w", s, err)
	}
	return t.UTC(), nil
}
