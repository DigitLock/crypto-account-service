// Package chain gives card-auth access to the network: an RPC client of the primary endpoint and, when set,
// of the fallback endpoint, and the start checks of SRS — Card Spend §3.2 "Chain access".
// No error of this package contains an RPC URL: the URLs may carry an API key.
package chain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// CheckTimeout bounds each call of the start checks.
const CheckTimeout = 10 * time.Second

// Endpoint is one RPC endpoint. Name is the environment variable it comes from; errors name it, never the URL.
type Endpoint struct {
	Name   string
	Client *ethclient.Client
}

// Client holds the primary endpoint and, when configured, the fallback endpoint.
type Client struct {
	Primary  Endpoint
	Fallback *Endpoint // nil: no fallback

	urls []string
}

// Dial creates the clients without sending a request. An empty fallback URL means no fallback.
// primaryName and fallbackName are the names of the variables, used in errors.
func Dial(ctx context.Context, primaryName string, primary vault.Secret[string], fallbackName string, fallback vault.Secret[string]) (*Client, error) {
	c := &Client{urls: []string{primary.Value(), fallback.Value()}}
	p, err := ethclient.DialContext(ctx, primary.Value())
	if err != nil {
		// The error of the RPC package is not wrapped: it may quote the URL.
		return nil, fmt.Errorf("chain: %s: cannot create the RPC client", primaryName)
	}
	c.Primary = Endpoint{Name: primaryName, Client: p}
	if fallback.Value() != "" {
		f, err := ethclient.DialContext(ctx, fallback.Value())
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("chain: %s: cannot create the RPC client", fallbackName)
		}
		c.Fallback = &Endpoint{Name: fallbackName, Client: f}
	}
	return c, nil
}

// Close closes every client.
func (c *Client) Close() {
	c.Primary.Client.Close()
	if c.Fallback != nil {
		c.Fallback.Client.Close()
	}
}

// Endpoints returns the primary and, when set, the fallback endpoint.
func (c *Client) Endpoints() []Endpoint {
	if c.Fallback == nil {
		return []Endpoint{c.Primary}
	}
	return []Endpoint{c.Primary, *c.Fallback}
}

// Expected holds the configured values the start checks compare the chain with.
type Expected struct {
	ChainID    uint64
	Allowed    []uint64
	Controller common.Address
	Token      common.Address
	Decimals   uint8
}

// StartCheckError is a failed start check. Check names the check; the message contains no URL.
type StartCheckError struct {
	Check  string
	Detail string
}

func (e *StartCheckError) Error() string {
	return "start check " + e.Check + " failed: " + e.Detail
}

// StartChecks runs the start checks of SRS — Card Spend §3.2: eth_chainId of every endpoint equals the
// configured chain ID and is in the allow-list; token() of the controller equals the configured token;
// decimals() of the token equals the configured decimals; symbol() of the token is read. It returns the symbol,
// which card-auth keeps in memory and stores with every quote (D-10), or the first failure.
func (c *Client) StartChecks(ctx context.Context, want Expected) (string, error) {
	for _, ep := range c.Endpoints() {
		check := "eth_chainId of " + ep.Name
		cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
		id, err := ep.Client.ChainID(cctx)
		cancel()
		if err != nil {
			return "", &StartCheckError{Check: check, Detail: c.describe(err)}
		}
		if !id.IsUint64() || id.Uint64() != want.ChainID {
			return "", &StartCheckError{Check: check, Detail: fmt.Sprintf("the endpoint serves chain ID %s, CARD_AUTH_CHAIN_ID is %d", id, want.ChainID)}
		}
		if !slices.Contains(want.Allowed, id.Uint64()) {
			return "", &StartCheckError{Check: check, Detail: fmt.Sprintf("chain ID %d is not in EVM_ALLOWED_CHAIN_IDS", id.Uint64())}
		}
	}

	backend := c.Primary.Client
	controller, err := bindings.NewCardSpendControllerCaller(want.Controller, backend)
	if err != nil {
		return "", &StartCheckError{Check: "token() of the controller", Detail: "cannot bind the controller ABI"}
	}
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	token, err := controller.Token(&bind.CallOpts{Context: cctx})
	cancel()
	if err != nil {
		return "", &StartCheckError{Check: "token() of the controller", Detail: c.describe(err)}
	}
	if token != want.Token {
		return "", &StartCheckError{Check: "token() of the controller", Detail: fmt.Sprintf(
			"the controller %s returns the token %s, CARD_AUTH_TOKEN_ADDRESS is %s", want.Controller.Hex(), token.Hex(), want.Token.Hex())}
	}

	tokenCaller, err := bindings.NewMockUSDCCaller(want.Token, backend)
	if err != nil {
		return "", &StartCheckError{Check: "decimals() of the token", Detail: "cannot bind the token ABI"}
	}
	cctx, cancel = context.WithTimeout(ctx, CheckTimeout)
	decimals, err := tokenCaller.Decimals(&bind.CallOpts{Context: cctx})
	cancel()
	if err != nil {
		return "", &StartCheckError{Check: "decimals() of the token", Detail: c.describe(err)}
	}
	if decimals != want.Decimals {
		return "", &StartCheckError{Check: "decimals() of the token", Detail: fmt.Sprintf(
			"the token returns %d, CARD_AUTH_TOKEN_DECIMALS is %d", decimals, want.Decimals)}
	}

	cctx, cancel = context.WithTimeout(ctx, CheckTimeout)
	symbol, err := tokenCaller.Symbol(&bind.CallOpts{Context: cctx})
	cancel()
	if err != nil {
		return "", &StartCheckError{Check: "symbol() of the token", Detail: c.describe(err)}
	}
	return symbol, nil
}

// describe turns an error of an RPC call into text without a URL. Only the JSON-RPC error of the node and the
// HTTP status are kept; transport errors quote the URL and are reduced to a fixed text.
func (c *Client) describe(err error) string {
	return c.describeWithin(err, CheckTimeout)
}

// describeWithin is describe for a call bounded by timeout.
func (c *Client) describeWithin(err error, timeout time.Duration) string {
	var (
		rpcErr  rpc.Error
		httpErr rpc.HTTPError
	)
	var s string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s = fmt.Sprintf("no answer within %s", timeout)
	case errors.Is(err, context.Canceled):
		s = "cancelled"
	case errors.Is(err, bind.ErrNoCode):
		s = "no contract code at the address"
	case errors.As(err, &httpErr):
		s = fmt.Sprintf("the endpoint answered HTTP %d", httpErr.StatusCode)
	case errors.As(err, &rpcErr):
		s = fmt.Sprintf("JSON-RPC error %d: %s", rpcErr.ErrorCode(), rpcErr.Error())
	default:
		s = "the endpoint is unreachable or its answer is malformed"
	}
	for _, u := range c.urls {
		if u != "" {
			s = strings.ReplaceAll(s, u, "[redacted]")
		}
	}
	return s
}

// OperatorNonce returns the transaction count of the operator at the pending block tag, from the primary
// endpoint. A failure is a StartCheckError without a URL.
func (c *Client) OperatorNonce(ctx context.Context, operator common.Address) (uint64, error) {
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	n, err := c.Primary.Client.PendingNonceAt(cctx, operator)
	if err != nil {
		return 0, &StartCheckError{Check: "transaction count of the operator", Detail: c.describe(err)}
	}
	return n, nil
}
