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
| `timestamp` | The time the charger put in the frame, in UTC. `null` when the frame has none: always for `charger.*`, and for a `connector.status` whose StatusNotification left it out. A charger time with an offset is converted; one with no zone is read as UTC (rule T1, below). |
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
| `interval_s` | the answer's `interval`, seconds. With `Accepted` it is the heartbeat interval. With `Pending` or `Rejected` it is the minimum wait before the charger's next BootNotification, and `0` means the charger chooses (OCPP 1.6 §4.2) |

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

Which sampled values are used. The rules are applied **per measurand** (energy, power, current, SoC), to the `sampledValue` array of one MeterValue element, and are tested by [`conformance.json`](conformance.json) through `events.SelectMeterSample`.

- **M1** Only `Raw` values (`format` absent or `Raw`) are used; `SignedData` is skipped.
- **M2** OCPP defaults apply: no `measurand` means `Energy.Active.Import.Register`, no `location` means `Outlet`, no `unit` on an energy value means `Wh` (OCPP 1.6 §7.43 defines this default for energy only). **No `unit` on a power, current or SoC value is read as `W`, `A` or `Percent`: octap's own assumption, not an OCPP default**, made so that a charger that omits the unit still reports its value. Units are normalised: kWh to Wh, kW to W. A value that is not a finite number, or whose unit the measurand does not take (e.g. `kvar` for power), is skipped as if absent. OCPP sends each `value` as a string; the event's `energy_wh`, `power_w`, `current_a` and `soc_percent` are JSON numbers, parsed from it.
- **M3** Location. For `connector_id` 0, the main meter (OCPP 1.6 §4.7, §3.16.3): use `Inlet` values; if the measurand has none, use `Outlet` values. For `connector_id` above 0: use `Outlet` values only. When one element carries both for a measurand, the preferred one wins and the other is ignored. Other locations (`Body`, `Cable`, `EV`) are ignored for energy, power and current. SoC takes the first value of any location (usually `EV`) and has no phases.
- **M4** Phases. If an unphased value (no `phase`) remains, it is the value and phased values are ignored. Otherwise take, for each of `L1`, `L2`, `L3`, its `Lx` value, or its `Lx-N` value only when there is no `Lx` (so `L1` and `L1-N` are never both counted). Energy and power are the **sum** of the phases found, current the **maximum**. Phases that are absent are skipped: a charger that reports only `L1` and `L2` gets the sum of those two, as is. `N` and line-to-line phases (`L1-L2`, ...) are ignored.
- **M5** Where several values qualify for the same slot (unphased, or the same phase), the **first in `sampledValue` array order** wins; later ones are ignored.
- **M6** `context` is the context of the **first used value in `sampledValue` array order**, over all four measurands. Values that were skipped, shadowed by an unphased value or lost to a duplicate do not count. It is `null` if that value has no `context`, or one that is not a ReadingContext (OCPP then means `Sample.Periodic`).
- **M7** A field is `null` when the element carries no usable value for it. An element with none of the four measurands makes no event.

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

## Timestamps in frames

- **T1** OCPP 1.6 §3.15 (normative) requires implementations to use ISO 8601 and receivers to handle fractional seconds and time zone offsets; §3.14 (informative) recommends UTC. octap accepts exactly this grammar, a subset of ISO 8601 (extended format only):

  ```
  YYYY-MM-DD "T" hh:mm:ss [ ( "." | "," ) digits ] [ "Z" | ("+" | "-") hh [ [":"] mm ] ]
  ```

  That is, the fraction may start with `.` or `,` (ISO 8601 allows both), and the offset may be `Z`, `+hh:mm`, `+hhmm` or `+hh` (the same with `-`), with hours 00-23 and minutes 00-59 (`+24:00` is rejected; there is no further cap on the offset). A time with an offset is converted to UTC. A time **with no zone designator** (`2026-10-23T09:15:00`) is read as UTC, the zone §3.14 recommends, so the event is kept and `timestamp` is never `null` for `meter.sample` and `transaction.*`. A charger that sends local time without a zone therefore reports a shifted `timestamp`; `received_at` is the reference for that. Anything outside the grammar makes no event: a date alone, basic format (`20261023T091500Z`), a space for `T`, a one-digit offset (`+2`), lower-case `t` or `z`. Tested by `events.ParseFrameTime`.

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
