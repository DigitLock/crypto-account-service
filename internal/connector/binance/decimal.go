package binance

import (
	"fmt"
	"strings"
)

// decimal normalises a decimal string of Binance (SRS — Binance §2.1.1 Amounts): digits with an optional fraction,
// optionally negative. The result has no leading zeros of the integer part, no trailing zeros of the fraction and
// no exponent: 0.00100000 → 0.001, 0.00000000 → 0, 12.50000000 → 12.5. The value is never parsed as a float. A
// string of another form, such as 1e-3, .5, +1 or an empty one, is an error that does not quote the value. Range
// and sign rules of a balance are the snapshot's (SRS — Core Connector contract).
func decimal(s string) (string, error) {
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	intPart, frac, hasFrac := strings.Cut(digits, ".")
	if intPart == "" || (hasFrac && frac == "") || !allDigits(intPart) || !allDigits(frac) {
		return "", fmt.Errorf("binance: an amount is not a plain decimal (%d characters)", len(s))
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	out := intPart
	if frac != "" {
		out += "." + frac
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out, nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
