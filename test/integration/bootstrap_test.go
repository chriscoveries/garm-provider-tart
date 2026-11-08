//go:build integration

// Package integration_test drives the real GARM controller against the real
// provider binary, two fake hosts (real tart-hostctl + stub tart + real sshd)
// and a real Gitea forge, all on one compose network. No Apple hardware, no
// GitHub tokens, no host ports: everything is driven through `docker compose
// exec`.
//
// Run with:
//
//	go test -tags integration ./test/integration/... -count=1 -v
//
// or `make integration`. The compose stack is brought up, the suite runs, and
// `docker compose down -v` tears it down (via defer/trap) even on failure.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	garmService    = "garm"
	giteaService   = "gitea"
	fakeHost1      = "fake-host-1"
	fakeHost2      = "fake-host-2"
	garmBin        = "/bin/garm-cli"
	garmURL        = "http://garm:9997"
	giteaURL       = "http://gitea:3000"
	giteaAdminUser = "testing"
	giteaAdminPass = "superSecretPassword"
	giteaAdminMail = "admin@example.com"
	garmAdminUser  = "admin"
	garmAdminPass  = "Garm-Admin-Password-9!xK2#"
	repoName       = "testrepo"
)

// ---- test scaffolding -----------------------------------------------------

// integrationDir returns the absolute path of the directory holding this test,
// so the suite is location-independent.
func integrationDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(file)
}

// compose returns the compose command prefix as a token list. Override with
// GARM_TEST_COMPOSE (e.g. "podman-compose") for local rootless runs.
func compose() []string {
	if v := os.Getenv("GARM_TEST_COMPOSE"); v != "" {
		return strings.Fields(v)
	}
	return []string{"docker", "compose"}
}

func integrationWD() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// runCompose runs a compose subcommand in the integration directory.
func runCompose(t *testing.T, args ...string) string {
	t.Helper()
	cmd := append(compose(), args...)
	return runCmd(t, integrationWD(), cmd[0], cmd[1:]...)
}

// execIn runs a command inside a service container.
func execIn(t *testing.T, service string, cmd ...string) string {
	t.Helper()
	args := append(compose(), "exec", "-T", service)
	args = append(args, cmd...)
	return runCmd(t, integrationWD(), args[0], args[1:]...)
}

// execInAs is execIn with a --user, used to run gitea admin commands as the
// gitea `git` user (uid 1000) rather than root.
func execInAs(t *testing.T, service, user string, cmd ...string) string {
	t.Helper()
	args := append(compose(), "exec", "-T", "--user", user, service)
	args = append(args, cmd...)
	return runCmd(t, integrationWD(), args[0], args[1:]...)
}

// execInErr is execIn that tolerates failure; used for readiness polling.
func execInErr(service string, cmd ...string) (string, error) {
	args := append(compose(), "exec", "-T", service)
	args = append(args, cmd...)
	return runCmdErr(integrationWD(), args[0], args[1:]...)
}

// garmCLI runs garm-cli inside the garm container.
func garmCLI(t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{garmBin}, args...)
	return execIn(t, garmService, full...)
}

// giteaAPI performs a Gitea API request from inside the garm container (which
// can reach gitea over the compose network). It returns the response body.
func giteaAPI(t *testing.T, method, path, body, basicAuth string) string {
	t.Helper()
	url := giteaURL + path
	args := []string{"curl", "-sS", "-X", method, url, "-H", "Content-Type: application/json"}
	if basicAuth != "" {
		args = append(args, "-u", basicAuth)
	}
	if body != "" {
		args = append(args, "-d", body)
	}
	return execIn(t, garmService, args...)
}

// runCmd runs a command, returning stdout; it fails the test on error and
// attaches stderr.
func runCmd(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	out, err := runCmdErr(dir, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// runCmdErr runs a command and returns stdout and a wrapped error.
func runCmdErr(dir string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w (stderr: %s)",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// waitFor polls cond until it returns nil or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if err := cond(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(2 * time.Second)
	}
}

// ---- staged files ---------------------------------------------------------

// prepare builds the two binaries, generates an SSH keypair and stages every
// file the compose stack mounts, all under <dir>/.work.
func prepare(t *testing.T, dir string) {
	t.Helper()
	work := filepath.Join(dir, ".work")
	// Best-effort reset of the working directory: stale gitea/garm sqlite files
	// now live in named volumes, so nothing here is stateful across runs, but
	// drop any leftovers. Ignore errors — a rootless podman run can leave
	// foreign-uid files behind that the host user cannot remove.
	_ = os.RemoveAll(work)
	bin := filepath.Join(work, "bin")
	provider := filepath.Join(work, "provider")
	garmDir := filepath.Join(work, "garm")
	for _, d := range []string{bin, provider, garmDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	repoRoot := filepath.Dir(filepath.Dir(dir))
	// The provider binary must land under .work/provider (mounted read-only at
	// /opt/garm-tart) so the wrapper's `exec /opt/garm-tart/garm-provider-tart`
	// finds it; tart-hostctl goes under .work/bin (mounted on the fake hosts).
	runCmd(t, repoRoot, "go", "build", "-o", filepath.Join(provider, "garm-provider-tart"), "./cmd/garm-provider-tart")
	runCmd(t, repoRoot, "go", "build", "-o", filepath.Join(bin, "tart-hostctl"), "./cmd/tart-hostctl")

	key := filepath.Join(provider, "id_ed25519")
	// ssh-keygen refuses to overwrite an existing key, so drop any stale pair.
	_ = os.Remove(key)
	_ = os.Remove(key + ".pub")
	runCmd(t, dir, "ssh-keygen", "-t", "ed25519", "-f", key, "-N", "", "-C", "garm-tart-integration-test", "-q")
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatalf("read pubkey: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "authorized_keys"), pub, 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}

	// Provider payloads (binary, wrapper, configs, key) mount at /opt/garm-tart;
	// the GARM server config mounts at /etc/garm/config.toml.
	copyFile(t, filepath.Join(provider, "provider-wrapper.sh"), filepath.Join(dir, "provider-wrapper.sh"))
	if err := os.Chmod(filepath.Join(provider, "provider-wrapper.sh"), 0o755); err != nil {
		t.Fatalf("chmod wrapper: %v", err)
	}
	copyFile(t, filepath.Join(provider, "provider-config.toml"), filepath.Join(dir, "provider-config.toml"))
	copyFile(t, filepath.Join(provider, "provider-config-blackhole.toml"), filepath.Join(dir, "provider-config-blackhole.toml"))
	copyFile(t, filepath.Join(garmDir, "config.toml"), filepath.Join(dir, "garm-config.toml"))
}

func copyFile(t *testing.T, dst, src string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

// ---- test -----------------------------------------------------------------

func TestRealGARMAgainstFakeHosts(t *testing.T) {
	dir := integrationDir(t)

	// Best-effort teardown of any stale stack from a crashed prior run.
	_, _ = runCmdErr(dir, compose()[0], append(compose()[1:], "down", "-v", "--remove-orphans")...)

	prepare(t, dir)

	// Teardown always runs: remove the stack and volumes.
	defer func() {
		runCompose(t, "down", "-v", "--remove-orphans")
	}()

	// Bring the stack up (building the fake-host and garm images).
	runCompose(t, "up", "-d", "--build", "--remove-orphans")

	// Wait for GARM and Gitea to be reachable.
	waitFor(t, 3*time.Minute, "garm API", func() error {
		_, err := execInErr(garmService, "curl", "-sS", garmURL+"/api/v1/providers")
		return err
	})
	waitFor(t, 2*time.Minute, "gitea API", func() error {
		out, err := execInErr(garmService, "curl", "-sS", giteaURL+"/api/v1/version")
		if err != nil || !strings.Contains(out, "version") {
			return fmt.Errorf("gitea not ready: %v %s", err, out)
		}
		return nil
	})

	// Gitea admin user, repository, and PAT (admin commands run as uid 1000).
	execInAs(t, giteaService, "1000", "gitea", "admin", "user", "create",
		"--username", giteaAdminUser, "--password", giteaAdminPass,
		"--email", giteaAdminMail, "--admin", "--must-change-password=false")
	giteaAPI(t, "POST", "/api/v1/user/repos",
		`{"name":"`+repoName+`","private":false,"auto_init":false}`,
		giteaAdminUser+":"+giteaAdminPass)
	token := parseGiteaToken(t, giteaAPI(t, "POST", "/api/v1/users/"+giteaAdminUser+"/tokens",
		`{"name":"garm-token","scopes":["write:repository","write:organization"]}`,
		giteaAdminUser+":"+giteaAdminPass))

	// Initialize GARM, then register endpoint, credentials, repo and pool.
	garmCLI(t, "init", "--name", "garm", "--url", garmURL,
		"--username", garmAdminUser, "--password", garmAdminPass,
		"--email", giteaAdminMail, "--full-name", "GARM Admin")
	garmCLI(t, "gitea", "endpoint", "create",
		"--name", "local-gitea",
		"--api-base-url", giteaURL+"/",
		"--base-url", giteaURL+"/",
		"--tools-metadata-url", "http://tools:8080/gitea-runner-releases.json",
		"--description", "local gitea")
	garmCLI(t, "gitea", "credentials", "add",
		"--name", "gitea-token", "--endpoint", "local-gitea",
		"--auth-type", "pat", "--pat-oauth-token", token,
		"--description", "gitea runner token")
	garmCLI(t, "repository", "add",
		"--name", repoName, "--owner", giteaAdminUser,
		"--credentials", "gitea-token", "--forge-type", "gitea",
		"--random-webhook-secret")
	repoID := listEntityID(t, "repository")
	poolID := createPool(t, repoID, "tart", 5)

	// An idle instance must appear and reach `running`.
	inst := waitForRunningInstance(t, poolID)

	assertBootstrapPayload(t, poolID)
	assertTartVerbs(t)
	assertRegistryAndRunning(t, inst)
	assertListAndGetRoundTrip(t, inst)

	// Scale-down: remove the runner (driving the provider's DeleteInstance and
	// `tart delete`), then delete the now-empty pool and assert the host is
	// fully cleaned up. GARM refuses to delete a pool that still has runners,
	// so the runner must go first.
	garmCLI(t, "runner", "delete", inst.Name)
	waitFor(t, 2*time.Minute, "tart delete after runner delete", func() error {
		if !tartLogContains(t, fakeHost1, "delete") && !tartLogContains(t, fakeHost2, "delete") {
			return fmt.Errorf("no `tart delete` observed yet")
		}
		return nil
	})
	waitFor(t, 2*time.Minute, "registry empty after runner delete", func() error {
		if registryRecords(t, fakeHost1)+registryRecords(t, fakeHost2) != 0 {
			return fmt.Errorf("registry still has records")
		}
		return nil
	})
	garmCLI(t, "pool", "delete", poolID)
	assertNoLeftoverArtifacts(t)

	// Failure path: a blackholed host must fail fast, retry, and leak nothing.
	blackPool := createPool(t, repoID, "tart-blackhole", 3)
	blackInst := waitForErrorInstance(t, blackPool, 90*time.Second)
	// The blackholed host cannot be reached for a clean delete, so force-remove
	// the errored instance from GARM (ignoring the provider error) and wait for
	// the reconcile loop to drop it before deleting the pool.
	garmCLI(t, "runner", "delete", blackInst.Name, "--force-remove-runner")
	waitFor(t, 90*time.Second, "blackhole runner removed", func() error {
		if findInstance(t, blackPool, func(i cliInstance) bool { return true }).ID != "" {
			return fmt.Errorf("runner still present in pool")
		}
		return nil
	})
	garmCLI(t, "pool", "delete", blackPool)
	assertNoLeftoverArtifacts(t)
}

// ---- garm-cli JSON helpers ------------------------------------------------

type cliInstance struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ProviderID string `json:"provider_id"`
	Status     string `json:"status"`
	PoolID     string `json:"pool_id"`
}

func listEntityID(t *testing.T, entity string) string {
	t.Helper()
	var out []struct {
		ID string `json:"id"`
	}
	raw := garmCLI(t, entity, "list", "--format", "json")
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("parse %s list JSON: %v\n%s", entity, err, raw)
	}
	if len(out) == 0 {
		t.Fatalf("no %s registered", entity)
	}
	return out[0].ID
}

func createPool(t *testing.T, repoID, provider string, bootstrapTimeout uint) string {
	t.Helper()
	garmCLI(t, "pool", "add",
		"--repo", repoID,
		"--provider-name", provider,
		"--image", "ghcr.io/cirruslabs/macos-tahoe-xcode@sha256:integration-test",
		"--flavor", "default",
		"--tags", "self-hosted,macos,arm64",
		"--os-arch", "arm64",
		"--os-type", "linux",
		"--enabled",
		"--min-idle-runners", "1",
		"--max-runners", "2",
		"--runner-bootstrap-timeout", fmt.Sprint(bootstrapTimeout))
	var pools []struct {
		ID string `json:"id"`
	}
	raw := garmCLI(t, "pool", "list", "--format", "json")
	if err := json.Unmarshal([]byte(raw), &pools); err != nil {
		t.Fatalf("parse pool list JSON: %v\n%s", err, raw)
	}
	if len(pools) == 0 {
		t.Fatal("no pools")
	}
	return pools[len(pools)-1].ID
}

func waitForRunningInstance(t *testing.T, poolID string) cliInstance {
	t.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	var inst cliInstance
	for {
		inst = findInstance(t, poolID, func(i cliInstance) bool { return i.Status == "running" })
		if inst.ID != "" {
			return inst
		}
		if time.Now().After(deadline) {
			dumpDiagnostics(t, poolID)
			t.Fatalf("timed out waiting for running instance in pool %s", poolID)
		}
		time.Sleep(3 * time.Second)
	}
}

// dumpDiagnostics prints GARM logs, the provider capture log and the stub tart
// logs, to make an integration failure debuggable without a live shell.
func dumpDiagnostics(t *testing.T, poolID string) {
	t.Helper()
	t.Logf("----- diagnostics for pool %s -----", poolID)
	if out, err := runCmdErr(integrationWD(), compose()[0], append(compose()[1:], "logs", "--tail", "80", garmService)...); err == nil {
		t.Logf("-- garm logs --\n%s", out)
	}
	t.Logf("-- provider calls --")
	if out, err := execInErr(garmService, "cat", "/tmp/garm-provider-capture/calls.log"); err == nil {
		t.Logf("%s", out)
	}
	t.Logf("-- tart log (fake-host-1) --")
	if out, err := execInErr(fakeHost1, "cat", "/var/lib/tart-fake/tart.log"); err == nil {
		t.Logf("%s", out)
	}
	t.Logf("-- tart log (fake-host-2) --")
	if out, err := execInErr(fakeHost2, "cat", "/var/lib/tart-fake/tart.log"); err == nil {
		t.Logf("%s", out)
	}
}

func waitForErrorInstance(t *testing.T, poolID string, within time.Duration) cliInstance {
	t.Helper()
	start := time.Now()
	var inst cliInstance
	waitFor(t, within, "error instance", func() error {
		inst = findInstance(t, poolID, func(i cliInstance) bool { return i.Status == "error" })
		if inst.ID == "" {
			return fmt.Errorf("no error instance yet")
		}
		return nil
	})
	t.Logf("create failed in %s", time.Since(start))
	return inst
}

func findInstance(t *testing.T, poolID string, match func(cliInstance) bool) cliInstance {
	t.Helper()
	raw := garmCLI(t, "runner", "list", poolID, "--format", "json")
	var list []cliInstance
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("parse runner list JSON: %v\n%s", err, raw)
	}
	for _, i := range list {
		if match(i) {
			return i
		}
	}
	return cliInstance{}
}

func parseGiteaToken(t *testing.T, raw string) string {
	t.Helper()
	var resp struct {
		SHA1 string `json:"sha1"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("parse gitea token JSON: %v\n%s", err, raw)
	}
	if resp.SHA1 == "" {
		t.Fatalf("no sha1 token in gitea response: %s", raw)
	}
	return resp.SHA1
}

// ---- assertion helpers ----------------------------------------------------

// capturedCall is one line of the provider wrapper's calls.log.
type capturedCall struct {
	Command    string
	PoolID     string
	InstanceID string
	Controller string
	StdinPath  string
}

func captureLog(t *testing.T) []capturedCall {
	t.Helper()
	raw := execIn(t, garmService, "cat", "/tmp/garm-provider-capture/calls.log")
	var calls []capturedCall
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "CALL ") {
			continue
		}
		c := capturedCall{}
		for _, tok := range strings.Fields(line) {
			k, v, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			switch k {
			case "GARM_COMMAND":
				c.Command = v
			case "GARM_POOL_ID":
				c.PoolID = v
			case "GARM_INSTANCE_ID":
				c.InstanceID = v
			case "GARM_CONTROLLER_ID":
				c.Controller = v
			case "stdin":
				c.StdinPath = v
			}
		}
		calls = append(calls, c)
	}
	return calls
}

func bootstrapForPool(t *testing.T, poolID string) map[string]any {
	t.Helper()
	var stdinPath string
	for _, c := range captureLog(t) {
		if c.Command == "CreateInstance" && c.PoolID == poolID {
			stdinPath = c.StdinPath
			break
		}
	}
	if stdinPath == "" {
		t.Fatalf("no CreateInstance capture for pool %s", poolID)
	}
	raw := execIn(t, garmService, "cat", stdinPath)
	var boot map[string]any
	if err := json.Unmarshal([]byte(raw), &boot); err != nil {
		t.Fatalf("parse bootstrap JSON: %v\n%s", err, raw)
	}
	return boot
}

// assertBootstrapPayload checks the CreateInstance input GARM sent the provider.
func assertBootstrapPayload(t *testing.T, poolID string) {
	t.Helper()
	var controller string
	for _, c := range captureLog(t) {
		if c.Command == "CreateInstance" && c.PoolID == poolID {
			controller = c.Controller
		}
	}
	if controller == "" {
		t.Fatal("CreateInstance GARM_CONTROLLER_ID empty")
	}

	boot := bootstrapForPool(t, poolID)

	// tools must contain a macOS/arm64 entry. Gitea reports the runner asset as
	// `darwin`; GitHub reports `osx`. The provider must accept either (T3).
	tools, ok := boot["tools"].([]any)
	if !ok {
		t.Fatalf("bootstrap has no tools array: %#v", boot["tools"])
	}
	var foundMacARM64 bool
	for _, tAny := range tools {
		tool, _ := tAny.(map[string]any)
		osName, _ := tool["os"].(string)
		arch, _ := tool["architecture"].(string)
		if arch == "arm64" && (osName == "osx" || osName == "darwin") {
			foundMacARM64 = true
		}
	}
	if !foundMacARM64 {
		t.Fatalf("bootstrap tools have no darwin|osx / arm64 entry: %s", mustJSON(tools))
	}

	extra, ok := boot["extra_specs"].(map[string]any)
	if !ok {
		t.Fatalf("bootstrap has no extra_specs object")
	}
	if tmpl, _ := extra["runner_install_template"].(string); strings.TrimSpace(tmpl) == "" {
		t.Fatalf("extra_specs.runner_install_template is empty")
	}
	if got := boot["pool_id"]; got != poolID {
		t.Fatalf("bootstrap pool_id = %v, want %s", got, poolID)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// assertTartVerbs checks the stub tart saw the create lifecycle in order. The
// hostctl create drives: clone, get (disk size), set (resources), detached
// run --no-graphics, exec (readiness), ip (address for the ProviderInstance).
func assertTartVerbs(t *testing.T) {
	t.Helper()
	log := tartLog(t, fakeHost1) + "\n" + tartLog(t, fakeHost2)
	order := []string{"clone ", "get ", "set ", "run --no-graphics", "exec ", "ip "}
	pos := -1
	for _, verb := range order {
		idx := strings.Index(log[pos+1:], verb)
		if idx < 0 {
			t.Fatalf("tart log missing %q (in order); log:\n%s", verb, log)
		}
		pos += 1 + idx
	}
}

// assertRegistryAndRunning checks the host registry and the GARM instance view.
func assertRegistryAndRunning(t *testing.T, inst cliInstance) {
	t.Helper()
	host, name := splitProviderID(t, inst.ProviderID)

	var rec struct {
		Name    string `json:"name"`
		State   string `json:"state"`
		OwnerID string `json:"owner_id"`
	}
	raw := execIn(t, host, "cat", "/var/db/tart-fleet/instances/"+name+".json")
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("parse registry record: %v\n%s", err, raw)
	}
	if rec.State != "ready" && rec.State != "bootstrapping" && rec.State != "idle" {
		t.Fatalf("registry record state = %q, want ready|bootstrapping|idle", rec.State)
	}
	if rec.OwnerID != inst.PoolID {
		t.Fatalf("registry owner_id = %q, want pool %q", rec.OwnerID, inst.PoolID)
	}
	if inst.Status != "running" {
		t.Fatalf("GARM instance status = %q, want running", inst.Status)
	}
}

// assertListAndGetRoundTrip checks ListInstances and GetInstance against GARM.
func assertListAndGetRoundTrip(t *testing.T, inst cliInstance) {
	t.Helper()
	raw := garmCLI(t, "runner", "list", inst.PoolID, "--format", "json")
	var list []cliInstance
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("parse runner list: %v\n%s", err, raw)
	}
	if len(list) != 1 {
		t.Fatalf("ListInstances returned %d instances, want 1: %s", len(list), raw)
	}
	if list[0].ProviderID != inst.ProviderID {
		t.Fatalf("ListInstances provider_id = %q, want %q", list[0].ProviderID, inst.ProviderID)
	}

	raw = garmCLI(t, "runner", "show", inst.Name, "--format", "json")
	var got cliInstance
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("parse runner show: %v\n%s", err, raw)
	}
	if got.ProviderID != inst.ProviderID {
		t.Fatalf("GetInstance provider_id = %q, want %q", got.ProviderID, inst.ProviderID)
	}
}

func splitProviderID(t *testing.T, providerID string) (host, name string) {
	t.Helper()
	h, n, ok := strings.Cut(providerID, "/")
	if !ok || h == "" || n == "" {
		t.Fatalf("unexpected provider_id %q", providerID)
	}
	return h, n
}

func tartLog(t *testing.T, host string) string {
	t.Helper()
	return execIn(t, host, "cat", "/var/lib/tart-fake/tart.log")
}

func tartLogContains(t *testing.T, host, needle string) bool {
	t.Helper()
	return strings.Contains(tartLog(t, host), needle)
}

func registryRecords(t *testing.T, host string) int {
	t.Helper()
	// `ls` on a non-matching glob exits non-zero (the empty state), so tolerate
	// the error and treat empty output as zero records.
	out, _ := execInErr(host, "sh", "-c", "ls /var/db/tart-fleet/instances/*.json 2>/dev/null")
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

// assertNoLeftoverArtifacts checks the SETUP.md artifact table: no instance
// records, no bootstrap scripts, no VM bundle directory.
func assertNoLeftoverArtifacts(t *testing.T) {
	t.Helper()
	for _, host := range []string{fakeHost1, fakeHost2} {
		if n := registryRecords(t, host); n != 0 {
			t.Fatalf("%s: %d leftover registry records", host, n)
		}
		out, _ := execInErr(host, "sh", "-c", "ls /var/db/tart-fleet/bootstrap/*.sh 2>/dev/null")
		if strings.TrimSpace(out) != "" {
			t.Fatalf("%s: leftover bootstrap scripts: %s", host, out)
		}
		out, _ = execInErr(host, "sh", "-c", "ls -d /var/lib/tart-fake/vms/* 2>/dev/null")
		if strings.TrimSpace(out) != "" {
			t.Fatalf("%s: leftover VM directories: %s", host, out)
		}
	}
}
