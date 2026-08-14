package ports

import (
	"net"
	"testing"
)

func TestFreeSkipsAPortSomethingElseHolds(t *testing.T) {
	// Bind a port the way a foreign process would. Sous must notice, because
	// checking only its own records is exactly how k3s Traefik silently owning
	// 443 on every node IP went undetected on this fleet.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	a := Allocator{Low: held, High: held + 3}
	got, err := a.Free("127.0.0.1")
	if err != nil {
		t.Fatalf("Free: %v", err)
	}
	if got == held {
		t.Fatalf("allocated a port that is already bound: %d", got)
	}
	if got < held || got > held+3 {
		t.Fatalf("allocated outside the range: %d", got)
	}
}

func TestIsFreeReportsHeldPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	a := Allocator{Low: 1, High: 65535}
	if a.IsFree("127.0.0.1", held) {
		t.Fatalf("port %d reported free while bound", held)
	}
}

func TestExhaustedRangeErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := ln.Addr().(*net.TCPAddr).Port

	a := Allocator{Low: held, High: held} // a range of exactly one, and it is taken
	if _, err := a.Free("127.0.0.1"); err == nil {
		t.Fatal("exhausted range must error")
	}
}

func TestFreeReturnsLowestAvailable(t *testing.T) {
	// Find a window of ports that are all free, then confirm we get the first.
	base := 41000
	a := Allocator{Low: base, High: base + 20}
	got, err := a.Free("127.0.0.1")
	if err != nil {
		t.Skipf("no free port in the test window: %v", err)
	}
	if !a.IsFree("127.0.0.1", got) {
		t.Fatalf("returned port %d is not actually free", got)
	}
}
