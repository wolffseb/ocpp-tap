package events

import (
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the schema_version every event carries. v1 never changes
// incompatibly; a breaking change is version "2" (docs/events/v1/README.md).
const SchemaVersion = "1"

// Type is an event type, the envelope's "type".
type Type string

// The v1 event types.
const (
	TypeChargerConnected    Type = "charger.connected"
	TypeChargerDisconnected Type = "charger.disconnected"
	TypeChargerBooted       Type = "charger.booted"
	TypeConnectorStatus     Type = "connector.status"
	TypeMeterSample         Type = "meter.sample"
	TypeTransactionStarted  Type = "transaction.started"
	TypeTransactionStopped  Type = "transaction.stopped"
)

// Data is an event's type-specific payload: one of the payload types in
// this package.
type Data interface {
	eventType() Type
}

// Event is one v1 event: the envelope plus its payload. Build it with the
// New* constructor for its type, which sets Type, ID and the fields the type
// fixes (connector 0 and no timestamp for charger-level events).
type Event struct {
	// ID is a UUIDv7 (NewID).
	ID string
	// Type matches Data; MarshalJSON refuses a mismatch.
	Type      Type
	ChargerID string
	// ConnectorID is the OCPP connector id, 0 for the charge point as a
	// whole. nil only for a transaction.stopped whose connector is unknown.
	ConnectorID *int
	// Timestamp is the time the charger put in the frame (ParseFrameTime: a
	// time without a zone is UTC); nil when the frame carries none.
	Timestamp *time.Time
	// ReceivedAt is when octap read the frame.
	ReceivedAt time.Time
	Data       Data
}

// MarshalJSON writes the v1 wire format: snake_case keys, times in UTC with
// up to nanosecond precision, absent values as null.
func (e Event) MarshalJSON() ([]byte, error) {
	if e.Data == nil {
		return nil, fmt.Errorf("events: %s event without data", e.Type)
	}
	if got := e.Data.eventType(); got != e.Type {
		return nil, fmt.Errorf("events: %s event carries %s data", e.Type, got)
	}
	var ts *string
	if e.Timestamp != nil {
		s := formatTime(*e.Timestamp)
		ts = &s
	}
	return json.Marshal(struct {
		SchemaVersion string  `json:"schema_version"`
		ID            string  `json:"id"`
		Type          Type    `json:"type"`
		ChargerID     string  `json:"charger_id"`
		ConnectorID   *int    `json:"connector_id"`
		Timestamp     *string `json:"timestamp"`
		ReceivedAt    string  `json:"received_at"`
		Data          Data    `json:"data"`
	}{SchemaVersion, e.ID, e.Type, e.ChargerID, e.ConnectorID, ts, formatTime(e.ReceivedAt), e.Data})
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func newEvent(typ Type, chargerID string, connectorID *int, ts *time.Time, receivedAt time.Time, d Data) Event {
	return Event{
		ID:          NewID(),
		Type:        typ,
		ChargerID:   chargerID,
		ConnectorID: connectorID,
		Timestamp:   ts,
		ReceivedAt:  receivedAt,
		Data:        d,
	}
}

// chargePoint is connector 0, the charge point as a whole. Each event gets
// its own copy so callers can't alias one another's.
func chargePoint() *int {
	zero := 0
	return &zero
}

// ChargerConnected is the payload of charger.connected.
type ChargerConnected struct {
	// Subprotocol is the WebSocket subprotocol both sides agreed on.
	Subprotocol string `json:"subprotocol"`
}

func (ChargerConnected) eventType() Type { return TypeChargerConnected }

// NewChargerConnected builds a charger.connected event (connector 0, no
// frame timestamp).
func NewChargerConnected(chargerID string, receivedAt time.Time, d ChargerConnected) Event {
	return newEvent(TypeChargerConnected, chargerID, chargePoint(), nil, receivedAt, d)
}

// ChargerDisconnected is the payload of charger.disconnected.
type ChargerDisconnected struct {
	// Reason is free text for people, not for branching on.
	Reason string `json:"reason"`
}

func (ChargerDisconnected) eventType() Type { return TypeChargerDisconnected }

// NewChargerDisconnected builds a charger.disconnected event (connector 0,
// no frame timestamp).
func NewChargerDisconnected(chargerID string, receivedAt time.Time, d ChargerDisconnected) Event {
	return newEvent(TypeChargerDisconnected, chargerID, chargePoint(), nil, receivedAt, d)
}

// RegistrationStatus is OCPP 1.6 RegistrationStatus (BootNotification.conf).
type RegistrationStatus string

// The OCPP 1.6 RegistrationStatus values.
const (
	RegistrationAccepted RegistrationStatus = "Accepted"
	RegistrationPending  RegistrationStatus = "Pending"
	RegistrationRejected RegistrationStatus = "Rejected"
)

// ChargerBooted is the payload of charger.booted: a BootNotification and the
// CPMS's answer.
type ChargerBooted struct {
	Vendor             string             `json:"vendor"`
	Model              string             `json:"model"`
	SerialNumber       *string            `json:"serial_number"`
	FirmwareVersion    *string            `json:"firmware_version"`
	RegistrationStatus RegistrationStatus `json:"registration_status"`
	// IntervalS is BootNotification.conf's interval, in seconds. With
	// RegistrationAccepted it is the heartbeat interval; otherwise it is the
	// minimum wait before the charger's next BootNotification, and 0 lets the
	// charger choose (OCPP 1.6 section 4.2).
	IntervalS int `json:"interval_s"`
}

func (ChargerBooted) eventType() Type { return TypeChargerBooted }

// NewChargerBooted builds a charger.booted event (connector 0;
// BootNotification carries no timestamp).
func NewChargerBooted(chargerID string, receivedAt time.Time, d ChargerBooted) Event {
	return newEvent(TypeChargerBooted, chargerID, chargePoint(), nil, receivedAt, d)
}

// ChargePointStatus is OCPP 1.6 ChargePointStatus, never collapsed.
type ChargePointStatus string

// The nine OCPP 1.6 ChargePointStatus values.
const (
	StatusAvailable     ChargePointStatus = "Available"
	StatusPreparing     ChargePointStatus = "Preparing"
	StatusCharging      ChargePointStatus = "Charging"
	StatusSuspendedEVSE ChargePointStatus = "SuspendedEVSE"
	StatusSuspendedEV   ChargePointStatus = "SuspendedEV"
	StatusFinishing     ChargePointStatus = "Finishing"
	StatusReserved      ChargePointStatus = "Reserved"
	StatusUnavailable   ChargePointStatus = "Unavailable"
	StatusFaulted       ChargePointStatus = "Faulted"
)

// ChargePointErrorCode is OCPP 1.6 ChargePointErrorCode.
type ChargePointErrorCode string

// The sixteen OCPP 1.6 ChargePointErrorCode values.
const (
	ErrorConnectorLockFailure ChargePointErrorCode = "ConnectorLockFailure"
	ErrorEVCommunicationError ChargePointErrorCode = "EVCommunicationError"
	ErrorGroundFailure        ChargePointErrorCode = "GroundFailure"
	ErrorHighTemperature      ChargePointErrorCode = "HighTemperature"
	ErrorInternalError        ChargePointErrorCode = "InternalError"
	ErrorLocalListConflict    ChargePointErrorCode = "LocalListConflict"
	ErrorNoError              ChargePointErrorCode = "NoError"
	ErrorOtherError           ChargePointErrorCode = "OtherError"
	ErrorOverCurrentFailure   ChargePointErrorCode = "OverCurrentFailure"
	ErrorPowerMeterFailure    ChargePointErrorCode = "PowerMeterFailure"
	ErrorPowerSwitchFailure   ChargePointErrorCode = "PowerSwitchFailure"
	ErrorReaderFailure        ChargePointErrorCode = "ReaderFailure"
	ErrorResetFailure         ChargePointErrorCode = "ResetFailure"
	ErrorUnderVoltage         ChargePointErrorCode = "UnderVoltage"
	ErrorOverVoltage          ChargePointErrorCode = "OverVoltage"
	ErrorWeakSignal           ChargePointErrorCode = "WeakSignal"
)

// ConnectorStatus is the payload of connector.status: a StatusNotification.
type ConnectorStatus struct {
	Status          ChargePointStatus    `json:"status"`
	ErrorCode       ChargePointErrorCode `json:"error_code"`
	Info            *string              `json:"info"`
	VendorID        *string              `json:"vendor_id"`
	VendorErrorCode *string              `json:"vendor_error_code"`
}

func (ConnectorStatus) eventType() Type { return TypeConnectorStatus }

// NewConnectorStatus builds a connector.status event. timestamp is nil when
// the StatusNotification has none (it is optional in OCPP 1.6).
func NewConnectorStatus(chargerID string, connectorID int, timestamp *time.Time, receivedAt time.Time, d ConnectorStatus) Event {
	return newEvent(TypeConnectorStatus, chargerID, &connectorID, timestamp, receivedAt, d)
}

// ReadingContext is OCPP 1.6 ReadingContext.
type ReadingContext string

// The OCPP 1.6 ReadingContext values.
const (
	ContextInterruptionBegin ReadingContext = "Interruption.Begin"
	ContextInterruptionEnd   ReadingContext = "Interruption.End"
	ContextSampleClock       ReadingContext = "Sample.Clock"
	ContextSamplePeriodic    ReadingContext = "Sample.Periodic"
	ContextTransactionBegin  ReadingContext = "Transaction.Begin"
	ContextTransactionEnd    ReadingContext = "Transaction.End"
	ContextTrigger           ReadingContext = "Trigger"
	ContextOther             ReadingContext = "Other"
)

// MeterSample is the payload of meter.sample: one MeterValue element, units
// normalised. Each value is nil when the element doesn't carry it.
// SelectMeterSample applies the README's selection rules (M1-M7).
type MeterSample struct {
	TransactionID *int            `json:"transaction_id"`
	EnergyWh      *float64        `json:"energy_wh"`
	PowerW        *float64        `json:"power_w"`
	CurrentA      *float64        `json:"current_a"`
	SoCPercent    *float64        `json:"soc_percent"`
	Context       *ReadingContext `json:"context"`
}

func (MeterSample) eventType() Type { return TypeMeterSample }

// NewMeterSample builds a meter.sample event; timestamp is the MeterValue
// element's.
func NewMeterSample(chargerID string, connectorID int, timestamp time.Time, receivedAt time.Time, d MeterSample) Event {
	return newEvent(TypeMeterSample, chargerID, &connectorID, &timestamp, receivedAt, d)
}

// AuthorizationStatus is OCPP 1.6 AuthorizationStatus (idTagInfo.status).
type AuthorizationStatus string

// The OCPP 1.6 AuthorizationStatus values.
const (
	AuthorizationAccepted     AuthorizationStatus = "Accepted"
	AuthorizationBlocked      AuthorizationStatus = "Blocked"
	AuthorizationExpired      AuthorizationStatus = "Expired"
	AuthorizationInvalid      AuthorizationStatus = "Invalid"
	AuthorizationConcurrentTx AuthorizationStatus = "ConcurrentTx"
)

// TransactionStarted is the payload of transaction.started: a
// StartTransaction and the CPMS's answer.
type TransactionStarted struct {
	TransactionID int `json:"transaction_id"`
	// IDTag is personal data (usually an RFID card UID).
	IDTag               string              `json:"id_tag"`
	MeterStartWh        int                 `json:"meter_start_wh"`
	ReservationID       *int                `json:"reservation_id"`
	AuthorizationStatus AuthorizationStatus `json:"authorization_status"`
}

func (TransactionStarted) eventType() Type { return TypeTransactionStarted }

// NewTransactionStarted builds a transaction.started event; timestamp is
// StartTransaction.req's.
func NewTransactionStarted(chargerID string, connectorID int, timestamp time.Time, receivedAt time.Time, d TransactionStarted) Event {
	return newEvent(TypeTransactionStarted, chargerID, &connectorID, &timestamp, receivedAt, d)
}

// StopReason is OCPP 1.6 Reason (StopTransaction.req).
type StopReason string

// The OCPP 1.6 Reason values.
const (
	ReasonEmergencyStop  StopReason = "EmergencyStop"
	ReasonEVDisconnected StopReason = "EVDisconnected"
	ReasonHardReset      StopReason = "HardReset"
	ReasonLocal          StopReason = "Local"
	ReasonOther          StopReason = "Other"
	ReasonPowerLoss      StopReason = "PowerLoss"
	ReasonReboot         StopReason = "Reboot"
	ReasonRemote         StopReason = "Remote"
	ReasonSoftReset      StopReason = "SoftReset"
	ReasonUnlockCommand  StopReason = "UnlockCommand"
	ReasonDeAuthorized   StopReason = "DeAuthorized"
)

// TransactionStopped is the payload of transaction.stopped.
type TransactionStopped struct {
	TransactionID int `json:"transaction_id"`
	MeterStopWh   int `json:"meter_stop_wh"`
	// Reason is nil when the charger left it out, which OCPP 1.6 defines as
	// Local; v1 reports what the frame says.
	Reason *StopReason `json:"reason"`
	// IDTag is personal data (usually an RFID card UID).
	IDTag *string `json:"id_tag"`
}

func (TransactionStopped) eventType() Type { return TypeTransactionStopped }

// NewTransactionStopped builds a transaction.stopped event. StopTransaction
// carries no connector id: connectorID comes from the matching start, and is
// nil when octap didn't see it. timestamp is StopTransaction.req's.
func NewTransactionStopped(chargerID string, connectorID *int, timestamp time.Time, receivedAt time.Time, d TransactionStopped) Event {
	if connectorID != nil {
		c := *connectorID
		connectorID = &c
	}
	return newEvent(TypeTransactionStopped, chargerID, connectorID, &timestamp, receivedAt, d)
}
