package donewhen

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// Harness of phase 7: the binaries of server, card-auth and casctl are built once by TestMain. The three rows share
// one world, built by the first of them that runs (scene): T702 breaks the chain and runs last. The world lives until
// the end of the package: its cleanups run in TestMain after the last test.

var ctx = context.Background()

// bin holds the paths of the built binaries.
var bin struct {
	server, cardAuth, casctl string
}

// kept holds the cleanups of the shared world, run in reverse order after the last test.
var kept struct {
	mu  sync.Mutex
	fns []func()
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "donewhen-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "donewhen:", err)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", dir+string(os.PathSeparator), "./cmd/server", "./cmd/card-auth", "./cmd/casctl")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "donewhen: build the binaries: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	bin.server, bin.cardAuth, bin.casctl = filepath.Join(dir, "server"), filepath.Join(dir, "card-auth"), filepath.Join(dir, "casctl")
	code := m.Run()
	kept.mu.Lock()
	for i := len(kept.fns) - 1; i >= 0; i-- {
		kept.fns[i]()
	}
	kept.mu.Unlock()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// keeper is the testing.TB that builds the shared world: everything but Cleanup goes to the test that builds it;
// a cleanup is kept until the end of the package.
type keeper struct{ testing.TB }

func (keeper) Cleanup(f func()) {
	kept.mu.Lock()
	defer kept.mu.Unlock()
	kept.fns = append(kept.fns, f)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// usdc returns n whole USDC in base units.
func usdc(n int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000))
}

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func freePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// process is a running binary.
type process struct {
	name   string
	cmd    *exec.Cmd
	logs   *syncBuffer
	exited chan struct{}
}

// startProcess runs path with exactly env, waits for /healthz on healthPort and stops it on cleanup of t.
func startProcess(t testing.TB, name, path string, env map[string]string, healthPort int) *process {
	t.Helper()
	cmd := exec.Command(path)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	p := &process{name: name, cmd: cmd, logs: &syncBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	// The cleanup may run after the test that started the process: it reports on stderr, not through t.
	t.Cleanup(func() {
		if !p.stop() {
			fmt.Fprintf(os.Stderr, "donewhen: %s did not stop within 15 s of SIGTERM\n", name)
		}
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(healthPort) + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return p
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("%s exited before it served:\n%s", name, p.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer /healthz within 30 s:\n%s", name, p.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop sends SIGTERM and waits for the exit; after 15 s it kills the process and returns false.
func (p *process) stop() bool {
	select {
	case <-p.exited:
		return true
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
		return true
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
		return false
	}
}

// runCasctl runs casctl with exactly env and requires exit 0; it returns stdout.
func runCasctl(t testing.TB, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin.casctl, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("casctl %s: %v\n%s", args[0], err, errOut.String())
	}
	return out.String()
}

// simAnswer is one output line of casctl sim.
type simAnswer struct {
	Status int            `json:"status"`
	Body   map[string]any `json:"body"`
}

// grpcClient dials the gRPC port of server; the connection closes on cleanup of t.
func grpcClient(t testing.TB, port int) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("127.0.0.1:"+strconv.Itoa(port), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// bearer is the call context of a tenant's service token.
func bearer(token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// metricsText returns /metrics of a health port.
func metricsText(t testing.TB, port int) string {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

var labelPair = regexp.MustCompile(`(\w+)="([^"]*)"`)

// metricValue returns the value of the series of name whose labels include want, or false when there is none.
func metricValue(text, name string, want map[string]string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		rest, ok := strings.CutPrefix(line, name+"{")
		if !ok {
			continue
		}
		labels, value, ok := strings.Cut(rest, "} ")
		if !ok {
			continue
		}
		got := map[string]string{}
		for _, m := range labelPair.FindAllStringSubmatch(labels, -1) {
			got[m[1]] = m[2]
		}
		match := true
		for k, v := range want {
			if got[k] != v {
				match = false
			}
		}
		if match {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// logLine is a JSON log line of server.
type logLine map[string]any

func (l logLine) str(key string) string {
	s, _ := l[key].(string)
	return s
}

// logLines decodes the JSON lines of a process log; other lines are left out.
func logLines(text string) []logLine {
	var out []logLine
	for _, raw := range strings.Split(text, "\n") {
		var l logLine
		if json.Unmarshal([]byte(raw), &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// masterKey returns a generated CAS_MASTER_KEY: standard base64 of 32 random bytes.
func masterKey(t testing.TB) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(randomBytes(t, 32))
}

// deploymentBlock returns the first block in which addr has code: the deployment block of the contract.
func deploymentBlock(t testing.TB, c *testchain.Chain, addr common.Address) uint64 {
	t.Helper()
	head := c.Head(t)
	for n := uint64(0); n <= head; n++ {
		var code hexutil.Bytes
		c.Call(t, &code, "eth_getCode", addr, hexutil.EncodeUint64(n))
		if len(code) > 0 {
			return n
		}
	}
	t.Fatalf("%s has no code up to block %d", addr.Hex(), head)
	return 0
}

// blockOf returns the block number of a mined transaction.
func blockOf(t testing.TB, c *testchain.Chain, hash common.Hash) uint64 {
	t.Helper()
	var receipt struct {
		BlockNumber hexutil.Uint64 `json:"blockNumber"`
	}
	c.Call(t, &receipt, "eth_getTransactionReceipt", hash)
	if receipt.BlockNumber == 0 {
		t.Fatalf("no receipt of %s", hash.Hex())
	}
	return uint64(receipt.BlockNumber)
}

// blockHash returns the hash of block n, or the zero hash when the chain has no such block.
func blockHash(t testing.TB, c *testchain.Chain, n uint64) common.Hash {
	t.Helper()
	var header *struct {
		Hash common.Hash `json:"hash"`
	}
	c.Call(t, &header, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false)
	if header == nil {
		return common.Hash{}
	}
	return header.Hash
}

// hexID returns 32 random bytes and their 0x form.
func hexID(t testing.TB) ([32]byte, string) {
	t.Helper()
	var id [32]byte
	copy(id[:], randomBytes(t, 32))
	return id, "0x" + hex.EncodeToString(id[:])
}
