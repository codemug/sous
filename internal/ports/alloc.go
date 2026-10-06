// Package ports allocates host ports at deploy time.
//
// Availability is decided by ACTUALLY BINDING, not by consulting Sous's own
// records. A process outside Sous can hold a port - k3s Traefik held 443 on
// every node IP in this fleet, silently - and a self-referential check cannot
// see that. The cost of being wrong is a model that loads for six minutes and
// then fails to bind.
package ports

import (
	"fmt"
	"net"
	"strconv"
)

type Allocator struct{ Low, High int }

// IsFree binds and immediately releases. There is an unavoidable race between
// this check and the container starting; it is narrowed by re-checking
// immediately before start, and a lost race surfaces as a clear bind error
// rather than as silent misbehaviour.
func (a Allocator) IsFree(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

func (a Allocator) Free(host string) (int, error) { return a.FreeExcept(host, nil) }

// FreeExcept is Free that also skips every port in taken.
//
// Binding is still the test - a port not in taken but held by a foreign
// process is skipped exactly as before - but binding cannot see a port that
// is SPOKEN FOR without being bound: a model container that is stopped, or
// that Docker is between restarts of, holds no socket, yet takes its port back
// the moment Docker starts it again. Only the caller (which asked Docker) knows
// those; handing one out would leave two containers wanting one port.
func (a Allocator) FreeExcept(host string, taken map[int]bool) (int, error) {
	for p := a.Low; p <= a.High; p++ {
		if !taken[p] && a.IsFree(host, p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("ports: no free port in %d-%d on %s", a.Low, a.High, host)
}
