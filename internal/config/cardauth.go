package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// OperatorKeySize is the length of OPERATOR_PRIVATE_KEY after hexadecimal decoding.
const OperatorKeySize = 32

// Finality modes of CARD_AUTH_FINALITY_MODE and tags of CARD_AUTH_FINALITY_TAG (SRS — Card Spend §3.1).
const (
	FinalityModeConfirmations = "confirmations"
	FinalityModeTag           = "tag"
	FinalityTagFinalized      = "finalized"
	FinalityTagSafe           = "safe"
)

// Subscription types of CARD_AUTH_LISTENER_SUBSCRIPTION.
const (
	SubscriptionPendingLogs = "pendingLogs"
	SubscriptionLogs        = "logs"
)

// MinGasLimit is the gas of a plain transfer; a gas limit of a contract call must exceed it.
const MinGasLimit = 21000

// Defaults of CARD_AUTH_DEBIT_GAS_LIMIT and CARD_AUTH_REFUND_GAS_LIMIT: the gas measured on Anvil, maximum of cold
// and warm storage, + 25 %, rounded up to a thousand (SRS — Card Spend §3.1; internal/debit gas test).
const (
	DefaultDebitGasLimit  uint64 = 176000
	DefaultRefundGasLimit uint64 = 145000
)

// CardAuth is the parsed environment of card-auth (SRS — Card Spend §3.1).
// The operator key, CARD_AUTH_DATABASE_URL and every RPC URL are held as vault.Secret values:
// no method prints them, and LogValue omits them.
type CardAuth struct {
	DatabaseURL vault.Secret[string]
	OperatorKey vault.Secret[[]byte]

	ChainID            uint64
	RPCURL             vault.Secret[string]
	RPCFallbackURL     vault.Secret[string] // empty: no fallback
	RPCWSURL           vault.Secret[string] // empty: no listener, polling only
	ControllerAddress  common.Address
	TokenAddress       common.Address
	TokenDecimals      uint8
	EVMAllowedChainIDs []uint64

	DecisionDeadline      time.Duration
	DebitValidity         time.Duration
	MinSendWindow         time.Duration // min_send_window: no debit is sent with less time left (D-20)
	RPCReadTimeout        time.Duration
	ReceiptPollInterval   time.Duration
	QuoteBufferBPS        int
	FinalityMode          string
	FinalityTag           string
	FinalityConfirmations int
	TrackerInterval       time.Duration
	ReturnRetryInterval   time.Duration
	RPCFallbackAfter      int
	FeeBumpPercent        int
	ListenerSubscription  string
	DebitGasLimit         uint64
	RefundGasLimit        uint64

	CRSAddress string

	HTTPPort   int
	HealthPort int

	DBPoolMaxConns int32
	DBPoolMinConns int32

	ShutdownTimeout time.Duration

	LogLevel  slog.Level
	LogFormat string
}

// LoadCardAuth reads and validates the environment of card-auth through getenv. An empty value counts as unset.
// The returned error names every invalid variable and never contains a value.
func LoadCardAuth(getenv func(string) string) (CardAuth, error) {
	p := parser{getenv: getenv}
	cfg := CardAuth{
		DatabaseURL: vault.NewSecret(p.required("CARD_AUTH_DATABASE_URL")),
		OperatorKey: p.operatorKey("OPERATOR_PRIVATE_KEY"),

		ChainID:            p.chainID("CARD_AUTH_CHAIN_ID"),
		RPCURL:             p.url("CARD_AUTH_RPC_URL", true, "http", "https"),
		RPCFallbackURL:     p.url("CARD_AUTH_RPC_FALLBACK_URL", false, "http", "https"),
		RPCWSURL:           p.url("CARD_AUTH_RPC_WS_URL", false, "ws", "wss"),
		ControllerAddress:  p.address("CARD_AUTH_CONTROLLER_ADDRESS"),
		TokenAddress:       p.address("CARD_AUTH_TOKEN_ADDRESS"),
		TokenDecimals:      uint8(p.requiredInteger("CARD_AUTH_TOKEN_DECIMALS", 0, 18)),
		EVMAllowedChainIDs: p.chainIDs("EVM_ALLOWED_CHAIN_IDS", []uint64{31337, 84532}),

		DecisionDeadline:      p.duration("CARD_AUTH_DECISION_DEADLINE", 2500*time.Millisecond),
		DebitValidity:         p.duration("CARD_AUTH_DEBIT_VALIDITY", 4*time.Second),
		MinSendWindow:         p.duration("CARD_AUTH_MIN_SEND_WINDOW", 500*time.Millisecond),
		RPCReadTimeout:        p.duration("CARD_AUTH_RPC_READ_TIMEOUT", 500*time.Millisecond),
		ReceiptPollInterval:   p.duration("CARD_AUTH_RECEIPT_POLL_INTERVAL", 200*time.Millisecond),
		QuoteBufferBPS:        p.integer("CARD_AUTH_QUOTE_BUFFER_BPS", 100, 0, 10000),
		FinalityMode:          p.oneOf("CARD_AUTH_FINALITY_MODE", FinalityModeConfirmations, FinalityModeConfirmations, FinalityModeTag),
		FinalityTag:           p.oneOf("CARD_AUTH_FINALITY_TAG", FinalityTagFinalized, FinalityTagFinalized, FinalityTagSafe),
		FinalityConfirmations: p.integer("CARD_AUTH_FINALITY_CONFIRMATIONS", 10, 1, 1<<31-1),
		TrackerInterval:       p.duration("CARD_AUTH_TRACKER_INTERVAL", 2*time.Second),
		ReturnRetryInterval:   p.duration("CARD_AUTH_RETURN_RETRY_INTERVAL", 30*time.Second),
		RPCFallbackAfter:      p.integer("CARD_AUTH_RPC_FALLBACK_AFTER", 3, 1, 1<<31-1),
		FeeBumpPercent:        p.integer("CARD_AUTH_FEE_BUMP_PERCENT", 25, 10, 1<<31-1),
		ListenerSubscription:  p.oneOf("CARD_AUTH_LISTENER_SUBSCRIPTION", SubscriptionPendingLogs, SubscriptionPendingLogs, SubscriptionLogs),
		DebitGasLimit:         uint64(p.integer("CARD_AUTH_DEBIT_GAS_LIMIT", int(DefaultDebitGasLimit), MinGasLimit+1, math.MaxInt64)),
		RefundGasLimit:        uint64(p.integer("CARD_AUTH_REFUND_GAS_LIMIT", int(DefaultRefundGasLimit), MinGasLimit+1, math.MaxInt64)),

		CRSAddress: p.getenv("CRS_ADDRESS"),

		HTTPPort:   p.integer("CARD_AUTH_HTTP_PORT", 8092, 1, 65535),
		HealthPort: p.integer("CARD_AUTH_HEALTH_PORT", 8093, 1, 65535),

		DBPoolMaxConns: int32(p.integer("CARD_AUTH_DB_POOL_MAX_CONNS", 10, 1, 1<<31-1)),
		DBPoolMinConns: int32(p.integer("CARD_AUTH_DB_POOL_MIN_CONNS", 2, 0, 1<<31-1)),

		ShutdownTimeout: p.duration("CARD_AUTH_SHUTDOWN_TIMEOUT", 15*time.Second),

		LogLevel:  p.logLevel("LOG_LEVEL", slog.LevelInfo),
		LogFormat: p.oneOf("LOG_FORMAT", LogFormatJSON, LogFormatJSON, LogFormatText),
	}

	if cfg.DebitValidity < cfg.DecisionDeadline+time.Second {
		p.fail("CARD_AUTH_DEBIT_VALIDITY", "must be at least CARD_AUTH_DECISION_DEADLINE + 1s")
	}
	if cfg.MinSendWindow >= cfg.DecisionDeadline {
		p.fail("CARD_AUTH_MIN_SEND_WINDOW", "must be below CARD_AUTH_DECISION_DEADLINE")
	}
	if cfg.ChainID != 0 && !slices.Contains(cfg.EVMAllowedChainIDs, cfg.ChainID) {
		p.fail("CARD_AUTH_CHAIN_ID", "must be in EVM_ALLOWED_CHAIN_IDS")
	}
	if cfg.DBPoolMinConns > cfg.DBPoolMaxConns {
		p.fail("CARD_AUTH_DB_POOL_MIN_CONNS", "must not exceed CARD_AUTH_DB_POOL_MAX_CONNS")
	}

	if err := errors.Join(p.errs...); err != nil {
		return CardAuth{}, err
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer. The operator key, CARD_AUTH_DATABASE_URL and the RPC URLs are omitted.
func (c CardAuth) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Uint64("CARD_AUTH_CHAIN_ID", c.ChainID),
		slog.String("CARD_AUTH_CONTROLLER_ADDRESS", c.ControllerAddress.Hex()),
		slog.String("CARD_AUTH_TOKEN_ADDRESS", c.TokenAddress.Hex()),
		slog.Int("CARD_AUTH_TOKEN_DECIMALS", int(c.TokenDecimals)),
		slog.String("EVM_ALLOWED_CHAIN_IDS", joinUint(c.EVMAllowedChainIDs)),
		duration("CARD_AUTH_DECISION_DEADLINE", c.DecisionDeadline),
		duration("CARD_AUTH_DEBIT_VALIDITY", c.DebitValidity),
		duration("CARD_AUTH_MIN_SEND_WINDOW", c.MinSendWindow),
		duration("CARD_AUTH_RPC_READ_TIMEOUT", c.RPCReadTimeout),
		duration("CARD_AUTH_RECEIPT_POLL_INTERVAL", c.ReceiptPollInterval),
		slog.Int("CARD_AUTH_QUOTE_BUFFER_BPS", c.QuoteBufferBPS),
		slog.String("CARD_AUTH_FINALITY_MODE", c.FinalityMode),
		slog.String("CARD_AUTH_FINALITY_TAG", c.FinalityTag),
		slog.Int("CARD_AUTH_FINALITY_CONFIRMATIONS", c.FinalityConfirmations),
		duration("CARD_AUTH_TRACKER_INTERVAL", c.TrackerInterval),
		duration("CARD_AUTH_RETURN_RETRY_INTERVAL", c.ReturnRetryInterval),
		slog.Int("CARD_AUTH_RPC_FALLBACK_AFTER", c.RPCFallbackAfter),
		slog.Int("CARD_AUTH_FEE_BUMP_PERCENT", c.FeeBumpPercent),
		slog.String("CARD_AUTH_LISTENER_SUBSCRIPTION", c.ListenerSubscription),
		slog.Uint64("CARD_AUTH_DEBIT_GAS_LIMIT", c.DebitGasLimit),
		slog.Uint64("CARD_AUTH_REFUND_GAS_LIMIT", c.RefundGasLimit),
		slog.String("CRS_ADDRESS", c.CRSAddress),
		slog.Int("CARD_AUTH_HTTP_PORT", c.HTTPPort),
		slog.Int("CARD_AUTH_HEALTH_PORT", c.HealthPort),
		slog.Int("CARD_AUTH_DB_POOL_MAX_CONNS", int(c.DBPoolMaxConns)),
		slog.Int("CARD_AUTH_DB_POOL_MIN_CONNS", int(c.DBPoolMinConns)),
		duration("CARD_AUTH_SHUTDOWN_TIMEOUT", c.ShutdownTimeout),
		slog.String("LOG_LEVEL", strings.ToLower(c.LogLevel.String())),
		slog.String("LOG_FORMAT", c.LogFormat),
	)
}

// requiredInteger is integer for a variable without default.
func (p *parser) requiredInteger(name string, minimum, maximum int) int {
	if p.required(name) == "" {
		return 0
	}
	return p.integer(name, 0, minimum, maximum)
}

func (p *parser) chainID(name string) uint64 {
	v := p.required(name)
	if v == "" {
		return 0
	}
	// The strconv error is not wrapped: it quotes the value.
	id, err := strconv.ParseUint(v, 10, 64)
	if err != nil || id == 0 {
		p.fail(name, "must be a positive integer chain ID")
		return 0
	}
	return id
}

// operatorKey reads 32 bytes as 64 hexadecimal characters with an optional 0x prefix.
// Whether the bytes are a valid secp256k1 key is checked by the signer.
func (p *parser) operatorKey(name string) vault.Secret[[]byte] {
	v := p.required(name)
	if v == "" {
		return vault.Secret[[]byte]{}
	}
	v = strings.TrimPrefix(v, "0x")
	// The decoding error is not wrapped: it would point into the value.
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != OperatorKeySize {
		p.fail(name, fmt.Sprintf("must be %d bytes as %d hexadecimal characters, 0x optional", OperatorKeySize, 2*OperatorKeySize))
		return vault.Secret[[]byte]{}
	}
	return vault.NewSecret(key)
}

// url reads an absolute URL with one of the schemes and a host. Neither the value nor a parse error is printed.
func (p *parser) url(name string, required bool, schemes ...string) vault.Secret[string] {
	var v string
	if required {
		v = p.required(name)
	} else {
		v = p.getenv(name)
	}
	if v == "" {
		return vault.Secret[string]{}
	}
	u, err := url.Parse(v)
	if err != nil || !slices.Contains(schemes, u.Scheme) || u.Host == "" {
		p.fail(name, "must be a URL with the scheme "+strings.Join(schemes, " or ")+" and a host")
		return vault.Secret[string]{}
	}
	return vault.NewSecret(v)
}

// address reads a contract address: 0x and 40 hexadecimal characters, EIP-55 when in mixed case, not zero.
func (p *parser) address(name string) common.Address {
	v := p.required(name)
	if v == "" {
		return common.Address{}
	}
	// The error of CheckAddress names wallet.address: only its outcome is used.
	checked, err := evm.CheckAddress(v)
	if err != nil {
		p.fail(name, "must be 0x and 40 hexadecimal characters, EIP-55 when in mixed case, and not the zero address")
		return common.Address{}
	}
	return common.HexToAddress(checked)
}
