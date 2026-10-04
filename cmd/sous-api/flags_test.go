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
