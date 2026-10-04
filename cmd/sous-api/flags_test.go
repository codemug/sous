package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requiredFlags is the least fromFlags accepts. Nothing is opened or bound by
// parsing, so the paths need not exist.
var requiredFlags = []string{
	"-listen", "100.64.0.1:8090", "-grpc-listen", "100.64.0.1:8091",
	"-data", "/tmp/sous-api", "-ca-state", "/tmp/ca.json", "-models", "/models",
}

// Off unless asked for: an install that never set the flag gets no extra
// listener, unauthenticated or otherwise.
func TestMetricsListenIsOffByDefault(t *testing.T) {
	if _, _, _, ml := fromFlags(requiredFlags); ml != "" {
		t.Fatalf("-metrics-listen defaulted to %q, want empty (feature off)", ml)
	}
}

func TestMetricsListenIsTakenWhenSet(t *testing.T) {
	args := append(append([]string(nil), requiredFlags...), "-metrics-listen", "100.64.0.1:9464")
	if _, _, _, ml := fromFlags(args); ml != "100.64.0.1:9464" {
		t.Fatalf("-metrics-listen = %q, want 100.64.0.1:9464", ml)
	}
}

// The metrics listener is unauthenticated, so the network boundary is all it
// has - the same reason -listen and -grpc-listen refuse to bind everything.
// fromFlags exits the process on a bad address, so each case runs it in a
// child process and checks it was refused for the right reason.
func TestMetricsListenRefusesAWildcard(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:9464", "[::]:9464", ":9464", "9464"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFromFlagsInAChildProcess$")
		cmd.Env = append(os.Environ(), "SOUS_TEST_FROMFLAGS_ARGS="+
			strings.Join(append(append([]string(nil), requiredFlags...), "-metrics-listen", addr), "\x1f"))
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("-metrics-listen %q was accepted:\n%s", addr, out)
			continue
		}
		if !strings.Contains(string(out), "-metrics-listen") {
			t.Errorf("-metrics-listen %q failed for some other reason:\n%s", addr, out)
		}
	}
}

// TestFromFlagsInAChildProcess is not a test of its own: it is the child
// TestMetricsListenRefusesAWildcard runs, and does nothing unless that test
// set the variable it reads.
func TestFromFlagsInAChildProcess(t *testing.T) {
	args := os.Getenv("SOUS_TEST_FROMFLAGS_ARGS")
	if args == "" {
		return
	}
	fromFlags(strings.Split(args, "\x1f"))
	os.Exit(0)
}

// EVERY SPELLING OF "ALL INTERFACES", not only the common ones. The check
// used to compare the host against a list of strings, and [::0], [0::0],
// [::ffff:0.0.0.0] and a zoned [::%lo] all walked past it and bound
// everything - for -listen and -grpc-listen as well as this flag.
func TestRequireBindableRefusesEveryUnspecifiedAddress(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:1", "[::]:1", ":1", "[::0]:1", "[0::0]:1", "[0:0:0:0:0:0:0:0]:1",
		"[0000::]:1", "[::ffff:0.0.0.0]:1", "[::%lo]:1", "1", "",
	} {
		if err := requireBindable("-listen", addr); err == nil {
			t.Errorf("%q was accepted", addr)
		}
	}
	for _, addr := range []string{"100.64.0.1:8090", "127.0.0.1:1", "[::1]:1", "[fd7a:115c:a1e0::1]:1"} {
		if err := requireBindable("-listen", addr); err != nil {
			t.Errorf("%q was refused: %v", addr, err)
		}
	}
}

// The metrics listener on the address of another listener would stop that one
// from starting - and the one it stops is the API.
func TestMetricsListenMustBeItsOwnAddress(t *testing.T) {
	if err := requireOwnAddress("100.64.0.1:8090", "100.64.0.1:8090", "100.64.0.1:8091"); err == nil {
		t.Error("-metrics-listen equal to -listen was accepted")
	}
	if err := requireOwnAddress("100.64.0.1:8091", "100.64.0.1:8090", "100.64.0.1:8091"); err == nil {
		t.Error("-metrics-listen equal to -grpc-listen was accepted")
	}
	if err := requireOwnAddress("100.64.0.1:8092", "100.64.0.1:8090", "100.64.0.1:8091"); err != nil {
		t.Errorf("a distinct address was refused: %v", err)
	}
}
