package evm

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/DigitLock/crypto-account-service/internal/connector"
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
	c := New([]uint64{31337})
	for _, v := range eip55Vectors {
		info, err := c.CheckAccount(context.Background(), connector.Source{}, connector.Credentials{WalletAddress: v})
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

// C1-T534 — Req: UC-301. The check runs without a network, and the package does not import one,
// directly or through its dependencies.
func TestT534_NoNetworkCall(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for dep := range strings.Lines(string(out)) {
		dep = strings.TrimSpace(dep)
		if dep == "net" || strings.HasPrefix(dep, "net/") || dep == "crypto/tls" ||
			strings.HasPrefix(dep, "github.com/ethereum/") || strings.HasPrefix(dep, "google.golang.org/grpc") {
			t.Errorf("internal/connector/evm depends on %s", dep)
		}
	}

	if _, err := New(nil).CheckAccount(context.Background(), connector.Source{},
		connector.Credentials{WalletAddress: eip55Vectors[4]}); err != nil {
		t.Errorf("address check: %v", err)
	}
}
