package rehearsal

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

var ctx = context.Background()

// baseSepolia is the chain ID the scripts accept; the rehearsal runs Anvil with it.
const baseSepolia = 84532

// roles are the five accounts of the keys file, in the order of the Deployment Guide.
var roles = []string{"DEPLOYER", "ADMIN", "OPERATOR", "TREASURY", "USER"}

// scripts are the scripts of scripts/sepolia.
var scripts = []string{"deploy.sh", "setup.sh", "register.sh", "run-card-auth.sh", "measure.sh"}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func scriptPath(name string) string { return filepath.Join(repoRoot(), "scripts", "sepolia", name) }

// requireTools skips the test locally and fails it in CI, where the variable CI is set, when a tool is missing.
func requireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatal(name + " is not on PATH; it is required in CI")
			}
			t.Skip(name + " is not on PATH")
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// baseEnv is the environment of every child process: what the tools need to run, never a variable of .env, which
// make exports to the tests.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "PATH", name == "HOME", name == "TMPDIR", name == "USER", name == "LANG",
			name == "XDG_CACHE_HOME", name == "XDG_CONFIG_HOME":
			env = append(env, kv)
		case strings.HasPrefix(name, "GO") && name != "GOFLAGS":
			env = append(env, kv)
		}
	}
	return append(env, "NO_COLOR=1")
}

// keySet is a generated set of the five accounts; nothing of it is written anywhere but the throwaway keys file.
type keySet struct {
	keys map[string]*ecdsa.PrivateKey
}

func generateKeys(t *testing.T) keySet {
	t.Helper()
	k := keySet{keys: map[string]*ecdsa.PrivateKey{}}
	for _, role := range roles {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		k.keys[role] = key
	}
	return k
}

func (k keySet) hex(role string) string { return hex.EncodeToString(crypto.FromECDSA(k.keys[role])) }

func (k keySet) address(role string) common.Address {
	return crypto.PubkeyToAddress(k.keys[role].PublicKey)
}

// secrets returns the forms of the keys a search for leaks looks for.
func (k keySet) secrets() []string {
	var out []string
	for _, role := range roles {
		out = append(out, k.hex(role))
	}
	return out
}

// writeKeysFile writes the keys file in the format of the Deployment Guide with the given mode.
func writeKeysFile(t *testing.T, path string, k keySet, mode os.FileMode) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# Throwaway keys of the rehearsal, generated at run time.\n")
	for _, role := range roles {
		b.WriteString(role + "_PRIVATE_KEY=0x" + k.hex(role) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// fund sends 10 ETH to every account of the set from the first account of Anvil.
func fund(t *testing.T, chain *testchain.Chain, k keySet) {
	t.Helper()
	var accounts []common.Address
	chain.Call(t, &accounts, "eth_accounts")
	ten := (*hexutil.Big)(new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18)))
	for _, role := range roles {
		var hash common.Hash
		chain.Call(t, &hash, "eth_sendTransaction", map[string]any{"from": accounts[0], "to": k.address(role), "value": ten})
	}
}

// startAnvil starts Anvil with chainID and returns a client of it.
func startAnvil(t *testing.T, chainID uint64) *testchain.Chain {
	t.Helper()
	return &testchain.Chain{RPCURL: testchain.StartAnvil(t, chainID)}
}

// writeEnvFile writes an environment file of the repository's format: NAME=value lines.
func writeEnvFile(t *testing.T, path string, vars map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# Environment file of the rehearsal.\n")
	for _, k := range keys {
		b.WriteString(k + "=" + vars[k] + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// outputs collects every output of a script and of a process of the rehearsal, for the search for leaks.
type outputs struct {
	mu   sync.Mutex
	bufs []*syncBuffer
}

func (o *outputs) add(b *syncBuffer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bufs = append(o.bufs, b)
}

func (o *outputs) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var b strings.Builder
	for _, buf := range o.bufs {
		b.WriteString(buf.String())
		b.WriteString("\n")
	}
	return b.String()
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// result is one run of a script.
type result struct {
	out  string
	code int
	took time.Duration
}

// runScript runs a script of scripts/sepolia with env and waits for it, at most 5 minutes.
func runScript(t *testing.T, o *outputs, name string, env []string) result {
	t.Helper()
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, scriptPath(name))
	cmd.Dir = repoRoot()
	cmd.Env = env
	out := &syncBuffer{}
	if o != nil {
		o.add(out)
	}
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	r := result{out: out.String(), took: time.Since(start)}
	if err != nil {
		r.code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
		}
	}
	return r
}

// with returns env with the extra variables appended.
func with(env []string, extra ...string) []string {
	return append(append([]string{}, env...), extra...)
}

// gitStatus returns the status of the working tree, ignored files left out.
func gitStatus(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = repoRoot()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return string(out)
}

// noLeak fails when any secret appears in text, in any letter case.
func noLeak(t *testing.T, text string, secrets ...string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, s := range secrets {
		if s != "" && strings.Contains(lower, strings.ToLower(s)) {
			t.Errorf("a secret of %d characters appears in the output", len(s))
		}
	}
}
