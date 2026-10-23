# Event schema v1

octap's tap turns a copy of the OCPP 1.6-J traffic into events. This directory is the published contract for those events: one [JSON Schema (draft 2020-12)](https://json-schema.org/draft/2020-12) per event type, the envelope they share, and one example per type.

| Event type | Schema | Example | From |
| --- | --- | --- | --- |
| (all) | [`envelope.schema.json`](envelope.schema.json) | | |
| `charger.connected` | [schema](charger.connected.schema.json) | [example](examples/charger.connected.json) | charger and CPMS connections both up |
| `charger.disconnected` | [schema](charger.disconnected.schema.json) | [example](examples/charger.disconnected.json) | the connection ended |
| `charger.booted` | [schema](charger.booted.schema.json) | [example](examples/charger.booted.json) | BootNotification and its answer |
| `connector.status` | [schema](connector.status.schema.json) | [example](examples/connector.status.json) | StatusNotification |
| `meter.sample` | [schema](meter.sample.schema.json) | [example](examples/meter.sample.json) | one MeterValue element of MeterValues |
| `transaction.started` | [schema](transaction.started.schema.json) | [example](examples/transaction.started.json) | StartTransaction and its answer |
| `transaction.stopped` | [schema](transaction.stopped.schema.json) | [example](examples/transaction.stopped.json) | StopTransaction |

The Go types are in [`internal/events`](../../../internal/events); their tests keep the Go types, these schemas and the examples identical.

## Envelope

Every event has these fields, always present:

| Field | Meaning |
| --- | --- |
| `schema_version` | `"1"`. |
| `id` | UUIDv7, lowercase. Unique per event. Within one octap process, a later event has a greater `id`, so `id` sorts events in the order octap made them. |
| `type` | The event type, e.g. `"connector.status"`. |
| `charger_id` | The charge point identity: the last segment of the URL path the charger dialled. |
| `connector_id` | OCPP connector id. `0` is the charge point as a whole (OCPP's convention); charger-level events (`charger.*`) always carry `0`. |
| `timestamp` | The time the charger put in the frame, in UTC. `null` when the frame has none: always for `charger.*`, and for a `connector.status` whose StatusNotification left it out. |
| `received_at` | When octap read the frame (for `charger.connected` and `charger.disconnected`: when it saw the connection change), in UTC, up to nanosecond precision. |
| `data` | The type-specific payload, below. |

All times are RFC 3339 in UTC with a `Z` suffix, e.g. `2026-10-23T09:15:02.123456789Z`. The charger's clock and octap's can disagree: `timestamp` is what the charger claims, `received_at` is what octap observed.

## Payloads

Field names are the OCPP fields in `snake_case`. Enumerated values are exactly the OCPP 1.6 values, never collapsed or renamed.

**`charger.connected`**: `subprotocol`, the WebSocket subprotocol both sides agreed on (e.g. `ocpp1.6`). The charger's address is deliberately not included.

**`charger.disconnected`**: `reason`, free text for people (e.g. `charger left`, `upstream lost`). Don't branch on it.

**`charger.booted`**: made when the CPMS answers a BootNotification.

| Field | From |
| --- | --- |
| `vendor`, `model` | `chargePointVendor`, `chargePointModel` |
| `serial_number` | `chargePointSerialNumber`, or `null` |
| `firmware_version` | `firmwareVersion`, or `null` |
| `registration_status` | the answer's `status`: `Accepted`, `Pending`, `Rejected` |
| `heartbeat_interval_s` | the answer's `interval`, seconds |

**`connector.status`**: one StatusNotification.

| Field | From |
| --- | --- |
| `status` | `status`, one of the nine ChargePointStatus values: `Available`, `Preparing`, `Charging`, `SuspendedEVSE`, `SuspendedEV`, `Finishing`, `Reserved`, `Unavailable`, `Faulted` |
| `error_code` | `errorCode`, one of the sixteen ChargePointErrorCode values (`NoError`, `ConnectorLockFailure`, …) |
| `info`, `vendor_id`, `vendor_error_code` | `info`, `vendorId`, `vendorErrorCode`, or `null` |

**`meter.sample`**: one event per MeterValue element of a MeterValues request; `timestamp` is the element's.

| Field | Measurand | Unit |
| --- | --- | --- |
| `energy_wh` | `Energy.Active.Import.Register` | Wh |
| `power_w` | `Power.Active.Import` | W |
| `current_a` | `Current.Import` | A |
| `soc_percent` | `SoC` | percent |
| `transaction_id` | the request's `transactionId` | |
| `context` | the ReadingContext of the values used (`Sample.Periodic`, `Transaction.Begin`, …) | |

Which sampled values are used:

- Only `Raw` values (`format` absent or `Raw`); `SignedData` is skipped.
- OCPP defaults apply: a sampled value without `measurand` is `Energy.Active.Import.Register`, without `unit` it is Wh (energy) or the measurand's base unit, without `location` it is `Outlet`.
- Energy, power and current come from `Outlet` values. SoC comes from any location (usually `EV`).
- Units are normalised: kWh → Wh, kW → W. Values are numbers, not strings.
- Phases: if an unphased value (no `phase`) is present, it is used. Otherwise energy and power are the **sum** over `L1`, `L2`, `L3`, and current is the **maximum** over `L1`, `L2`, `L3`. `L1-N`, `L2-N`, `L3-N` count as `L1`, `L2`, `L3`; `N` and line-to-line phases are ignored.
- `context` is the context of the first value used; `null` if it has none (OCPP then means `Sample.Periodic`).
- A field is `null` when the element carries no usable value for it. An element with none of the four measurands makes no event.

**`transaction.started`**: made when the CPMS answers a StartTransaction; `timestamp` is the request's.

| Field | From |
| --- | --- |
| `transaction_id` | the answer's `transactionId` |
| `id_tag` | `idTag` (personal data, see below) |
| `meter_start_wh` | `meterStart`, Wh |
| `reservation_id` | `reservationId`, or `null` |
| `authorization_status` | the answer's `idTagInfo.status`: `Accepted`, `Blocked`, `Expired`, `Invalid`, `ConcurrentTx` |

**`transaction.stopped`**: one StopTransaction; `timestamp` is the request's.

| Field | From |
| --- | --- |
| `transaction_id` | `transactionId` |
| `meter_stop_wh` | `meterStop`, Wh |
| `reason` | `reason`, or `null` when the charger left it out (OCPP then means `Local`) |
| `id_tag` | `idTag`, or `null` (personal data, see below) |

StopTransaction has no connector id, so `connector_id` is the one from the matching `transaction.started`. When octap did not see the start (it was restarted mid-transaction, or the event was dropped), `connector_id` is `null`. This is the only event where it can be.

## Null rules

- Every field listed in a schema is always present. A value the frame doesn't carry is `null`, never a missing key, so consumers don't have to tell "missing" from "null".
- `null` means "not in the frame" (or, for `transaction.stopped`'s `connector_id`, "not known to octap"). It never means zero.
- octap reports what the frame says and does not fill in OCPP defaults, apart from the meter.sample rules above.

## `id_tag` is personal data

`id_tag` is usually the UID of a driver's RFID card, so it identifies a person. It is in v1 because matching transactions to cards is a common use, but consumers must treat it as personal data: store it only when needed, restrict access, and delete it with the rest of the driver's data.

## Versioning

- v1 never changes incompatibly. A breaking change (a field removed or renamed, a type changed, an enum value removed, a field made nullable) is a new major version, `"2"`, published next to this one.
- Within v1, octap may add event types and add fields to an event. Consumers must ignore event types and fields they don't know. The schemas are open (no `additionalProperties: false`) so they keep accepting such events.
- An event outside this schema is a bug in octap: a frame whose values don't fit (e.g. a status that isn't one of the nine) makes no event, rather than an event that breaks the schema.
- Changing a published version needs a human-approved ADR (CLAUDE.md, rule 3).

## Validating

Formats (`date-time`) are annotations in draft 2020-12 unless the validator is told to assert them; octap's tests assert them. Load the schemas from files or URLs that keep their relative names: each event schema refers to `envelope.schema.json` by a relative `$ref`.
