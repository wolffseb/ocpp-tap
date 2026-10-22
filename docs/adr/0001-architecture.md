# ADR-0001: Architecture — forwarder/tap split, fail closed, tap copies, trial config

- Status: **Accepted** (2026-10-22, Sebastian: merged in wolffseb/ocpp-tap#1, "Let's do SP1")
- Date: 2026-10-21
- Deciders: Sebastian (human gate "Architecture ADR", before SP1)
- Related: [PRD](../PRD.md) § Principles, § Architecture, SP1, SP2, SP4

This ADR gated SP1.

## Context

ocpp-tap sits at the URL a charger dials. In OCPP 1.6 the charger has no standard fallback, so the proxy is a single point of failure by construction. The PRD's principles say how to contain that: only a minimal forwarder is inline, failure upstream drops the charger so it goes to its own offline mode, and everything else reads a lossy copy. This ADR turns those principles into concrete structure that SP1–SP4 build against.

## Decision

### 1. Forwarder/tap split

- `internal/forwarder` is the only package on the frame path. Per charger connection it holds one downstream WebSocket (charger) and one upstream WebSocket (CPMS) and copies each message payload from one to the other unchanged, in order.
- It parses only the OCPP-J envelope (message type and message ID), and only if correlation needs it (for example, to log which CALL a CALLERROR answered). It never decodes an action's payload, never answers a call itself, and never generates a message ID.
- Its one outward hook is an interface defined **in the forwarder package**:

  ```go
  // FrameSink receives a copy of every forwarded frame. Offer must not block.
  type FrameSink interface {
      Offer(Frame) // Frame: charger ID, direction, receive time, payload bytes
  }
  ```

  The tap implements `FrameSink`. The dependency arrow therefore points tap → forwarder, never forwarder → tap. A depguard rule in `.golangci.yml` forbids `internal/forwarder` from importing `internal/tap`, `internal/api`, `internal/ui` and `harness`, and `make ci` fails on violation.
- The binary wires them together in `cmd/octap`. In production mode (SP10) a forwarder-only instance is started with a nil-safe no-op sink.

### 2. Fail-closed policy

- **Upstream unreachable at charger connect:** the forwarder dials upstream *before* completing the charger's WebSocket upgrade. If the dial fails (refused, TLS error, HTTP rejection, timeout), the charger's upgrade is answered with HTTP 502 and no subprotocol, and the attempt is logged. The charger then retries on its own schedule and runs on its offline behaviour meanwhile. The charger is never accepted while there is nothing behind it.
- **Upstream lost mid-session:** when the upstream socket closes or errors, the charger socket is closed within 2 s (the SP1 target) with close code 1011 (or 1001 on shutdown). Symmetrically, a charger disconnect closes the upstream socket.
- **No buffering on the charger's behalf:** no queueing of frames across reconnects, no retries, no synthetic responses. One upstream connection per charger connection; a charger reconnect produces a new upstream dial.
- **Subprotocol:** the forwarder offers upstream exactly the subprotocols the charger offered and accepts the charger with the one upstream chose. If upstream chooses none, the charger is refused with a log line naming both sides (SP1).
- **Liveness:** WebSocket ping/pong keeps both legs honest at the transport level. The forwarder has no OCPP-level watchdog of its own; that is the CPMS's job.

### 3. How the tap receives copies

- The tap's `Offer` writes to a **bounded buffered channel** with a non-blocking send:

  ```go
  select {
  case t.frames <- f:
  default:
      t.dropped.Add(1) // exported as tap_events_dropped
  }
  ```

  A full channel costs the forwarder one failed `select`, never a wait. Proposed default capacity: 4096 frames, configurable.
- The frame payload handed to `Offer` is treated as immutable by both sides. The forwarder gives the tap the same byte slice it wrote, since the WebSocket library returns a fresh slice per read, so there is no extra copy on the hot path.
- The tap consumes the channel in its own goroutine(s), each wrapped in `recover`. A panic is logged and counted, and the consumer restarts. Panics never propagate into forwarder goroutines, because the forwarder never calls into tap code beyond `Offer`, which is itself guarded by `recover`.
- Acceptance comes from SP2: with the tap blocked for 60 s, forwarder latency stays within 10% of baseline and `tap_events_dropped` increases.

### 4. Config storage for trial mode

- One JSON file on the mounted volume, `/data/config.json`, file mode 0600, directory 0700. The PRD allows "one file or embedded database". A single file has no cgo, no migration tooling, and is readable by an admin in an emergency.
- Writes are atomic: write a temp file in the same directory, fsync it, rename it over the old file, then fsync the directory. This is the same pattern as `headless-csms` `internal/state/store.go`. A schema `version` field fails closed on a file from a newer octap.
- The forwarder never reads the file. It depends on a small read-only interface defined in the forwarder package (`ChargerConfig(id) (Config, bool)` plus a per-charger change notification). The config package implements it. A changed charger config closes and re-dials only that charger's connection (SP4).
- CPMS passwords are stored in this file in plain text, protected only by file mode and the volume. They are never returned by the API (SP4). Encryption at rest is not proposed here, because the key would have to live on the same volume.

## Out of scope / still open

Not decided by this ADR, per PRD § Open decisions:
- Config storage for production mode with several forwarder instances (SP10).
- Tap history and retention (live state only vs. ring buffer).
- Whether the proxy may send its own GetConfiguration for the offline-readiness check. This ADR's forwarder never originates a message. If that decision is "yes", it needs an amendment to Principle 1 and to this ADR.
- OCPP 2.0.1 timing.

## Consequences

- The forwarder can be reviewed in isolation: one package, two sockets, two copy loops, one non-blocking hook.
- Tap bugs and slowness cannot stop charging, but the tap can miss events. Consumers must treat the event stream as lossy, and the drop counter makes loss visible.
- Fail-closed means an upstream outage is visible to the charger immediately, which is intended: the charger's offline mode is the safety net, and the SP5 offline-readiness check tells the admin whether that net exists.
- Trial config is one file. Production mode needs a separate decision before SP10.
