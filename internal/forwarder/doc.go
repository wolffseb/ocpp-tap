// Package forwarder will pass OCPP-J frames unchanged between a charger and
// its CPMS. It is the only inline part of ocpp-tap.
//
// It is the smallest, most boring part of the system: it parses nothing
// beyond what correlation needs, fails closed when upstream is gone, and
// imports nothing from the tap, API, or UI packages (enforced by
// golangci-lint's depguard rule). Not implemented yet; see SP1 in
// docs/PRD.md and docs/adr/0001-architecture.md.
package forwarder
