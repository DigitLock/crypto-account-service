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
// tracker, the processor API, the CRS client and the chain listener.
var cardAuthPackages = []string{
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
	for _, pkg := range append([]string{signerPackage}, cardAuthPackages...) {
		if slices.Contains(deps, pkg) {
			t.Errorf("cmd/server depends on %s", pkg)
		}
	}
}
