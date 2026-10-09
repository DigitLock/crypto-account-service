package evm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// The test vectors of the EIP-55 text: all caps, all lower, mixed. The only real addresses of the repository.
var eip55Vectors = []string{
	"0x52908400098527886E0F7030069857D2E4169EE7",
	"0x8617E340B3D01FA5F11F306F4090FD50E238070D",
	"0xde709f2102306220921060314715629080e2fb77",
	"0x27b1fdb04752bbc536007a920d24acb045561c26",
	"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
	"0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359",
	"0xdbF03B407c01E7cD3CBea99509d93f8DDDC8C6FB",
	"0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb",
}

// C1-T529 — Req: FR-301. The unit part: the checksum algorithm against the vectors; internal/grpc/api
// sends them through CreateConnection.
func TestT529_ValidChecksum(t *testing.T) {
	c := New([]uint64{31337}, nil, nil, nil)
	for _, v := range eip55Vectors {
		info, err := c.CheckAccount(context.Background(), connector.Source{}, connector.Credentials{WalletAddress: v}, nil)
		if err != nil {
			t.Errorf("%s: %v", v, err)
			continue
		}
		if !strings.EqualFold(info.Identity, v) || ToEIP55(v[2:]) != info.Identity || info.Permissions != nil {
			t.Errorf("%s: identity %s, permissions %v", v, info.Identity, info.Permissions)
		}
		// A mixed-case vector is its own EIP-55 form.
		if strings.ToLower(v) != v && strings.ToUpper(v[2:]) != v[2:] && info.Identity != v {
			t.Errorf("%s: EIP-55 form %s differs", v, info.Identity)
		}
	}
}

// C1-T534 — Req: UC-301; S3 D-35. From S3 the connector holds an RPC client: the address check of UC-301, and the
// declaration of the streams, send no request. The endpoint of the source fails the test on any request, and the
// limiter on any reservation.
func TestT534_NoNetworkCall(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "no request expected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New([]uint64{31337}, map[string]Endpoints{"anvil": {Primary: vault.NewSecret(srv.URL)}}, nil, nil)
	src := source(`{` + required + `}`)

	for _, v := range eip55Vectors {
		if _, err := c.CheckAccount(context.Background(), src, connector.Credentials{WalletAddress: v}, failingLimiter{t}); err != nil {
			t.Errorf("address check of %s: %v", v, err)
		}
	}
	for _, bad := range []string{"0x0000000000000000000000000000000000000000", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAeD", " 0x12"} {
		if _, err := c.CheckAccount(context.Background(), src, connector.Credentials{WalletAddress: bad}, failingLimiter{t}); err == nil {
			t.Errorf("address check accepted %q", bad)
		}
	}
	if streams, err := c.Streams(context.Background(), src, connector.AccountInfo{Identity: eip55Vectors[4]}); err != nil || len(streams) != 2 {
		t.Errorf("streams: %v, %v", streams, err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

// failingLimiter fails the test on any use: a request would reserve its cost first.
type failingLimiter struct{ t *testing.T }

func (l failingLimiter) Reserve(context.Context, string, int) error {
	l.t.Error("a reservation in the limiter")
	return errors.New("no reservation expected")
}

func (l failingLimiter) Pause(string, time.Duration) { l.t.Error("a pause of the limiter") }

func (l failingLimiter) Observe(string, int) { l.t.Error("an observation of the limiter") }
