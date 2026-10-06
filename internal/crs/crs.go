// Package crs reads the rate of a fiat currency to USD from the Currency Rate Service (SRS — Card Spend §2.1.2).
// The contract is vendored in third_party/proto/currency_rate (CRS v0.2.0); only rate_decimal is read, the
// double field rate never is (D-15).
package crs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/DigitLock/crypto-account-service/internal/crs/pb"
)

// Budget is the time a rate may take (SRS — Card Spend §3.2 Performance).
const Budget = 100 * time.Millisecond

// USD is the currency every rate is quoted in.
const USD = "USD"

// Errors of Rate. The decision engine maps ErrNotFound to CURRENCY_NOT_SUPPORTED (D-14) and every other error
// to RATE_UNAVAILABLE.
var (
	// ErrNotFound: CRS answers NOT_FOUND, an unknown pair or a known pair before its first poll.
	ErrNotFound = errors.New("crs: no rate for the pair")
	// ErrOutdated: CRS marks the rate is_outdated, its last poll failed.
	ErrOutdated = errors.New("crs: the rate is outdated")
	// ErrNoDecimal: the answer has no rate_decimal, a CRS older than v0.2.0 (D-15).
	ErrNoDecimal = errors.New("crs: the answer has no rate_decimal")
	// ErrUnavailable: CRS is unreachable, fails, or answers after the budget.
	ErrUnavailable = errors.New("crs: unavailable")
)

// Client calls GetRate on one CRS address, plaintext.
type Client struct {
	conn *grpc.ClientConn
	rpc  pb.CurrencyRateServiceClient
}

// Dial creates the client without connecting. The address is not part of the error.
func Dial(address string) (*Client, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, errors.New("crs: CRS_ADDRESS is not a valid gRPC target")
	}
	return &Client{conn: conn, rpc: pb.NewCurrencyRateServiceClient(conn)}, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Rate returns rate_decimal of GetRate(currency → USD) as CRS sends it: USD per one unit of currency, never
// inverted. The call is bounded by Budget within ctx.
func (c *Client) Rate(ctx context.Context, currency string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	resp, err := c.rpc.GetRate(ctx, &pb.GetRateRequest{FromCurrency: currency, ToCurrency: USD})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", ErrNotFound
		}
		// The status message of CRS is not kept: it is not ours to log.
		return "", fmt.Errorf("%w: %s", ErrUnavailable, status.Code(err))
	}
	r := resp.GetRate()
	switch {
	case r == nil:
		return "", fmt.Errorf("%w: empty answer", ErrUnavailable)
	case r.GetIsOutdated():
		return "", ErrOutdated
	case r.GetRateDecimal() == "":
		return "", ErrNoDecimal
	}
	return r.GetRateDecimal(), nil
}
