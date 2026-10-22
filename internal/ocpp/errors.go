// Copied from github.com/wolffseb/headless-csms at 603c2d112c736ff98e44844a57fb63dbda7096e3
// (excerpt of internal/ocpp/chargepoint.go) and modified since; see NOTICE.
// Copyright (c) 2026 Sebastian Wolff. Licensed under the MIT License (NOTICE).

package ocpp

import "errors"

// Errors a call can fail with, matched with errors.Is.
var (
	// ErrTimeout means the peer did not answer within the call timeout.
	ErrTimeout = errors.New("ocpp: call timed out")
	// ErrNotConnected means the peer is not connected, or its connection
	// closed while the call was in flight.
	ErrNotConnected = errors.New("ocpp: charge point not connected")
)
