package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// closedAddr returns a loopback address with nothing listening on it, so a probe against it fails at the
// transport layer rather than reaching a server. Reserving and releasing a real port is what makes this
// reliable — a hardcoded one might happen to be in use.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// TestCheckHealthzDistinguishesUnreachableFromUnhealthy pins the discrimination the compose hint depends on.
//
// Both cases are failures and both make `doctor` exit non-zero, so a test that only asserted "it failed"
// would pass with the two collapsed into one. The difference is what the operator should do next: nothing
// answered means they are probably probing the wrong address (inside a compose stack, the server is another
// container), while a non-200 means they found the server and it is reporting a sick dependency. Sending the
// second case a "look somewhere else" hint would be actively misleading, which is why the sentinel exists.
func TestCheckHealthzDistinguishesUnreachableFromUnhealthy(t *testing.T) {
	t.Run("nothing listening is unreachable", func(t *testing.T) {
		err := checkHealthz(t.Context(), "http://"+closedAddr(t))
		if err == nil {
			t.Fatal("probing a closed port succeeded; the check cannot detect a wrong address")
		}
		if !errors.Is(err, errServerUnreachable) {
			t.Errorf("error = %v, want it to wrap errServerUnreachable so the caller can offer the compose hint", err)
		}
	})

	t.Run("a server answering non-200 is NOT unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		err := checkHealthz(t.Context(), srv.URL)
		if err == nil {
			t.Fatal("a 503 from /healthz was reported as healthy")
		}
		if errors.Is(err, errServerUnreachable) {
			t.Error("a 503 was classified as unreachable; the operator would be told to look elsewhere for a " +
				"server that is right there and unwell")
		}
	})
}

// TestDoctorPrintsTheComposeHintOnlyWhenItApplies checks the rendered output, not just the classification —
// the hint's value is that it appears in the terminal at the moment of confusion.
//
// The negative half is the one that matters: `doctor` is equally valid against a bare-metal server, and a
// hint about Docker printed under an unrelated failure is noise that teaches people to skim past hints.
func TestDoctorPrintsTheComposeHintOnlyWhenItApplies(t *testing.T) {
	t.Setenv("LORE_DATABASE_URL", "") // keep the database branch out of this test's output

	t.Run("unreachable server", func(t *testing.T) {
		out := runDoctor(t, "http://"+closedAddr(t))
		if !strings.Contains(out, doctorComposeInvocation) {
			t.Errorf("output does not name the invocation that works:\n%s", out)
		}
		if !strings.Contains(out, "the server is a different container") {
			t.Errorf("output does not explain WHY localhost failed:\n%s", out)
		}
	})

	t.Run("server present but unhealthy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		out := runDoctor(t, srv.URL)
		if strings.Contains(out, doctorComposeInvocation) {
			t.Errorf("the compose hint was printed for a server that answered 503 — it points the operator "+
				"away from the server they already found:\n%s", out)
		}
	})
}

// TestDoctorPrintsTheDatabaseHintOnlyOnAFailedConnection covers the other half of the same gap.
//
// The two failures a compose user can hit are symmetric — on the host the database is unreachable, in the
// stack the server is — so a hint on only one of them leaves half the users stuck. This needs no database:
// pgxpool connects lazily, so a DSN pointing at a closed port fails at Ping, which is the branch that
// carries the hint.
func TestDoctorPrintsTheDatabaseHintOnlyOnAFailedConnection(t *testing.T) {
	t.Run("unreachable database", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", "postgres://lore:lore@"+closedAddr(t)+"/lore?sslmode=disable")
		out := runDoctor(t, "http://"+closedAddr(t))
		if !strings.Contains(out, "the stack does not publish 5432") {
			t.Errorf("a failed database connection printed no hint about where the database actually is:\n%s", out)
		}
	})

	t.Run("an unset DSN gets no hint", func(t *testing.T) {
		t.Setenv("LORE_DATABASE_URL", "")
		out := runDoctor(t, "http://"+closedAddr(t))
		if strings.Contains(out, "the stack does not publish 5432") {
			t.Errorf("the database hint was printed when LORE_DATABASE_URL is simply unset — that is a "+
				"configuration error, not a sign of probing the wrong host:\n%s", out)
		}
	})
}

// runDoctor executes the command with a given --url and returns everything it printed. The error is
// deliberately ignored: every case here is expected to fail some check, and the output is what is under test.
func runDoctor(t *testing.T, url string) string {
	t.Helper()
	cmd := doctorCmd()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--url", url})
	_ = cmd.Execute()
	return buf.String()
}

// TestDoctorComposeInvocationMatchesTheREADME locks the one string that lives in two places.
//
// The hint tells an operator to run a command; the README documents the same command. If they drift, one of
// them is wrong and neither says so — and the failure mode is silent, because both files still read fine on
// their own. Comparing them here is what makes "documented" and "printed" the same claim rather than two
// claims that happen to agree today.
func TestDoctorComposeInvocationMatchesTheREADME(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	if !strings.Contains(string(readme), doctorComposeInvocation) {
		t.Errorf("README.md does not contain the invocation the hint prints:\n  %s\n"+
			"the terminal and the docs must give the same command, or one of them is sending people wrong",
			doctorComposeInvocation)
	}
}
