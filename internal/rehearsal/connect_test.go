package rehearsal

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// connectScript is scripts/binance/connect.sh.
func connectScript() string { return filepath.Join(repoRoot(), "scripts", "binance", "connect.sh") }

// shim writes an executable that appends its argv to argvLog and its environment to envLog, then runs the real tool.
func shim(t *testing.T, dir, tool, argvLog, envLog string) {
	t.Helper()
	real, err := exec.LookPath(tool)
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"" + tool + " $*\" >>'" + argvLog + "'\nenv >>'" + envLog + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runConnect runs connect.sh with env and args and returns its output and exit code.
func runConnect(t *testing.T, o *outputs, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(connectScript(), args...)
	cmd.Dir = repoRoot()
	cmd.Env = env
	out := &syncBuffer{}
	o.add(out)
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	r := result{out: out.String()}
	if err != nil {
		r.code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		}
	}
	return r
}

// restrictionsWithdrawals returns the apiRestrictions answer of key_read_only.json with enableWithdrawals true.
func restrictionsWithdrawals(t *testing.T, readOnly []httpfixture.Call) httpfixture.Call {
	t.Helper()
	c := readOnly[1]
	var body map[string]any
	if err := json.Unmarshal(c.Body, &body); err != nil {
		t.Fatal(err)
	}
	body["enableWithdrawals"] = true
	c.Body, _ = json.Marshal(body)
	return c
}

// X1-T506 — Req: X1 D-19, X1 D-39, X1 D-40, X1 D-41. connect.sh against the running server and a fake Binance server,
// with marker values of BINANCE_API_KEY and BINANCE_API_SECRET in a temporary environment file; buf and jq run
// through shims that record their argv and their environment. A key that can withdraw is refused with the gRPC code and reason; the read-only key is connected ACTIVE; the
// output holds connection_id, status, fingerprint and permissions; delete removes the connection. The markers appear
// in no argv, no environment, no output and no file left in the temporary directories. Refusals: a CAS_GRPC_ADDR that is not local,
// a missing key.
func TestT506_ConnectScript(t *testing.T) {
	requireTools(t, "bash", "jq", "buf", "go")
	owner := testdb.Open(t)
	testdb.Clean(t)

	f, err := httpfixture.Read(filepath.Join(repoRoot(), "testdata", "fixtures", "binance", "key_read_only.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The first create: the time, a key that can withdraw. The second: the read-only key, the time still fresh.
	calls := append([]httpfixture.Call{f.Calls[0], restrictionsWithdrawals(t, f.Calls)}, f.Calls[1:]...)
	fake := httpfixture.Serve(t, httpfixture.File{Description: "connect.sh", Calls: calls}, nil)
	var saved []byte
	if err := owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'binance'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(ctx, `UPDATE sources SET config = $1 WHERE code = 'binance'`, saved) })
	if _, err := owner.Exec(ctx, `UPDATE sources SET config = config || jsonb_build_object('base_url', $1::text)
		WHERE code = 'binance'`, fake.URL()); err != nil {
		t.Fatal(err)
	}

	// server with a tick of one hour: no balance run during the test, only the calls of connect.sh.
	r := &rig{dir: t.TempDir(), binDir: t.TempDir(), out: &outputs{}}
	r.ports.grpc, r.ports.health = freePort(t), freePort(t)
	r.startServerWith(t, "SYNC_TICK=1h")

	work, tmp, shims, home := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	argv, environ := filepath.Join(work, "argv.log"), filepath.Join(work, "env.log")
	shim(t, shims, "buf", argv, environ)
	shim(t, shims, "jq", argv, environ)
	markerKey, markerSecret := "x1markerkey"+randomHex(t, 16), "x1markersecret"+randomHex(t, 24)
	envPath := filepath.Join(work, "env")
	writeEnvFile(t, envPath, map[string]string{
		"CASCTL_DATABASE_URL": testdb.URL(t),
		"BINANCE_API_KEY":     markerKey,
		"BINANCE_API_SECRET":  markerSecret,
	})
	credentials := filepath.Join(work, "cas", "x1-real.env")
	// The caches of go stay where they are: a temporary HOME must not make go build download the modules again.
	goEnv, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	goVars := strings.Fields(string(goEnv))
	env := with(without(baseEnv(), "PATH"),
		"PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+home,
		"TMPDIR="+tmp,
		"CAS_ENV_FILE="+envPath,
		"CAS_BIN_DIR="+r.binDir,
		"CAS_GRPC_ADDR=127.0.0.1:"+strconv.Itoa(r.ports.grpc),
		"CAS_X1_CREDENTIALS="+credentials,
		"GOCACHE="+goVars[0], "GOMODCACHE="+goVars[1], "GOPATH="+goVars[2],
	)
	secrets := []string{markerKey, markerSecret, "x1markerkey", "x1markersecret", testdb.URL(t)}

	refused := runConnect(t, r.out, env, "create")
	if refused.code == 0 || !strings.Contains(refused.out, "CreateConnection failed: failed_precondition: the key is not read-only: enableWithdrawals (KEY_NOT_READ_ONLY)") {
		t.Fatalf("create with a key that can withdraw: exit %d\n%s", refused.code, refused.out)
	}
	created := runConnect(t, r.out, env, "create")
	if created.code != 0 {
		t.Fatalf("create: exit %d\n%s", created.code, created.out)
	}
	var id string
	for _, line := range strings.Split(created.out, "\n") {
		if v, ok := strings.CutPrefix(line, "connection_id: "); ok {
			id = v
		}
	}
	for _, want := range []string{"Tenant x1-real: exists", "from the credentials file", "status: CONNECTION_STATUS_ACTIVE",
		"key_fingerprint: …" + markerKey[len(markerKey)-4:], "permissions: READ"} {
		if !strings.Contains(created.out, want) {
			t.Errorf("create output lacks %q:\n%s", want, created.out)
		}
	}
	if !strings.Contains(refused.out, "Tenant x1-real: created") || !strings.Contains(refused.out, "issued into the credentials file") {
		t.Errorf("first create output:\n%s", refused.out)
	}
	var status string
	if err := owner.QueryRow(ctx, `SELECT status FROM connections WHERE id = $1`, id).Scan(&status); err != nil || status != "ACTIVE" {
		t.Fatalf("connection %q: %s %v", id, status, err)
	}
	if info, err := os.Stat(credentials); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file: %v %v, want mode 600", info, err)
	}

	deleted := runConnect(t, r.out, env, "delete", id)
	var n int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM connections WHERE id = $1`, id).Scan(&n); err != nil || deleted.code != 0 || n != 0 {
		t.Errorf("delete: exit %d, %d rows\n%s", deleted.code, n, deleted.out)
	}

	notLocal := runConnect(t, r.out, with(without(env, "CAS_GRPC_ADDR"), "CAS_GRPC_ADDR=example.com:50053"), "create")
	if notLocal.code == 0 || !strings.Contains(notLocal.out, "CAS_GRPC_ADDR is not local") {
		t.Errorf("a remote CAS_GRPC_ADDR: exit %d\n%s", notLocal.code, notLocal.out)
	}
	noKeyEnv := filepath.Join(work, "env-without-key")
	writeEnvFile(t, noKeyEnv, map[string]string{"CASCTL_DATABASE_URL": testdb.URL(t)})
	noKey := runConnect(t, r.out, with(without(env, "CAS_ENV_FILE"), "CAS_ENV_FILE="+noKeyEnv), "create")
	if noKey.code == 0 || !strings.Contains(noKey.out, "BINANCE_API_KEY and BINANCE_API_SECRET must be set") {
		t.Errorf("without the key: exit %d\n%s", noKey.code, noKey.out)
	}
	fake.AssertAllServed()

	// No marker in any argv or environment of buf or jq (X1 D-40), in any output, or in any file left in the temporary
	// directories but the environment files the test wrote.
	logged, err := os.ReadFile(argv)
	if err != nil || !strings.Contains(string(logged), "buf curl") || !strings.Contains(string(logged), "jq -cnR") {
		t.Fatalf("the shims recorded no call of buf curl and jq: %v\n%s", err, logged)
	}
	envLogged, err := os.ReadFile(environ)
	if err != nil || !strings.Contains(string(envLogged), "PATH=") {
		t.Fatalf("the shims recorded no environment: %v", err)
	}
	noLeak(t, string(logged), secrets[:4]...)
	noLeak(t, string(envLogged), secrets[:4]...)
	noLeak(t, r.out.text(), secrets...)
	for _, dir := range []string{tmp, home, work, r.binDir} {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || path == envPath || path == noKeyEnv || filepath.Base(path) == "casctl" || filepath.Base(path) == "server" {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, s := range secrets[:2] {
				if strings.Contains(string(data), s) {
					t.Errorf("%s holds a marker", path)
				}
			}
			return nil
		})
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("%d files left in TMPDIR", len(entries))
	}
}
