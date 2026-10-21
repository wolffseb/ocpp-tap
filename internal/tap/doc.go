// Package tap will turn a copy of the forwarded traffic into events.
//
// It is read-only and lossy by design: it never writes to a connection, and
// when it falls behind it drops frames and counts them rather than slowing
// the forwarder. Not implemented yet; see SP2 in docs/PRD.md.
package tap
