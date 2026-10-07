package evm

import (
	"strings"

	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Prefixes of the endpoint variables of an EVM source; the suffix is the source code in upper case with _ for -
// (SRS — EVM Connector §3.1). internal/config reads them.
const (
	RPCURLPrefix         = "EVM_RPC_URL_"
	RPCFallbackURLPrefix = "EVM_RPC_FALLBACK_URL_"
)

// Endpoints are the RPC endpoints of one EVM source. A provider URL usually carries an API key in its path or query,
// so both are secrets (SRS — EVM Connector §3.2 Security).
type Endpoints struct {
	Primary  vault.Secret[string] // empty: the source has no endpoint (S3 D-16), even with a fallback
	Fallback vault.Secret[string] // empty: no fallback
}

// SourceSuffix returns the suffix of the endpoint variables of a source: base-sepolia → BASE_SEPOLIA.
func SourceSuffix(code string) string {
	return strings.ReplaceAll(strings.ToUpper(code), "-", "_")
}

// PrimaryVariable and FallbackVariable name the endpoint variables of a source: errors and logs name them, never
// the URL.
func PrimaryVariable(code string) string  { return RPCURLPrefix + SourceSuffix(code) }
func FallbackVariable(code string) string { return RPCFallbackURLPrefix + SourceSuffix(code) }
