package events

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// update rewrites the example files from the Go values below:
//
//	go test ./internal/events -run TestExamplesMatchGo -update
var update = flag.Bool("update", false, "rewrite docs/events/v1/examples from the Go values")

// schemaDir is the published schema, relative to this package.
const schemaDir = "../../docs/events/v1"

// allTypes is every v1 event type, in the order the PRD lists them.
var allTypes = []Type{
	TypeChargerConnected,
	TypeChargerDisconnected,
	TypeChargerBooted,
	TypeConnectorStatus,
	TypeMeterSample,
	TypeTransactionStarted,
	TypeTransactionStopped,
}

func ptr[T any](v T) *T { return &v }

// Fixed values so the examples are stable. received is not UTC on purpose:
// the marshalled event must be.
var (
	received = time.Date(2026, 10, 23, 11, 15, 2, 123456789, time.FixedZone("CEST", 2*3600))
	frameTS  = time.Date(2026, 10, 23, 9, 15, 0, 0, time.UTC)
)

// full returns one event per type with every optional field set. These are
// the examples in docs/events/v1/examples; TestExamplesMatchGo keeps the two
// identical.
func full() map[Type]Event {
	evs := []Event{
		NewChargerConnected("CP1", received, ChargerConnected{Subprotocol: "ocpp1.6"}),
		NewChargerDisconnected("CP1", received, ChargerDisconnected{Reason: "upstream lost"}),
		NewChargerBooted("CP1", received, ChargerBooted{
			Vendor:             "Kempower",
			Model:              "S-Series",
			SerialNumber:       ptr("KP-2026-000123"),
			FirmwareVersion:    ptr("1.42.0"),
			RegistrationStatus: RegistrationAccepted,
			HeartbeatIntervalS: 300,
		}),
		NewConnectorStatus("CP1", 1, &frameTS, received, ConnectorStatus{
			Status:          StatusSuspendedEV,
			ErrorCode:       ErrorOtherError,
			Info:            ptr("EV paused charging"),
			VendorID:        ptr("com.example"),
			VendorErrorCode: ptr("E042"),
		}),
		NewMeterSample("CP1", 1, frameTS, received, MeterSample{
			TransactionID: ptr(4711),
			EnergyWh:      ptr(123456.5),
			PowerW:        ptr(150000.0),
			CurrentA:      ptr(310.2),
			SoCPercent:    ptr(57.0),
			Context:       ptr(ContextSamplePeriodic),
		}),
		NewTransactionStarted("CP1", 1, frameTS, received, TransactionStarted{
			TransactionID:       4711,
			IDTag:               "04A1B2C3D4E5F6",
			MeterStartWh:        123000,
			ReservationID:       ptr(17),
			AuthorizationStatus: AuthorizationAccepted,
		}),
		NewTransactionStopped("CP1", ptr(1), frameTS, received, TransactionStopped{
			TransactionID: 4711,
			MeterStopWh:   187650,
			Reason:        ptr(ReasonEVDisconnected),
			IDTag:         ptr("04A1B2C3D4E5F6"),
		}),
	}
	m := map[Type]Event{}
	for i, ev := range evs {
		// Fixed, valid UUIDv7s so the examples don't change on every run.
		ev.ID = []string{
			"019a1056-6b63-7000-8000-000000000001",
			"019a1056-6b63-7000-8000-000000000002",
			"019a1056-6b63-7000-8000-000000000003",
			"019a1056-6b63-7000-8000-000000000004",
			"019a1056-6b63-7000-8000-000000000005",
			"019a1056-6b63-7000-8000-000000000006",
			"019a1056-6b63-7000-8000-000000000007",
		}[i]
		m[ev.Type] = ev
	}
	return m
}

// minimal returns one event per type with every optional field left out.
func minimal() map[Type]Event {
	evs := []Event{
		NewChargerConnected("CP1", received, ChargerConnected{Subprotocol: "ocpp1.6"}),
		NewChargerDisconnected("CP1", received, ChargerDisconnected{Reason: "charger left"}),
		NewChargerBooted("CP1", received, ChargerBooted{
			Vendor: "V", Model: "M", RegistrationStatus: RegistrationPending, HeartbeatIntervalS: 0,
		}),
		NewConnectorStatus("CP1", 0, nil, received, ConnectorStatus{Status: StatusAvailable, ErrorCode: ErrorNoError}),
		NewMeterSample("CP1", 2, frameTS, received, MeterSample{}),
		NewTransactionStarted("CP1", 2, frameTS, received, TransactionStarted{
			TransactionID: 1, IDTag: "A", AuthorizationStatus: AuthorizationInvalid,
		}),
		NewTransactionStopped("CP1", nil, frameTS, received, TransactionStopped{TransactionID: 1}),
	}
	m := map[Type]Event{}
	for _, ev := range evs {
		m[ev.Type] = ev
	}
	return m
}

func compileSchemas(t *testing.T) (envelope *jsonschema.Schema, byType map[Type]*jsonschema.Schema) {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	// Formats (date-time) are annotations in 2020-12 unless asserted.
	c.AssertFormat()
	load := func(name string) *jsonschema.Schema {
		p, err := filepath.Abs(filepath.Join(schemaDir, name))
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Compile(p)
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		return s
	}
	envelope = load("envelope.schema.json")
	byType = map[Type]*jsonschema.Schema{}
	for _, typ := range allTypes {
		byType[typ] = load(string(typ) + ".schema.json")
	}
	return envelope, byType
}

func validate(t *testing.T, s *jsonschema.Schema, doc []byte) error {
	t.Helper()
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("not JSON: %v\n%s", err, doc)
	}
	return s.Validate(inst)
}

func readExample(t *testing.T, typ Type) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(schemaDir, "examples", string(typ)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func marshal(t *testing.T, ev Event) []byte {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal %s: %v", ev.Type, err)
	}
	return b
}

// The published directory holds exactly the envelope, one schema per v1
// type, and one example per type: nothing missing, nothing extra.
func TestSchemaFilesComplete(t *testing.T) {
	t.Parallel()

	want := []string{"envelope.schema.json"}
	wantEx := []string{}
	for _, typ := range allTypes {
		want = append(want, string(typ)+".schema.json")
		wantEx = append(wantEx, string(typ)+".json")
	}
	got, _ := filepath.Glob(filepath.Join(schemaDir, "*.schema.json"))
	gotEx, _ := filepath.Glob(filepath.Join(schemaDir, "examples", "*.json"))
	base := func(ps []string) []string {
		out := []string{}
		for _, p := range ps {
			out = append(out, filepath.Base(p))
		}
		slices.Sort(out)
		return out
	}
	slices.Sort(want)
	slices.Sort(wantEx)
	if !slices.Equal(base(got), want) {
		t.Errorf("schemas = %v, want %v", base(got), want)
	}
	if !slices.Equal(base(gotEx), wantEx) {
		t.Errorf("examples = %v, want %v", base(gotEx), wantEx)
	}
}

// Acceptance 1: every example validates against its schema and the envelope.
func TestExamplesValidate(t *testing.T) {
	t.Parallel()

	env, schemas := compileSchemas(t)
	for _, typ := range allTypes {
		doc := readExample(t, typ)
		if err := validate(t, schemas[typ], doc); err != nil {
			t.Errorf("example %s: %v", typ, err)
		}
		if err := validate(t, env, doc); err != nil {
			t.Errorf("example %s against envelope: %v", typ, err)
		}
	}
}

// Go values for every type, with and without optional fields, marshal to
// JSON that validates.
func TestGoValuesValidate(t *testing.T) {
	t.Parallel()

	env, schemas := compileSchemas(t)
	for name, set := range map[string]map[Type]Event{"full": full(), "minimal": minimal()} {
		for _, typ := range allTypes {
			ev, ok := set[typ]
			if !ok {
				t.Fatalf("%s: no %s value", name, typ)
			}
			doc := marshal(t, ev)
			if err := validate(t, schemas[typ], doc); err != nil {
				t.Errorf("%s %s: %v\n%s", name, typ, err, doc)
			}
			if err := validate(t, env, doc); err != nil {
				t.Errorf("%s %s against envelope: %v\n%s", name, typ, err, doc)
			}
		}
	}
}

// Acceptance 3: the envelope carries charger ID, connector ID, the frame's
// timestamp and the receive time, and every one of them is required.
func TestEnvelopeFields(t *testing.T) {
	t.Parallel()

	var env struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	readJSON(t, filepath.Join(schemaDir, "envelope.schema.json"), &env)
	want := []string{"schema_version", "id", "type", "charger_id", "connector_id", "timestamp", "received_at", "data"}
	slices.Sort(want)
	got := slices.Sorted(maps.Keys(env.Properties))
	if !slices.Equal(got, want) {
		t.Errorf("envelope properties = %v, want %v", got, want)
	}
	slices.Sort(env.Required)
	if !slices.Equal(env.Required, want) {
		t.Errorf("envelope required = %v, want %v", env.Required, want)
	}

	// And the Go envelope marshals exactly these keys, for every type.
	for _, ev := range minimal() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(marshal(t, ev), &m); err != nil {
			t.Fatal(err)
		}
		if got := slices.Sorted(maps.Keys(m)); !slices.Equal(got, want) {
			t.Errorf("%s marshals keys %v, want %v", ev.Type, got, want)
		}
	}
}

func TestMarshalTimes(t *testing.T) {
	t.Parallel()

	ev := full()[TypeMeterSample]
	ev.Timestamp = ptr(time.Date(2026, 10, 23, 11, 15, 0, 500, time.FixedZone("CEST", 2*3600)))
	var m struct {
		SchemaVersion string  `json:"schema_version"`
		Timestamp     *string `json:"timestamp"`
		ReceivedAt    string  `json:"received_at"`
	}
	if err := json.Unmarshal(marshal(t, ev), &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != "1" {
		t.Errorf("schema_version = %q, want \"1\"", m.SchemaVersion)
	}
	if want := "2026-10-23T09:15:02.123456789Z"; m.ReceivedAt != want {
		t.Errorf("received_at = %q, want %q (UTC, nanoseconds)", m.ReceivedAt, want)
	}
	if m.Timestamp == nil || *m.Timestamp != "2026-10-23T09:15:00.0000005Z" {
		t.Errorf("timestamp = %v, want the frame's instant in UTC", m.Timestamp)
	}

	// Events without a frame timestamp marshal null, not a zero time.
	b := marshal(t, full()[TypeChargerBooted])
	if !bytes.Contains(b, []byte(`"timestamp":null`)) {
		t.Errorf("booted event: %s, want \"timestamp\":null", b)
	}
}

func TestMarshalRejectsMismatchedData(t *testing.T) {
	t.Parallel()

	ev := full()[TypeConnectorStatus]
	ev.Type = TypeMeterSample
	if _, err := json.Marshal(ev); err == nil {
		t.Error("marshal of a meter.sample event with connector.status data: no error")
	}
	ev.Data = nil
	if _, err := json.Marshal(ev); err == nil {
		t.Error("marshal of an event without data: no error")
	}
}

// The example files and the Go types can't drift apart: each example is
// exactly what the fully populated Go value marshals to.
func TestExamplesMatchGo(t *testing.T) {
	for typ, ev := range full() {
		got := marshal(t, ev)
		path := filepath.Join(schemaDir, "examples", string(typ)+".json")
		if *update {
			var out bytes.Buffer
			if err := json.Indent(&out, got, "", "  "); err != nil {
				t.Fatal(err)
			}
			out.WriteString("\n")
			if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		var a, b any
		if err := json.Unmarshal(got, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(readExample(t, typ), &b); err != nil {
			t.Fatalf("example %s: %v", typ, err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: Go value marshals to\n%s\nbut the example is\n%s\n(run with -update if the Go change is intended)", typ, got, readExample(t, typ))
		}
	}
}

// Each data schema lists exactly the Go struct's JSON fields, and every one
// of them is required (absent values are null, never missing). This catches
// a Go field the schema doesn't know, which an open schema would accept.
func TestDataFieldsMatchGo(t *testing.T) {
	t.Parallel()

	for typ, ev := range full() {
		var s struct {
			Defs struct {
				Data struct {
					Properties map[string]json.RawMessage `json:"properties"`
					Required   []string                   `json:"required"`
				} `json:"data"`
			} `json:"$defs"`
		}
		readJSON(t, filepath.Join(schemaDir, string(typ)+".schema.json"), &s)
		props := slices.Sorted(maps.Keys(s.Defs.Data.Properties))
		req := slices.Sorted(slices.Values(s.Defs.Data.Required))

		var fields []string
		rt := reflect.TypeOf(ev.Data)
		for i := range rt.NumField() {
			tag := rt.Field(i).Tag.Get("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				t.Errorf("%s: field %s has no JSON name", typ, rt.Field(i).Name)
			}
			if strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") {
				t.Errorf("%s: field %s is omitted when empty; v1 sends null instead", typ, rt.Field(i).Name)
			}
			fields = append(fields, name)
		}
		slices.Sort(fields)
		if !slices.Equal(props, fields) {
			t.Errorf("%s: schema data properties %v, Go fields %v", typ, props, fields)
		}
		if !slices.Equal(req, fields) {
			t.Errorf("%s: schema data required %v, want every field %v", typ, req, fields)
		}
	}
}

// Acceptance 2 and the other enums: each schema enum is exactly the OCPP 1.6
// value set (OCPP 1.6 edition 2, section 7, and its JSON schemas), and the
// Go constants are exactly that set too.
func TestEnumsAreFullOCPP(t *testing.T) {
	t.Parallel()

	strs := func(vs ...any) []string {
		out := []string{}
		for _, v := range vs {
			out = append(out, reflect.ValueOf(v).String())
		}
		slices.Sort(out)
		return out
	}
	cases := []struct {
		schema, field string
		ocpp          []string
		gov           []string
	}{
		{
			"connector.status", "status",
			[]string{"Available", "Preparing", "Charging", "SuspendedEVSE", "SuspendedEV", "Finishing", "Reserved", "Unavailable", "Faulted"},
			strs(StatusAvailable, StatusPreparing, StatusCharging, StatusSuspendedEVSE, StatusSuspendedEV, StatusFinishing, StatusReserved, StatusUnavailable, StatusFaulted),
		},
		{
			"connector.status", "error_code",
			[]string{"ConnectorLockFailure", "EVCommunicationError", "GroundFailure", "HighTemperature", "InternalError", "LocalListConflict", "NoError", "OtherError", "OverCurrentFailure", "PowerMeterFailure", "PowerSwitchFailure", "ReaderFailure", "ResetFailure", "UnderVoltage", "OverVoltage", "WeakSignal"},
			strs(ErrorConnectorLockFailure, ErrorEVCommunicationError, ErrorGroundFailure, ErrorHighTemperature, ErrorInternalError, ErrorLocalListConflict, ErrorNoError, ErrorOtherError, ErrorOverCurrentFailure, ErrorPowerMeterFailure, ErrorPowerSwitchFailure, ErrorReaderFailure, ErrorResetFailure, ErrorUnderVoltage, ErrorOverVoltage, ErrorWeakSignal),
		},
		{
			"charger.booted", "registration_status",
			[]string{"Accepted", "Pending", "Rejected"},
			strs(RegistrationAccepted, RegistrationPending, RegistrationRejected),
		},
		{
			"meter.sample", "context",
			[]string{"Interruption.Begin", "Interruption.End", "Sample.Clock", "Sample.Periodic", "Transaction.Begin", "Transaction.End", "Trigger", "Other"},
			strs(ContextInterruptionBegin, ContextInterruptionEnd, ContextSampleClock, ContextSamplePeriodic, ContextTransactionBegin, ContextTransactionEnd, ContextTrigger, ContextOther),
		},
		{
			"transaction.started", "authorization_status",
			[]string{"Accepted", "Blocked", "Expired", "Invalid", "ConcurrentTx"},
			strs(AuthorizationAccepted, AuthorizationBlocked, AuthorizationExpired, AuthorizationInvalid, AuthorizationConcurrentTx),
		},
		{
			"transaction.stopped", "reason",
			[]string{"EmergencyStop", "EVDisconnected", "HardReset", "Local", "Other", "PowerLoss", "Reboot", "Remote", "SoftReset", "UnlockCommand", "DeAuthorized"},
			strs(ReasonEmergencyStop, ReasonEVDisconnected, ReasonHardReset, ReasonLocal, ReasonOther, ReasonPowerLoss, ReasonReboot, ReasonRemote, ReasonSoftReset, ReasonUnlockCommand, ReasonDeAuthorized),
		},
	}
	for _, c := range cases {
		var s struct {
			Defs struct {
				Data struct {
					Properties map[string]struct {
						Enum []any `json:"enum"`
					} `json:"properties"`
				} `json:"data"`
			} `json:"$defs"`
		}
		readJSON(t, filepath.Join(schemaDir, c.schema+".schema.json"), &s)
		var got []string
		for _, v := range s.Defs.Data.Properties[c.field].Enum {
			if v == nil {
				continue // nullable enums list null; it is not an OCPP value
			}
			got = append(got, v.(string))
		}
		slices.Sort(got)
		slices.Sort(c.ocpp)
		if !slices.Equal(got, c.ocpp) {
			t.Errorf("%s %s: schema enum %v, OCPP 1.6 has %v", c.schema, c.field, got, c.ocpp)
		}
		if !slices.Equal(c.gov, c.ocpp) {
			t.Errorf("%s %s: Go constants %v, OCPP 1.6 has %v", c.schema, c.field, c.gov, c.ocpp)
		}
	}
}

// Each schema rejects documents that are wrong in a way a consumer would
// care about: a collapsed status, a missing charger_id, a wrong type, and so
// on. Each mutation starts from the valid example.
func TestSchemasRejectWrongDocuments(t *testing.T) {
	t.Parallel()

	_, schemas := compileSchemas(t)
	type mutation struct {
		name string
		edit func(m map[string]any)
	}
	data := func(m map[string]any) map[string]any { return m["data"].(map[string]any) }
	// Wrong in every event type.
	common := []mutation{
		{"missing charger_id", func(m map[string]any) { delete(m, "charger_id") }},
		{"empty charger_id", func(m map[string]any) { m["charger_id"] = "" }},
		{"missing connector_id", func(m map[string]any) { delete(m, "connector_id") }},
		{"negative connector_id", func(m map[string]any) { m["connector_id"] = -1 }},
		{"missing timestamp", func(m map[string]any) { delete(m, "timestamp") }},
		{"missing received_at", func(m map[string]any) { delete(m, "received_at") }},
		{"received_at not UTC", func(m map[string]any) { m["received_at"] = "2026-10-23T11:15:02.123+02:00" }},
		{"received_at not a date-time", func(m map[string]any) { m["received_at"] = "yesterday" }},
		{"schema_version 2", func(m map[string]any) { m["schema_version"] = "2" }},
		{"id not a UUIDv7", func(m map[string]any) { m["id"] = "4f3c2a8e-1b2d-4c5e-8f90-123456789abc" }},
		{"missing data", func(m map[string]any) { delete(m, "data") }},
		{"data not an object", func(m map[string]any) { m["data"] = "x" }},
	}
	perType := map[Type][]mutation{
		TypeChargerConnected: {
			{"wrong type", func(m map[string]any) { m["type"] = "charger.disconnected" }},
			{"connector_id not 0", func(m map[string]any) { m["connector_id"] = 1 }},
			{"timestamp set", func(m map[string]any) { m["timestamp"] = "2026-10-23T09:15:00Z" }},
			{"missing subprotocol", func(m map[string]any) { delete(data(m), "subprotocol") }},
		},
		TypeChargerDisconnected: {
			{"wrong type", func(m map[string]any) { m["type"] = "charger.connected" }},
			{"connector_id not 0", func(m map[string]any) { m["connector_id"] = 2 }},
			{"missing reason", func(m map[string]any) { delete(data(m), "reason") }},
		},
		TypeChargerBooted: {
			{"wrong type", func(m map[string]any) { m["type"] = "charger.boot" }},
			{"connector_id not 0", func(m map[string]any) { m["connector_id"] = 1 }},
			{"unknown registration_status", func(m map[string]any) { data(m)["registration_status"] = "Unknown" }},
			{"missing vendor", func(m map[string]any) { delete(data(m), "vendor") }},
			{"missing serial_number", func(m map[string]any) { delete(data(m), "serial_number") }},
			{"negative heartbeat", func(m map[string]any) { data(m)["heartbeat_interval_s"] = -1 }},
			{"fractional heartbeat", func(m map[string]any) { data(m)["heartbeat_interval_s"] = 1.5 }},
		},
		TypeConnectorStatus: {
			{"wrong type", func(m map[string]any) { m["type"] = "meter.sample" }},
			{"collapsed status Occupied", func(m map[string]any) { data(m)["status"] = "Occupied" }},
			{"collapsed status lowercase", func(m map[string]any) { data(m)["status"] = "charging" }},
			{"missing status", func(m map[string]any) { delete(data(m), "status") }},
			{"unknown error_code", func(m map[string]any) { data(m)["error_code"] = "Overheated" }},
			{"null connector_id", func(m map[string]any) { m["connector_id"] = nil }},
			{"missing info", func(m map[string]any) { delete(data(m), "info") }},
		},
		TypeMeterSample: {
			{"wrong type", func(m map[string]any) { m["type"] = "connector.status" }},
			{"energy as string", func(m map[string]any) { data(m)["energy_wh"] = "123456.5" }},
			{"missing power_w", func(m map[string]any) { delete(data(m), "power_w") }},
			{"unknown context", func(m map[string]any) { data(m)["context"] = "Sample" }},
			{"null timestamp", func(m map[string]any) { m["timestamp"] = nil }},
			{"fractional transaction_id", func(m map[string]any) { data(m)["transaction_id"] = 1.5 }},
		},
		TypeTransactionStarted: {
			{"wrong type", func(m map[string]any) { m["type"] = "transaction.stopped" }},
			{"missing id_tag", func(m map[string]any) { delete(data(m), "id_tag") }},
			{"null transaction_id", func(m map[string]any) { data(m)["transaction_id"] = nil }},
			{"unknown authorization_status", func(m map[string]any) { data(m)["authorization_status"] = "Ok" }},
			{"null timestamp", func(m map[string]any) { m["timestamp"] = nil }},
			{"null connector_id", func(m map[string]any) { m["connector_id"] = nil }},
		},
		TypeTransactionStopped: {
			{"wrong type", func(m map[string]any) { m["type"] = "transaction.started" }},
			{"null transaction_id", func(m map[string]any) { data(m)["transaction_id"] = nil }},
			{"unknown reason", func(m map[string]any) { data(m)["reason"] = "Unplugged" }},
			{"missing meter_stop_wh", func(m map[string]any) { delete(data(m), "meter_stop_wh") }},
			{"null timestamp", func(m map[string]any) { m["timestamp"] = nil }},
		},
	}
	for _, typ := range allTypes {
		for _, mu := range append(slices.Clone(common), perType[typ]...) {
			var m map[string]any
			if err := json.Unmarshal(readExample(t, typ), &m); err != nil {
				t.Fatal(err)
			}
			mu.edit(m)
			doc, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(t, schemas[typ], doc); err == nil {
				t.Errorf("%s schema accepted %s:\n%s", typ, mu.name, doc)
			}
		}
	}
}

// The schema validator is a test-only dependency: neither the events
// package nor the octap binary links it.
func TestValidatorIsTestOnly(t *testing.T) {
	t.Parallel()

	for _, pkg := range []string{".", "../../cmd/octap"} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "github.com/santhosh-tekuri/") || strings.HasPrefix(dep, "golang.org/x/") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
