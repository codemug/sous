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

func (a Allocator) Free(host string) (int, error) {
	for p := a.Low; p <= a.High; p++ {
		if a.IsFree(host, p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("ports: no free port in %d-%d on %s", a.Low, a.High, host)
}
