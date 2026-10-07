package decision

import (
	"testing"

	"github.com/google/uuid"
)

// D-16 — Req: SRS — Card Spend §2.1.5, §2.4. The expected values are computed outside Go with
// cast keccak 0x<16 bytes of the tenant UUID><UTF-8 bytes of the ID>.
func TestD16_ChainIDs(t *testing.T) {
	tenant := uuid.MustParse("0b1e7a52-3c4d-4e5f-8a9b-0c1d2e3f4a5b")
	if got := ChainAuthID(tenant, "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11").Hex(); got !=
		"0x6ede38fddffed64029965076835a340f3c2767dd811d904df1098c238a55739c" {
		t.Errorf("ChainAuthID = %s", got)
	}
	if got := ChainRefundID(tenant, "rv-20261002-0001").Hex(); got !=
		"0x0085bfc9d8641d2a78311d114e66b2323bcb4f51451ef553065af3026ea8a18c" {
		t.Errorf("ChainRefundID = %s", got)
	}

	other := uuid.MustParse("0b1e7a52-3c4d-4e5f-8a9b-0c1d2e3f4a5c")
	for _, id := range []string{"auth-1", "9f1c2a7e-5b1d-4c58-9a57-0d2f6f1e8a11", " "} {
		if ChainAuthID(tenant, id) == ChainAuthID(other, id) {
			t.Errorf("auth_id %q gives the same authId for two tenants", id)
		}
		if ChainRefundID(tenant, id) == ChainRefundID(other, id) {
			t.Errorf("return_id %q gives the same refundId for two tenants", id)
		}
	}
}
