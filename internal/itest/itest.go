// Package itest holds shared helpers for OxiMail's integration tests:
// booting a throwaway oxidb-server, picking free ports, and waiting for
// a listener to come up.
//
// It is a normal package (not a _test package) so it can be imported by
// the integration tests across internal/store, internal/smtp, and so
// on. Nothing under cmd/ imports it, so it never reaches a release
// binary.
package itest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Option tweaks how StartOxiDB boots the server.
type Option func(*config)

type config struct {
	extraEnv []string
}

// LazySync starts the server with OXIDB_LAZY_SYNC=true: commits are
// batched by a background thread instead of being fsync'd per write.
//
// This trades crash durability for speed. Use it only for tests that
// exercise in-memory behaviour — logical correctness, atomicity,
// concurrency — and not durability across a restart. Atomicity is
// lock-based in OxiDB, so it holds regardless of the sync mode.
func LazySync() Option {
	return func(c *config) { c.extraEnv = append(c.extraEnv, "OXIDB_LAZY_SYNC=true") }
}

// StartOxiDB boots an oxidb-server in a fresh temp directory on a free
// port and registers its teardown with t. If the server binary cannot
// be found the test is skipped, not failed. It returns the host and
// port to dial.
//
// The binary is located at $OXIDB_BIN, then at the sibling OxiDB
// checkout's target/{release,debug}/oxidb-server.
func StartOxiDB(t *testing.T, opts ...Option) (host string, port int) {
	t.Helper()
	bin := findOxiDBBinary(t)

	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}

	port = FreePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("OXIDB_ADDR=127.0.0.1:%d", port),
		"OXIDB_DATA="+t.TempDir(),
		"OXIDB_S3_PORT=0", // S3 surface off — blob ops go over the native protocol
		"OXIDB_IDLE_TIMEOUT=120",
	)
	cmd.Env = append(cmd.Env, cfg.extraEnv...)
	cmd.Stdout = os.Stderr // surfaced by `go test` only on failure
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start oxidb-server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_, _ = cmd.Process.Wait()
	})

	WaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return "127.0.0.1", port
}

// findOxiDBBinary locates the oxidb-server binary: $OXIDB_BIN, then the
// release and debug build outputs of the sibling OxiDB checkout. Paths
// are relative to the test's working directory (its package directory).
func findOxiDBBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("OXIDB_BIN"); bin != "" {
		if _, err := os.Stat(bin); err == nil {
			return bin
		}
		t.Fatalf("OXIDB_BIN=%s does not exist", bin)
	}
	for _, p := range []string{
		"../../../docdb/target/release/oxidb-server",
		"../../../docdb/target/debug/oxidb-server",
	} {
		if abs, err := filepath.Abs(p); err == nil {
			if _, err := os.Stat(abs); err == nil {
				return abs
			}
		}
	}
	t.Skip("oxidb-server binary not found — set OXIDB_BIN or build it (cargo build -p oxidb-server)")
	return ""
}

// FreePort returns a TCP port that was free at the time of the call.
// There is an inherent race between the port being released here and a
// caller binding it; it is good enough for tests.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// WaitTCP blocks until `addr` accepts a TCP connection, failing the test
// if nothing comes up within 10 seconds.
func WaitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after 10s", addr)
}
