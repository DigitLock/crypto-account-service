package hardtest

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
)

// Harness of phase 7: the binaries of card-auth and casctl are built once by TestMain. Every test has its own world:
// a fresh Anvil with the deployed contracts and a generated operator key, its own tenant and Basic pair on the
// shared test database, and card-auth started as a process. Every log line of card-auth and every output of casctl
// is collected for S2-T710, with every secret of the phase.

var ctx = context.Background()

// bin holds the paths of the built binaries.
var bin struct {
	cardAuth, casctl string
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hardtest-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "hardtest:", err)
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-o", dir+string(os.PathSeparator), "./cmd/card-auth", "./cmd/casctl")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "hardtest: build the binaries: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	bin.cardAuth, bin.casctl = filepath.Join(dir, "card-auth"), filepath.Join(dir, "casctl")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// collected is what S2-T710 searches: the logs of every card-auth process and the output of every casctl run of
// the phase, and the secrets of every world.
var collected struct {
	mu      sync.Mutex
	outputs []*syncBuffer
	secrets []string
	pairs   []registry.IssuedPair
	clean   sync.Once
}

func collect(b *syncBuffer) {
	collected.mu.Lock()
	defer collected.mu.Unlock()
	collected.outputs = append(collected.outputs, b)
}

func addSecrets(s ...string) {
	collected.mu.Lock()
	defer collected.mu.Unlock()
	collected.secrets = append(collected.secrets, s...)
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

// phaseDB returns the owner pool of the test database. The first test of the process empties it; later tests keep
// the rows of earlier ones, so that S2-T710 finds the database of the whole phase.
func phaseDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.Open(t)
	collected.clean.Do(func() { testdb.Clean(t) })
	return pool
}

func usdc(t *testing.T, s string) *big.Int {
	t.Helper()
	n, err := decision.TokenAmount(s, "", 0, testchain.TokenDecimals)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
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

// worldOptions change a world.
type worldOptions struct {
	noRefundAllowance bool // the test sets the treasury allowance itself (T707)
}

// world is one scenario: a chain, a tenant with its Basic pair, and card-auth.
type world struct {
	chain    *testchain.Chain
	owner    *pgxpool.Pool
	tenantID uuid.UUID
	pair     registry.IssuedPair
	env      map[string]string
	ca       *process
	// dbTrap is CASCTL_DATABASE_URL of every casctl run: a listener that counts connections.
	dbTrap   string
	trapHits *atomic.Int32
}

func newWorld(t *testing.T, o worldOptions) *world {
	t.Helper()
	owner := phaseDB(t)
	c := testchain.Start(t)
	if !o.noRefundAllowance {
		c.ApproveRefunds(t, usdc(t, "1000000"))
	}
	reg := registry.New(owner)
	name := "tenant-" + randomHex(t, 4)
	tenant, err := reg.CreateTenant(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := reg.IssueProcessorCredential(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{chain: c, owner: owner, tenantID: tenant.ID, pair: pair, trapHits: &atomic.Int32{}}
	key := hex.EncodeToString(c.OperatorKey.Value())
	w.env = map[string]string{
		"CARD_AUTH_DATABASE_URL":           testdb.CardAuthURL(t),
		"OPERATOR_PRIVATE_KEY":             "0x" + key,
		"CARD_AUTH_CHAIN_ID":               strconv.Itoa(testchain.ChainID),
		"CARD_AUTH_RPC_URL":                c.RPCURL,
		"CARD_AUTH_CONTROLLER_ADDRESS":     c.Controller.Hex(),
		"CARD_AUTH_TOKEN_ADDRESS":          c.Token.Hex(),
		"CARD_AUTH_TOKEN_DECIMALS":         strconv.Itoa(testchain.TokenDecimals),
		"CARD_AUTH_HTTP_PORT":              strconv.Itoa(freePort(t)),
		"CARD_AUTH_HEALTH_PORT":            strconv.Itoa(freePort(t)),
		"CARD_AUTH_SHUTDOWN_TIMEOUT":       "5s",
		"CARD_AUTH_TRACKER_INTERVAL":       "250ms",
		"CARD_AUTH_RECEIPT_POLL_INTERVAL":  "50ms",
		"CARD_AUTH_FINALITY_CONFIRMATIONS": "2",
		"CARD_AUTH_RETURN_RETRY_INTERVAL":  "2s",
		"LOG_LEVEL":                        "debug",
	}
	addSecrets(key, strings.ToUpper(key), pair.Password, strings.ToUpper(pair.Password))
	collected.mu.Lock()
	collected.pairs = append(collected.pairs, pair)
	collected.mu.Unlock()

	trap, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = trap.Close() })
	go func() {
		for {
			conn, err := trap.Accept()
			if err != nil {
				return
			}
			w.trapHits.Add(1)
			_ = conn.Close()
		}
	}()
	w.dbTrap = "postgres://owner:" + randomHex(t, 8) + "@" + trap.Addr().String() + "/cas"
	return w
}

// fundedCard adds a card of the tenant bound to a new wallet with 100 USDC, allowance 100 USDC and a wallet daily
// limit of 50 USDC; the card's daily limit is 200 USDC.
func (w *world) fundedCard(t *testing.T, cardRef string) common.Address {
	t.Helper()
	wallet := w.chain.NewWallet(t)
	w.chain.Mint(t, wallet, usdc(t, "100"))
	w.chain.Approve(t, wallet, usdc(t, "100"))
	w.chain.SetDailyLimit(t, wallet, usdc(t, "50"))
	w.addCard(t, cardRef, wallet)
	return wallet
}

// addCard registers a card of the tenant on a wallet connection of the local chain.
func (w *world) addCard(t *testing.T, cardRef string, wallet common.Address) {
	t.Helper()
	if _, err := w.owner.Exec(ctx, `WITH conn AS (
			INSERT INTO connections (tenant_id, owner_ref, source_id, external_account)
			VALUES ($1, 'owner-a', (SELECT id FROM sources WHERE code = 'anvil'), $2) RETURNING id)
		INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, status, daily_limit)
		SELECT $1, $3, 'owner-a', id, 'ACTIVE', 200000000 FROM conn`, w.tenantID, wallet.Hex(), cardRef); err != nil {
		t.Fatal(err)
	}
}

// process is a running card-auth.
type process struct {
	cmd    *exec.Cmd
	logs   *syncBuffer
	exited chan struct{}
}

// start runs card-auth with the env of the world and waits for /healthz. It stops on cleanup.
func (w *world) start(t *testing.T) {
	t.Helper()
	cmd := exec.Command(bin.cardAuth)
	cmd.Env = []string{}
	for k, v := range w.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	p := &process{cmd: cmd, logs: &syncBuffer{}, exited: make(chan struct{})}
	collect(p.logs)
	cmd.Stdout, cmd.Stderr = p.logs, p.logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start card-auth: %v", err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	w.ca = p
	t.Cleanup(func() { p.stop(t) })

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(w.healthURL() + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-p.exited:
			t.Fatalf("card-auth exited before it served:\n%s", p.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("card-auth did not answer /healthz within 30 s:\n%s", p.logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stop sends SIGTERM and waits for the exit.
func (p *process) stop(t *testing.T) {
	select {
	case <-p.exited:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(15 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.exited
		t.Error("card-auth did not stop within 15 s of SIGTERM")
	}
}

// kill sends SIGKILL and waits for the exit.
func (p *process) kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.exited
}

func (w *world) apiURL() string    { return "http://127.0.0.1:" + w.env["CARD_AUTH_HTTP_PORT"] }
func (w *world) healthURL() string { return "http://127.0.0.1:" + w.env["CARD_AUTH_HEALTH_PORT"] }

// cliResult is one run of casctl.
type cliResult struct {
	stdout, stderr string
	code           int
}

// answer is one output line of casctl sim.
type answer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

func (a answer) field(name string) string {
	var m map[string]any
	_ = json.Unmarshal(a.Body, &m)
	s, _ := m[name].(string)
	return s
}

// lines decodes the output lines of casctl sim.
func (r cliResult) lines(t *testing.T) []answer {
	t.Helper()
	var out []answer
	for _, l := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		if l == "" {
			continue
		}
		var a answer
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("casctl output line %q is not JSON: %v", l, err)
		}
		out = append(out, a)
	}
	return out
}

// casctl runs the casctl binary with the sim environment of the world and the database trap.
func (w *world) casctl(t *testing.T, args ...string) cliResult {
	t.Helper()
	cmd := w.casctlCmd(args...)
	out, errOut := &syncBuffer{}, &syncBuffer{}
	collect(out)
	collect(errOut)
	cmd.Stdout, cmd.Stderr = out, errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return cliResult{stdout: out.String(), stderr: errOut.String(), code: code}
}

func (w *world) casctlCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(bin.casctl, args...)
	cmd.Env = []string{
		"CASCTL_CARD_AUTH_URL=" + w.apiURL(),
		"CASCTL_PROCESSOR_USERNAME=" + w.pair.Username,
		"CASCTL_PROCESSOR_PASSWORD=" + w.pair.Password,
		"CASCTL_DATABASE_URL=" + w.dbTrap,
	}
	return cmd
}

// mustSim runs casctl and requires exit 0.
func (w *world) mustSim(t *testing.T, args ...string) []answer {
	t.Helper()
	r := w.casctl(t, args...)
	if r.code != 0 {
		t.Fatalf("casctl %v: exit %d, stderr %q", args, r.code, r.stderr)
	}
	return r.lines(t)
}

// authorizeHTTP sends POST /v1/authorizations directly, for the rows that time the answer.
func (w *world) authorizeHTTP(t *testing.T, authID, cardRef, amount string) (map[string]any, time.Time) {
	t.Helper()
	body := `{"auth_id":"` + authID + `","card_ref":"` + cardRef + `","amount":"` + amount + `","currency":"USD"}`
	req, err := http.NewRequest(http.MethodPost, w.apiURL()+"/v1/authorizations", strings.NewReader(body))
	if err != nil {
		t.Error(err)
		return nil, time.Time{}
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(w.pair.Username, w.pair.Password)
	resp, err := http.DefaultClient.Do(req)
	at := time.Now()
	if err != nil {
		t.Error(err)
		return nil, at
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, at
}

// chainAuthID is the authId of the contract for an auth_id of the tenant.
func (w *world) chainAuthID(authID string) common.Hash {
	return decision.ChainAuthID(w.tenantID, authID)
}

var debitedTopic = crypto.Keccak256Hash([]byte("Debited(bytes32,address,uint256)"))

// debited returns the number of Debited events of authID on chain.
func (w *world) debited(t *testing.T, authID string) int {
	t.Helper()
	var logs []json.RawMessage
	w.chain.Call(t, &logs, "eth_getLogs", map[string]any{
		"address": w.chain.Controller, "topics": []any{debitedTopic, w.chainAuthID(authID)}, "fromBlock": "0x0", "toBlock": "latest",
	})
	return len(logs)
}

// balance returns the token balance of addr in base units.
func (w *world) balance(t *testing.T, addr common.Address) *big.Int {
	t.Helper()
	var out hexutil.Bytes
	w.chain.Call(t, &out, "eth_call", map[string]any{
		"to": w.chain.Token, "data": hexutil.Bytes(testchain.TokenCalldata(t, "balanceOf", addr)),
	}, "latest")
	return new(big.Int).SetBytes(out)
}

// operatorNonces returns the operator's transaction count at latest and pending.
func (w *world) operatorNonces(t *testing.T) (latest, pending uint64) {
	t.Helper()
	var l, p hexutil.Uint64
	w.chain.Call(t, &l, "eth_getTransactionCount", w.chain.Operator, "latest")
	w.chain.Call(t, &p, "eth_getTransactionCount", w.chain.Operator, "pending")
	return uint64(l), uint64(p)
}

// waitPending waits until the operator has a transaction in the pool that is not mined.
func (w *world) waitPending(t *testing.T, within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if l, p := w.operatorNonces(t); p > l {
			return time.Now()
		}
		if time.Now().After(deadline) {
			t.Fatalf("no operator transaction in the pool within %s", within)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// status returns the status of an authorization of the tenant.
func (w *world) status(t *testing.T, authID string) string {
	t.Helper()
	var s string
	if err := w.owner.QueryRow(ctx, `SELECT status FROM authorizations WHERE tenant_id = $1 AND auth_id = $2`,
		w.tenantID, authID).Scan(&s); err != nil {
		t.Fatalf("status of %s: %v", authID, err)
	}
	return s
}

// waitStatus waits until the authorization reaches one of want.
func (w *world) waitStatus(t *testing.T, authID string, within time.Duration, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		s := w.status(t, authID)
		for _, v := range want {
			if s == v {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s after %s, want one of %v\n%s", authID, s, within, want, w.ca.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (w *world) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.owner.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// noNonceGap checks the one nonce sequence of the operator of the world: next_nonce equals its count on chain, no
// PLANNED slot, no gap.
func (w *world) noNonceGap(t *testing.T) {
	t.Helper()
	op := w.chain.Operator.Bytes()
	next := w.count(t, `SELECT next_nonce FROM operator_accounts WHERE chain_id = $1 AND address = $2`, testchain.ChainID, op)
	latest, pending := w.operatorNonces(t)
	if uint64(next) != latest || pending != latest {
		t.Errorf("next_nonce %d, the operator's count on chain %d (pending %d)", next, latest, pending)
	}
	if n := w.count(t, `SELECT count(*) FROM operator_txs WHERE operator_address = $1 AND status = 'PLANNED'`, op); n != 0 {
		t.Errorf("%d PLANNED slots left", n)
	}
	if n := w.count(t, `SELECT count(DISTINCT nonce) FROM operator_txs WHERE operator_address = $1`, op); n !=
		next-w.count(t, `SELECT COALESCE(min(nonce), 0) FROM operator_txs WHERE operator_address = $1`, op) {
		t.Errorf("%d distinct nonces below next_nonce %d: a gap", n, next)
	}
}

// metrics returns /metrics of card-auth.
func (w *world) metrics(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(w.healthURL() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// settle mines a block per tick until the condition holds: blocks on top for the finality rule, inclusion of the
// refunds of the tracker.
func (w *world) settle(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s\n%s", what, within, w.ca.logs.String())
		}
		w.chain.Mine(t)
		time.Sleep(100 * time.Millisecond)
	}
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dbText returns every row of every table of the public schema as text.
func dbText(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	var b strings.Builder
	for _, table := range tables {
		rows, err := pool.Query(ctx, `SELECT t::text FROM `+table+` t`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(table + " " + line + "\n")
		}
		rows.Close()
	}
	return b.String()
}
