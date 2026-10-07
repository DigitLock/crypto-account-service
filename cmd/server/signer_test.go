package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const signerPackage = "github.com/DigitLock/crypto-account-service/internal/signer"

// cardAuthPackages run only in card-auth: the decision engine, the Debit step, the operator queue, returns, the
// tracker, the processor API and its contract, the CRS client, the chain listener and the command itself.
// internal/chain is shared: the read of the final block and the RPC clients (S3 D-4); it holds no key.
var cardAuthPackages = []string{
	"github.com/DigitLock/crypto-account-service/cmd/card-auth",
	"github.com/DigitLock/crypto-account-service/api/openapi",
	"github.com/DigitLock/crypto-account-service/internal/decision",
	"github.com/DigitLock/crypto-account-service/internal/debit",
	"github.com/DigitLock/crypto-account-service/internal/operator",
	"github.com/DigitLock/crypto-account-service/internal/returns",
	"github.com/DigitLock/crypto-account-service/internal/tracker",
	"github.com/DigitLock/crypto-account-service/internal/processorapi",
	"github.com/DigitLock/crypto-account-service/internal/crs",
	"github.com/DigitLock/crypto-account-service/internal/listener",
}

// S2-T709 — Req: ADR-3. Only card-auth signs: server and every package it uses must not import the signer.
// Extended by S3-T214 (S3 D-4, S3 D-35): the EVM connector of server reaches the network through internal/chain,
// which is allowed; no card-auth package is.
func TestT709_ServerDoesNotImportSigner(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", ".")
	// GOFLAGS of the environment could change what go list reports (docs/backlog.md item 4).
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOFLAGS=")
	}), "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	deps := strings.Fields(string(out))
	if !slices.Contains(deps, "github.com/DigitLock/crypto-account-service/internal/config") {
		t.Fatalf("go list output does not hold the dependencies of server: %v", deps)
	}
	for _, pkg := range []string{
		"github.com/DigitLock/crypto-account-service/internal/chain",
		"github.com/DigitLock/crypto-account-service/internal/connector/evm",
	} {
		if !slices.Contains(deps, pkg) {
			t.Errorf("cmd/server does not depend on %s: the check is not of the server of S3", pkg)
		}
	}
	for _, pkg := range append([]string{signerPackage}, cardAuthPackages...) {
		if slices.Contains(deps, pkg) {
			t.Errorf("cmd/server depends on %s", pkg)
		}
	}
}
