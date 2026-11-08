package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sshStubPath(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", "testdata", "bin", "ssh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("ssh stub missing: %v", err)
	}
	return abs
}

func writeStubFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testClient(t *testing.T) *Client {
	t.Helper()
	return NewClient(
		Host{Name: "mac-mini-01", Address: "10.20.0.11"},
		"garm",
		"/etc/garm/id_ed25519",
		"",
		sshStubPath(t),
		"/usr/local/bin/tart-hostctl",
	)
}

func TestNewClientResolvesSSHBinWithoutPATH(t *testing.T) {
	// Arrange: GARM runs external providers with a minimal environment that has
	// no PATH, so `ssh` cannot be resolved by lookup and must fall back to the
	// standard absolute location.
	t.Setenv("PATH", "")

	// Act: build a client with no explicit ssh_bin.
	c := NewClient(Host{Name: "mac-mini-01", Address: "10.20.0.11"}, "garm", "", "", "", "")

	// Assert: a concrete path, never the bare "ssh" that would fail under a
	// PATH-less provider environment.
	if c.sshBin != "/usr/bin/ssh" {
		t.Fatalf("sshBin = %q, want /usr/bin/ssh", c.sshBin)
	}
}

func TestClientRunRecordsHostctlInvocation(t *testing.T) {
	// Arrange: a stub that logs argv and returns success.
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("SSH_STUB_LOG", logPath)
	c := testClient(t)

	// Act: run a delete verb.
	_, err := c.Run(context.Background(), "delete", "--name", "garm-abc123")

	// Assert: success and the remote command is hostctl delete --name ... .
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	data, _ := os.ReadFile(logPath)
	line := string(data)
	if !strings.Contains(line, "garm@10.20.0.11") {
		t.Fatalf("argv %q missing target user@host", line)
	}
	if !strings.Contains(line, "/usr/local/bin/tart-hostctl delete '--name' 'garm-abc123'") {
		t.Fatalf("argv %q missing hostctl delete invocation", line)
	}
}

func TestClientQuotesRemoteShellArguments(t *testing.T) {
	c := testClient(t)
	argv := c.argv("create", "--image", "a;$(touch /tmp/unsafe) 'quoted'", "--owner", "owner with spaces")
	remote := strings.Join(argv, " ")
	if !strings.Contains(remote, "'a;$(touch /tmp/unsafe) '\\''quoted'\\''' '") {
		t.Fatalf("remote command is not safely shell-quoted: %q", remote)
	}
	if !strings.Contains(remote, "'owner with spaces'") {
		t.Fatalf("remote command loses argument boundaries: %q", remote)
	}
}

func TestClientVerifiesSSHHostKeys(t *testing.T) {
	c := testClient(t)
	joined := strings.Join(c.argv("status"), " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("unconfigured host-key policy is unsafe: %q", joined)
	}
	c.knownHosts = "/etc/garm/known_hosts"
	joined = strings.Join(c.argv("status"), " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=yes") || !strings.Contains(joined, "UserKnownHostsFile=/etc/garm/known_hosts") {
		t.Fatalf("explicit known_hosts policy was not enforced: %q", joined)
	}
}

func TestClientRunInputForwardsBootstrapOverStdin(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "stdin.log")
	t.Setenv("SSH_STUB_STDIN_LOG", logPath)
	c := testClient(t)
	script := "#!/bin/sh\nprintf 'secret callback token'\n"
	if _, err := c.RunInput(context.Background(), strings.NewReader(script), "bootstrap", "--name", "garm-abc123"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != script {
		t.Fatalf("bootstrap stdin changed (got %q)", data)
	}
}

func TestClientRunHostUnreachableReturnsHardError(t *testing.T) {
	// Arrange: the stub simulates a connection failure (exit 255).
	t.Setenv("SSH_STUB_EXIT", "255")
	c := testClient(t)

	// Act: run.
	_, err := c.Run(context.Background(), "get", "--name", "garm-abc123")

	// Assert: a hard error wrapping ErrHostUnreachable, never a success.
	if !errors.Is(err, ErrHostUnreachable) {
		t.Fatalf("error = %v, want ErrHostUnreachable", err)
	}
}

func TestClientRunClassifiesCapacityFault(t *testing.T) {
	// Arrange: the stub exits 1 with a structured capacity fault on stderr.
	t.Setenv("SSH_STUB_EXIT", "1")
	t.Setenv("SSH_STUB_STDERR", writeStubFile(t, `{"fault":"capacity","error":"no free slot"}`))
	c := testClient(t)

	// Act: run create.
	_, err := c.Run(context.Background(), "create", "--name", "garm-abc123")

	// Assert: the fault is classified, not a raw ssh error.
	var fe *FaultError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a FaultError", err)
	}
	if fe.Fault != "capacity" {
		t.Fatalf("fault = %q, want capacity", fe.Fault)
	}
}

func TestClientRunReturnsStdout(t *testing.T) {
	// Arrange: the stub replays canned JSON.
	t.Setenv("SSH_STUB_OUTPUT", writeStubFile(t, `{"name":"garm-abc123","state":"ready"}`))
	c := testClient(t)

	// Act: run get.
	out, err := c.Run(context.Background(), "get", "--name", "garm-abc123")

	// Assert: the JSON comes back verbatim.
	if err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	if !strings.Contains(string(out), `"state":"ready"`) {
		t.Fatalf("stdout = %q, want canned JSON", string(out))
	}
}
