// Package tap reads a copy of the forwarded traffic.
//
// It is read-only and lossy by design (Principle 3, ADR-0001 §3): it never
// writes to a connection, and when it falls behind it drops frames and
// counts them rather than slowing the forwarder. A Tap is the forwarder's
// FrameSink; it hands each frame to a handler in its own goroutine. Turning
// frames into events is a later SP2 issue.
package tap
