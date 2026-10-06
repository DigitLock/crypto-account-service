package rehearsal

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// rig is one rehearsal: Anvil with chain ID 84532, a throwaway keys file and an environment file in a temporary
// directory outside the repository, server on the test database, and the scripts run with the rehearsal switch.
type rig struct {
	chain   *testchain.Chain
	keys    keySet
	dir     string
	binDir  string
	env     []string // of every script
	ports   struct{ grpc, health, http, cardHealth int }
	out     *outputs
	secrets []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	requireTools(t, "bash", "perl", "jq", "curl", "buf", "anvil", "forge", "cast", "go")
	testdb.Open(t)
	testdb.Clean(t)

	r := &rig{chain: startAnvil(t, baseSepolia), keys: generateKeys(t), dir: t.TempDir(), binDir: t.TempDir(), out: &outputs{}}
	fund(t, r.chain, r.keys)
	keysPath := filepath.Join(r.dir, "sepolia-keys.env")
	writeKeysFile(t, keysPath, r.keys, 0o600)

	r.ports.grpc, r.ports.health, r.ports.http, r.ports.cardHealth = freePort(t), freePort(t), freePort(t), freePort(t)
	decoy := generateKeys(t)
	envPath := filepath.Join(r.dir, "env")
	writeEnvFile(t, envPath, map[string]string{
		"CARD_AUTH_DATABASE_URL":          testdb.CardAuthURL(t),
		"CARD_AUTH_RPC_URL":               r.chain.RPCURL,
		"CARD_AUTH_HTTP_PORT":             strconv.Itoa(r.ports.http),
		"CARD_AUTH_HEALTH_PORT":           strconv.Itoa(r.ports.cardHealth),
		"CARD_AUTH_TRACKER_INTERVAL":      "250ms",
		"CARD_AUTH_RETURN_RETRY_INTERVAL": "2s",
		"CARD_AUTH_SHUTDOWN_TIMEOUT":      "5s",
		"CASCTL_DATABASE_URL":             testdb.URL(t),
		"LOG_LEVEL":                       "debug",
		// Values the script must override: an operator key that holds no role, and the local chain.
		"OPERATOR_PRIVATE_KEY": "0x" + decoy.hex("OPERATOR"),
		"CARD_AUTH_CHAIN_ID":   "31337",
	})
	r.secrets = append(r.keys.secrets(), decoy.hex("OPERATOR"), testdb.URL(t), testdb.ServerURL(t), testdb.CardAuthURL(t))

	r.env = with(baseEnv(),
		"CAS_SEPOLIA_REHEARSAL=1",
		"CAS_SEPOLIA_KEYS="+keysPath,
		"CAS_SEPOLIA_RPC_URL="+r.chain.RPCURL,
		"CAS_SEPOLIA_FALLBACK_URL="+r.chain.RPCURL,
		"CAS_ENV_FILE="+envPath,
		"CAS_BIN_DIR="+r.binDir,
		"CAS_GRPC_ADDR=127.0.0.1:"+strconv.Itoa(r.ports.grpc),
	)
	return r
}

// startServer builds and starts server on the test database as cas_server, with a generated master key.
func (r *rig) startServer(t *testing.T) {
	t.Helper()
	build := exec.Command("go", "build", "-o", r.binDir+string(os.PathSeparator), "./cmd/server")
	build.Dir = repoRoot()
	build.Env = baseEnv()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	masterKey := base64.StdEncoding.EncodeToString(master)
	r.secrets = append(r.secrets, masterKey)
	r.startProcess(t, "server", filepath.Join(r.binDir, "server"), with(baseEnv(),
		"DATABASE_URL="+testdb.ServerURL(t),
		"CAS_MASTER_KEY="+masterKey,
		"GRPC_PORT="+strconv.Itoa(r.ports.grpc),
		"HEALTH_HTTP_PORT="+strconv.Itoa(r.ports.health),
	), r.ports.health)
}

// startProcess starts a process, collects its output and waits for /healthz on healthPort; it stops on cleanup.
func (r *rig) startProcess(t *testing.T, name, path string, env []string, healthPort int) *syncBuffer {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Dir = repoRoot()
	cmd.Env = env
	logs := &syncBuffer{}
	r.out.add(logs)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		select {
		case <-exited:
			return
		default:
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			t.Errorf("%s did not stop within 15 s of SIGTERM", name)
		}
	})
	deadline := time.Now().Add(2 * time.Minute)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(healthPort) + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return logs
			}
		}
		select {
		case <-exited:
			t.Fatalf("%s exited before it served:\n%s", name, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer /healthz within 2 min:\n%s", name, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// must runs a script and requires exit 0.
func (r *rig) must(t *testing.T, name string, extra ...string) result {
	t.Helper()
	res := runScript(t, r.out, name, with(r.env, extra...))
	t.Logf("%s: exit %d in %s\n%s", name, res.code, res.took.Round(time.Millisecond), res.out)
	if res.code != 0 {
		t.Fatalf("%s failed with exit %d", name, res.code)
	}
	return res
}

// fileValue reads NAME=value of a file written by a script.
func fileValue(t *testing.T, path, name string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, name+"="); ok {
			return v
		}
	}
	t.Fatalf("%s has no %s", filepath.Base(path), name)
	return ""
}

// sim runs casctl sim, built by the scripts, with the processor pair of the credentials file.
func (r *rig) sim(t *testing.T, args ...string) map[string]any {
	t.Helper()
	creds := filepath.Join(r.dir, "cas-sepolia-credentials.env")
	cmd := exec.Command(filepath.Join(r.binDir, "casctl"), append([]string{"sim"}, args...)...)
	cmd.Env = []string{
		"CASCTL_CARD_AUTH_URL=http://127.0.0.1:" + strconv.Itoa(r.ports.http),
		"CASCTL_PROCESSOR_USERNAME=" + fileValue(t, creds, "CASCTL_PROCESSOR_USERNAME"),
		"CASCTL_PROCESSOR_PASSWORD=" + fileValue(t, creds, "CASCTL_PROCESSOR_PASSWORD"),
	}
	out := &syncBuffer{}
	r.out.add(out)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		t.Fatalf("casctl sim %v: %v\n%s", args, err, out.String())
	}
	var line struct {
		Status int            `json:"status"`
		Body   map[string]any `json:"body"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &line); err != nil {
		t.Fatalf("casctl sim %v: %v\n%s", args, err, out.String())
	}
	if line.Status != http.StatusOK {
		t.Fatalf("casctl sim %v: HTTP %d %v", args, line.Status, line.Body)
	}
	return line.Body
}

// tokenBalance reads balanceOf of MockUSDC in base units.
func (r *rig) tokenBalance(t *testing.T, token, holder string) string {
	t.Helper()
	cmd := exec.Command("cast", "call", token, "balanceOf(address)(uint256)", holder)
	cmd.Env = with(baseEnv(), "ETH_RPC_URL="+r.chain.RPCURL)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cast call balanceOf: %v", err)
	}
	return strings.Fields(string(out))[0]
}

// TestRehearsal_BaseSepoliaScripts runs every script of scripts/sepolia end to end on Anvil with chain ID 84532:
// deploy, setup (twice: idempotent), register (twice: resumes), card-auth without the listener, T805 in short
// (authorize 5 USD, return 2 USD, finality by the tag finalized) and measure.sh with N 10.
func TestRehearsal_BaseSepoliaScripts(t *testing.T) {
	r := newRig(t)
	before := gitStatus(t)
	r.startServer(t)

	r.must(t, "deploy.sh")
	deployment := filepath.Join(r.dir, "cas-sepolia-deployment.env")
	token, controller := fileValue(t, deployment, "TOKEN"), fileValue(t, deployment, "CONTROLLER")
	if res := runScript(t, r.out, "deploy.sh", r.env); res.code == 0 || !strings.Contains(res.out, "cas-sepolia-deployment.env exists") {
		t.Errorf("a second deploy.sh: exit %d, want a refusal\n%s", res.code, res.out)
	}

	r.must(t, "setup.sh")
	again := r.must(t, "setup.sh")
	for _, step := range []string{"1. mint", "2. approve", "4. refund allowance"} {
		if !strings.Contains(again.out, step+": ") || !strings.Contains(again.out, "nothing sent") {
			t.Errorf("the second setup.sh sent step %q again", step)
		}
	}

	r.must(t, "register.sh")
	creds := filepath.Join(r.dir, "cas-sepolia-credentials.env")
	if fi, err := os.Stat(creds); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v, mode %v; want 0600", err, fi.Mode().Perm())
	}
	r.secrets = append(r.secrets, fileValue(t, creds, "CASCTL_PROCESSOR_PASSWORD"), fileValue(t, creds, "CAS_SERVICE_TOKEN"))
	if res := r.must(t, "register.sh"); !strings.Contains(res.out, "created before") || !strings.Contains(res.out, "5. card card_A") {
		t.Errorf("a second register.sh did not resume:\n%s", res.out)
	}

	// run-card-auth.sh execs card-auth: the process of the script is card-auth.
	cardAuthStart := time.Now()
	cardAuth := r.startProcess(t, "run-card-auth.sh", scriptPath("run-card-auth.sh"), r.env, r.ports.cardHealth)
	t.Logf("run-card-auth.sh: card-auth served /healthz after %s", time.Since(cardAuthStart).Round(time.Millisecond))

	// T805 in short.
	start := time.Now()
	auth := r.sim(t, "authorize", "--auth-id", "t805-1", "--card-ref", "card_A", "--amount", "5.00", "--currency", "USD")
	if auth["decision"] != "APPROVED" || auth["tx_hash"] == nil || auth["token_amount"] != "5000000" {
		t.Fatalf("authorize 5 USD: %v\n%s", auth, cardAuth.String())
	}
	t.Logf("T805: APPROVED in %s, tx %v", time.Since(start).Round(time.Millisecond), auth["tx_hash"])
	ret := r.sim(t, "return", "--auth-id", "t805-1", "--return-id", "t805-r1", "--type", "REFUND", "--amount", "2.00")
	if ret["status"] != "ACCEPTED" || ret["token_amount"] != "2000000" {
		t.Fatalf("return 2 USD: %v", ret)
	}

	// Anvil answers the tag finalized with the block 64 below the latest (two epochs of 32 slots). 20 blocks are more
	// than the 10 confirmations of the default rule, and not final by the tag.
	r.chain.Call(t, nil, "anvil_mine", hexutil.Uint64(20))
	time.Sleep(time.Second)
	if got := r.sim(t, "get", "--auth-id", "t805-1"); got["status"] != "APPROVED" {
		t.Fatalf("after 20 blocks the status is %v, want APPROVED until the tag finalized reaches the block", got["status"])
	}
	finalStart := time.Now()
	var got map[string]any
	for deadline := time.Now().Add(time.Minute); ; {
		r.chain.Call(t, nil, "anvil_mine", hexutil.Uint64(70))
		time.Sleep(500 * time.Millisecond)
		got = r.sim(t, "get", "--auth-id", "t805-1")
		returns, _ := got["returns"].([]any)
		if got["status"] == "DEBIT_CONFIRMED" && len(returns) == 1 && returns[0].(map[string]any)["status"] == "CONFIRMED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("T805: not final within a minute: %v\n%s", got, cardAuth.String())
		}
	}
	t.Logf("T805: DEBIT_CONFIRMED and the return CONFIRMED %s after the first 70 blocks", time.Since(finalStart).Round(time.Millisecond))
	if got["returned_amount"] != "2000000" {
		t.Errorf("returned_amount %v, want 2000000", got["returned_amount"])
	}
	user, treasury := fileValue(t, deployment, "USER"), fileValue(t, deployment, "TREASURY")
	if b := r.tokenBalance(t, token, user); b != "97000000" {
		t.Errorf("USER holds %s base units, want 97000000: 100 − 5 + 2", b)
	}
	if b := r.tokenBalance(t, token, treasury); b != "3000000" {
		t.Errorf("TREASURY holds %s base units, want 3000000", b)
	}
	if b := r.tokenBalance(t, token, controller); b != "0" {
		t.Errorf("the controller holds %s base units", b)
	}

	res := r.must(t, "measure.sh", "CAS_MEASURE_N=10")
	for _, want := range []string{"approvals                 10", "N                         10", "polling 10", "PASS"} {
		if !strings.Contains(res.out, want) {
			t.Errorf("measure.sh: no %q in the summary", want)
		}
	}

	noLeak(t, r.out.text(), r.secrets...)
	if after := gitStatus(t); after != before {
		t.Errorf("the rehearsal changed the working tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRehearsal_Refusals proves the refusals of every script: CAS_SEPOLIA_KEYS unset, a missing keys file, a keys
// file inside the repository, a keys file others can read, chain ID 1, and the rehearsal switch: a public URL in a
// rehearsal, a local URL outside one. No transaction is sent.
func TestRehearsal_Refusals(t *testing.T) {
	requireTools(t, "bash", "perl", "cast")
	local := startAnvil(t, baseSepolia)
	mainnet := startAnvil(t, 1)
	keys := generateKeys(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "keys.env")
	writeKeysFile(t, good, keys, 0o600)
	readable := filepath.Join(t.TempDir(), "keys.env")
	writeKeysFile(t, readable, keys, 0o644)

	// Inside the repository: in bin/, which git ignores, removed at the end. It holds no key: the scripts refuse the
	// place before they read the file.
	inRepoDir, err := os.MkdirTemp(filepath.Join(repoRoot(), "bin"), "rehearsal-")
	if err != nil {
		if err := os.MkdirAll(filepath.Join(repoRoot(), "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if inRepoDir, err = os.MkdirTemp(filepath.Join(repoRoot(), "bin"), "rehearsal-"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(inRepoDir) })
	inRepo := filepath.Join(inRepoDir, "keys.env")
	if err := os.WriteFile(inRepo, []byte("# no key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	envFile := func(rpc string) string {
		path := filepath.Join(t.TempDir(), "env")
		writeEnvFile(t, path, map[string]string{"CARD_AUTH_RPC_URL": rpc})
		return path
	}
	localEnv, mainnetEnv := envFile(local.RPCURL), envFile(mainnet.RPCURL)
	rehearsal := func(keys, rpc, envPath string) []string {
		env := with(baseEnv(), "CAS_SEPOLIA_REHEARSAL=1", "CAS_SEPOLIA_RPC_URL="+rpc, "CAS_SEPOLIA_FALLBACK_URL="+rpc,
			"CAS_ENV_FILE="+envPath, "CAS_BIN_DIR="+t.TempDir())
		if keys != "" {
			env = append(env, "CAS_SEPOLIA_KEYS="+keys)
		}
		return env
	}

	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"CAS_SEPOLIA_KEYS unset", rehearsal("", local.RPCURL, localEnv), "CAS_SEPOLIA_KEYS is not set"},
		{"keys file missing", rehearsal(filepath.Join(dir, "none.env"), local.RPCURL, localEnv), "does not exist"},
		{"keys file inside the repository", rehearsal(inRepo, local.RPCURL, localEnv), "inside the repository working tree"},
		{"keys file readable by others", rehearsal(readable, local.RPCURL, localEnv), "has mode 0644"},
		{"chain ID 1", rehearsal(good, mainnet.RPCURL, mainnetEnv), "answers chain ID 1: only 84532"},
		{"public URL in a rehearsal", with(baseEnv(), "CAS_SEPOLIA_REHEARSAL=1", "CAS_SEPOLIA_KEYS="+good,
			"CAS_ENV_FILE="+envFile("https://sepolia.base.org")), "is not a local URL"},
		{"local URL without the rehearsal switch", with(baseEnv(), "CAS_SEPOLIA_KEYS="+good, "CAS_SEPOLIA_RPC_URL="+local.RPCURL,
			"CAS_ENV_FILE="+localEnv), "is a local URL"},
	}
	for _, c := range cases {
		for _, script := range scripts {
			res := runScript(t, nil, script, c.env)
			if res.code == 0 || !strings.Contains(res.out, c.want) {
				t.Errorf("%s, %s: exit %d, want a refusal with %q:\n%s", c.name, script, res.code, c.want, res.out)
			}
			noLeak(t, res.out, keys.secrets()...)
		}
	}

	for _, chain := range []*testchain.Chain{local, mainnet} {
		var block hexutil.Uint64
		chain.Call(t, &block, "eth_blockNumber")
		if block != 0 {
			t.Errorf("a refused script mined %d blocks", block)
		}
	}
}
