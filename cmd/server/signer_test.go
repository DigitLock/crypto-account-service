package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const signerPackage = "github.com/DigitLock/crypto-account-service/internal/signer"

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
	if slices.Contains(deps, signerPackage) {
		t.Errorf("cmd/server depends on %s", signerPackage)
	}
}
