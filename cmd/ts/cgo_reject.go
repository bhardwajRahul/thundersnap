// This file rejects CGO at compile time for the ts binary.
//
// RATIONALE: The ts binary runs inside containers and VMs where dynamically
// linked binaries may not work (missing libc, different glibc version, etc.).
// By enforcing CGO_ENABLED=0, we ensure the binary is fully static and portable.
//
// HOW TO BUILD AND TEST:
//   make all    - builds all binaries with CGO_ENABLED=0
//   make test   - runs all tests with CGO_ENABLED=0
//   make ts     - builds just the ts binary
//
// If you run "go test ./..." or "go build ./cmd/ts" directly without
// CGO_ENABLED=0, you'll see this error. Use the Makefile targets instead.
//
//go:build cgo

package main

/*
#error "cgo is not allowed in this package; build with CGO_ENABLED=0"
*/
import "C"
