package evm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// S3-T214, static part — Req: FR-313, ADR-3. The connector holds no key and sends no transaction: no send or sign
// call and no key type in its code. The import graph of server is checked by S2-T709 in cmd/server.
func TestT214_ConnectorIsReadOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"eth_sendRawTransaction", "eth_sendTransaction", "eth_sign", "personal_sign", "SendTransaction",
		"TransactOpts", "NewKeyedTransactor", "SignTx", "ecdsa.PrivateKey", "crypto.ToECDSA", "crypto.Sign",
		"internal/signer", "accounts/keystore",
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, s := range forbidden {
			if strings.Contains(string(src), s) {
				t.Errorf("%s contains %q", f, s)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("%d files checked", checked)
	}
}
