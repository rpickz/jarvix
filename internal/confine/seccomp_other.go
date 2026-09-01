//go:build !linux || (!amd64 && !arm64)

package confine

import (
	"errors"
	"runtime"
)

// On a machine this package has no seccomp filter for, a command does not run.
//
// That is the same rule as everywhere else here and it is worth being explicit
// about rather than leaving to a build failure: the filter's whole job is to
// stop a confined command reaching Jarvix's own socket and reconfiguring the
// thing confining it, so a machine where it cannot be installed is a machine
// where the boundary is not the one being claimed. Refusing is the only
// direction that keeps the claim true.

// denyUnixSockets refuses rather than silently omitting the second wall.
func denyUnixSockets() error {
	return errors.New("I cannot stop a command reaching my own socket on " + runtime.GOARCH +
		", and I will not run one that could reconfigure me")
}
