package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShutdownSignalsIncludeSIGHUP is the worker's half of the platform
// contract described in the API entrypoint's test.
//
// It matters more here than there. The API can lose a hard restart and still
// serve traffic afterwards, because its work is re-dispatched by the sweep
// loop. The worker *is* the consumer: if SIGHUP kills it outright, the shutdown
// path that closes the broker connection cleanly never runs, and every message
// that was prefetched but unacknowledged has to be recovered by the broker
// requeueing it on connection loss rather than by this process settling it.
//
// The outcome is still correct — the messages come back, and the execution
// leases they belonged to are reclaimed by the dispatcher — but it is the
// difference between a controlled stop and an abandoned one.
func TestShutdownSignalsIncludeSIGHUP(t *testing.T) {
	sigs := shutdownSignals()
	require.Contains(t, sigs, os.Signal(syscall.SIGHUP),
		"AlwaysData restarts a Service with SIGHUP; it must begin a graceful shutdown")
	require.Contains(t, sigs, os.Signal(os.Interrupt))
	require.Contains(t, sigs, os.Signal(syscall.SIGTERM))
}
