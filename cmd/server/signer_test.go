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
// which is allowed; no card-auth package is. Extended in S3 st7a: internal/reconcile, which casctl and the worker of
// server run, imports no card-auth package and nothing that reaches the network (UC-4 reads database tables only).
func TestT709_ServerDoesNotImportSigner(t *testing.T) {
	deps := goListDeps(t, "-test", ".")
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

	t.Run("internal/reconcile", func(t *testing.T) {
		// Without -test: the tests of internal/reconcile drive Anvil through internal/testchain.
		deps := goListDeps(t, "../../internal/reconcile")
		if !slices.Contains(deps, "github.com/DigitLock/crypto-account-service/internal/repository") {
			t.Fatalf("go list output does not hold the dependencies of internal/reconcile: %v", deps)
		}
		noNetwork := []string{
			"github.com/DigitLock/crypto-account-service/internal/chain",
			"github.com/DigitLock/crypto-account-service/internal/connector/evm",
			"github.com/ethereum/go-ethereum/ethclient",
			"github.com/ethereum/go-ethereum/rpc",
		}
		for _, pkg := range append(append([]string{signerPackage}, cardAuthPackages...), noNetwork...) {
			if slices.Contains(deps, pkg) {
				t.Errorf("internal/reconcile depends on %s", pkg)
			}
		}
	})
}

// goListDeps returns the output of go list -deps with args: the import paths of the dependencies.
func goListDeps(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-deps"}, args...)...)
	// GOFLAGS of the environment could change what go list reports (docs/backlog.md item 4).
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "GOFLAGS=")
	}), "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}
