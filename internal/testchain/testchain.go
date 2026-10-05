// Package testchain gives tests a local chain (docs/test-plan-s2.md §1): Anvil on a free port with
// MockUSDC and CardSpendController deployed by the deployment script of contracts/, as contracts/README.md
// describes. It is used only by tests.
//
// anvil and forge must be on PATH. Without them a test is skipped locally and fails in CI, where the
// variable CI is set. No key is written anywhere: the script runs with Anvil's unlocked accounts
// (DEPLOYER, ADMIN and TREASURY are accounts 0, 1 and 3), and the operator key is generated at run time.
package testchain

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// ChainID is the chain ID of the local chain.
const ChainID = 31337

// TokenDecimals is the decimals of MockUSDC.
const TokenDecimals = 6

// operatorBalance is the gas money of the operator: 100 ETH in wei.
var operatorBalance = hexutil.MustDecodeBig("0x56bc75e2d63100000")

// Chain is a local chain with the deployed contracts.
type Chain struct {
	RPCURL      string
	Token       common.Address
	Controller  common.Address
	Deployer    common.Address
	Admin       common.Address
	Treasury    common.Address
	Operator    common.Address
	OperatorKey vault.Secret[[]byte]
}

// Start starts Anvil with chain ID 31337, generates and funds the operator, and deploys the contracts.
// Anvil stops on cleanup.
func Start(t testing.TB) *Chain {
	t.Helper()
	forge := tool(t, "forge")
	rpcURL := StartAnvil(t, ChainID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := dial(t, rpcURL)
	var accounts []common.Address
	if err := client.CallContext(ctx, &accounts, "eth_accounts"); err != nil || len(accounts) < 4 {
		t.Fatalf("testchain: eth_accounts: %v (%d accounts)", err, len(accounts))
	}

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &Chain{
		RPCURL:      rpcURL,
		Deployer:    accounts[0],
		Admin:       accounts[1],
		Treasury:    accounts[3],
		Operator:    crypto.PubkeyToAddress(key.PublicKey),
		OperatorKey: vault.NewSecret(crypto.FromECDSA(key)),
	}
	if err := client.CallContext(ctx, nil, "anvil_setBalance", c.Operator, (*hexutil.Big)(operatorBalance)); err != nil {
		t.Fatalf("testchain: fund the operator: %v", err)
	}
	c.Token, c.Controller = deploy(t, forge, c)
	return c
}

// StartAnvil starts a bare Anvil with chainID on a free loopback port and returns its RPC URL.
// Anvil stops on cleanup.
func StartAnvil(t testing.TB, chainID uint64) string {
	t.Helper()
	anvil := tool(t, "anvil")
	port := freePort(t)
	cmd := exec.Command(anvil, "--chain-id", strconv.FormatUint(chainID, 10),
		"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--silent")
	cmd.Env = childEnv()
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("testchain: start anvil: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})

	rpcURL := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	client := dial(t, rpcURL)
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var id hexutil.Uint64
		err := client.CallContext(ctx, &id, "eth_chainId")
		cancel()
		if err == nil {
			if uint64(id) != chainID {
				t.Fatalf("testchain: anvil serves chain ID %d, want %d", id, chainID)
			}
			return rpcURL
		}
		select {
		case <-exited:
			t.Fatalf("testchain: anvil exited: %s", out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("testchain: anvil did not answer within 15 s: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ContractsDir returns the directory of the Foundry project.
func ContractsDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testchain: cannot locate the source file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "contracts")
}

var deployed = regexp.MustCompile(`(?m)^\s*(TOKEN|CONTROLLER)=(0x[0-9a-fA-F]{40})\s*$`)

// deploy builds the contracts and runs script/Deploy.s.sol with the unlocked DEPLOYER. TOKEN is the zero
// address, so the script deploys MockUSDC first.
func deploy(t testing.TB, forge string, c *Chain) (token, controller common.Address) {
	t.Helper()
	dir := ContractsDir(t)
	env := append(childEnv(),
		"DEPLOYER="+c.Deployer.Hex(),
		"ADMIN="+c.Admin.Hex(),
		"OPERATOR="+c.Operator.Hex(),
		"TREASURY="+c.Treasury.Hex(),
		"TOKEN="+common.Address{}.Hex(),
	)
	run := func(args ...string) string {
		cmd := exec.Command(forge, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("testchain: forge %s: %v\n%s", args[0], err, out)
		}
		return string(out)
	}
	run("build")
	out := run("script", "script/Deploy.s.sol", "--rpc-url", c.RPCURL, "--broadcast", "--unlocked", "--sender", c.Deployer.Hex())

	found := map[string]common.Address{}
	for _, m := range deployed.FindAllStringSubmatch(out, -1) {
		found[m[1]] = common.HexToAddress(m[2])
	}
	if found["TOKEN"] == (common.Address{}) || found["CONTROLLER"] == (common.Address{}) {
		t.Fatalf("testchain: no TOKEN= and CONTROLLER= lines in the output of the deployment script:\n%s", out)
	}
	return found["TOKEN"], found["CONTROLLER"]
}

// tool returns the path of name on PATH. Without it the test is skipped locally and fails in CI.
func tool(t testing.TB, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal(name + " is not on PATH; it is required in CI")
		}
		t.Skip(name + " is not on PATH")
	}
	return path
}

// childEnv is the environment of anvil and forge: only what they need to run. GOFLAGS and the variables of
// .env, which make exports to the tests, are not passed (docs/backlog.md item 4).
func childEnv() []string {
	var env []string
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, "NO_COLOR=1")
}

func dial(t testing.TB, rpcURL string) *rpc.Client {
	t.Helper()
	client, err := rpc.Dial(rpcURL)
	if err != nil {
		t.Fatalf("testchain: dial anvil: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// syncBuffer collects the output of anvil, written by the process and read by the test.
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
