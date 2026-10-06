package decision

import (
	"errors"
	"math/big"
	"strings"
)

// Exact decimal arithmetic of the quote (UC-1 step 7, FR-7). A decimal is an integer with a scale:
// "25.40" is 2540 × 10^-2. Nothing is ever a float: the amount and the rate are parsed from their strings, the
// product is formed on integers and divided once, rounding up.

// BPSDenominator is 100 % in basis points.
const BPSDenominator = 10_000

// Limits of the stored rate, NUMERIC(20,10) (SRS — Card Spend §2.4): 10 integer and 10 fractional digits.
const (
	rateIntegerDigits  = 10
	rateFractionDigits = 10
)

// ErrMalformedRate: rate_decimal is not a plain decimal greater than 0 that fits NUMERIC(20,10).
var ErrMalformedRate = errors.New("rate_decimal is not a plain positive decimal of at most 10 + 10 digits")

// decimal is unscaled × 10^-scale.
type decimal struct {
	unscaled *big.Int
	scale    int
}

// parseDecimal parses digits with an optional point and fraction: no sign, no exponent, no spaces.
func parseDecimal(s string) (decimal, bool) {
	intPart, frac, hasPoint := strings.Cut(s, ".")
	if intPart == "" || (hasPoint && frac == "") || !allDigits(intPart) || !allDigits(frac) {
		return decimal{}, false
	}
	u, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return decimal{}, false
	}
	return decimal{unscaled: u, scale: len(frac)}, true
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// TrimDecimal returns a plain decimal without trailing fractional zeros and without a trailing point:
// "25.40" → "25.4", "1.1642000000" → "1.1642", "10.00" → "10". The format of amounts in responses (§2.1.1).
func TrimDecimal(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// ParseRate checks rate_decimal of CRS and returns it without trailing zeros, the form that is stored and
// answered. It must be a plain decimal greater than 0 that fits NUMERIC(20,10): stored as used, never rounded.
func ParseRate(s string) (string, error) {
	d, ok := parseDecimal(s)
	if !ok || d.scale > rateFractionDigits || d.unscaled.Sign() <= 0 {
		return "", ErrMalformedRate
	}
	intPart, _, _ := strings.Cut(strings.TrimLeft(s, "0"), ".")
	if len(intPart) > rateIntegerDigits {
		return "", ErrMalformedRate
	}
	return TrimDecimal(s), nil
}

// TokenAmount returns the token base units of a fiat amount, rounded up to a whole base unit.
// rate == "": the amount is USD, amount × 10^decimals, no buffer.
// Otherwise: amount × rate × (1 + bufferBps / 10 000) × 10^decimals; rate is USD per one unit of the currency,
// as CRS serves it, never inverted. amount and rate are plain decimals; the result is exact up to the one
// rounding.
func TokenAmount(amount, rate string, bufferBps int, decimals uint8) (*big.Int, error) {
	a, ok := parseDecimal(amount)
	if !ok || a.unscaled.Sign() <= 0 {
		return nil, errors.New("amount is not a plain positive decimal")
	}
	num := new(big.Int).Set(a.unscaled)
	num.Mul(num, pow10(int(decimals)))
	den := pow10(a.scale)
	if rate != "" {
		r, ok := parseDecimal(rate)
		if !ok || r.unscaled.Sign() <= 0 {
			return nil, ErrMalformedRate
		}
		if bufferBps < 0 {
			return nil, errors.New("buffer is negative")
		}
		num.Mul(num, r.unscaled)
		num.Mul(num, big.NewInt(int64(BPSDenominator+bufferBps)))
		den.Mul(den, pow10(r.scale))
		den.Mul(den, big.NewInt(BPSDenominator))
	}
	return ceilDiv(num, den), nil
}

// ceilDiv is ⌈num / den⌉ for num ≥ 0 and den > 0.
func ceilDiv(num, den *big.Int) *big.Int {
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
