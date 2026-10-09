package binance

import (
	"math/big"
	"strconv"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Limits of Binance, constants of the connector (SRS — Binance §2.1.1 Rate limits, values as of 2026-10-09; X1 D-6).
// They are re-checked against the Binance documentation before each milestone; Budgets makes no call.
const (
	// LimitAPI is the weight of all /api endpoints per minute and IP.
	LimitAPI = 6000
	// LimitSAPIIP is the weight of one IP-limited /sapi endpoint per minute and IP.
	LimitSAPIIP = 12000
	// LimitWindow is the window of both limits.
	LimitWindow = time.Minute
)

// Budget names (X1 D-25): BudgetAPI for every /api endpoint; SAPIBudget(path) for each /sapi endpoint.
const (
	BudgetAPI        = "api"
	budgetSAPIPrefix = "sapi:"
)

// Used-weight headers of an answer (SRS — Binance §2.1.1 Rate limits). X1 has no UID-limited endpoint, so
// X-SAPI-USED-UID-WEIGHT-1M is not read.
const (
	headerUsedWeightAPI    = "X-MBX-USED-WEIGHT-1M"
	headerUsedWeightSAPIIP = "X-SAPI-USED-IP-WEIGHT-1M"
)

// SAPIBudget is the budget name of a /sapi endpoint: "sapi:" and its path, such as sapi:/sapi/v1/account/apiRestrictions.
func SAPIBudget(path string) string { return budgetSAPIPrefix + path }

// Budgets declares the budgets of the binance source (X1 D-6): BudgetAPI of floor(LimitAPI × budget_share) per
// minute, and one budget per /sapi endpoint of X1 of floor(LimitSAPIIP × budget_share) per minute. No budget per
// account: the /sapi endpoints of X1 are IP-limited. No request.
func (c *Connector) Budgets(src connector.Source) []connector.Budget {
	share := ParseConfig(src).BudgetShare
	budgets := []connector.Budget{{Name: BudgetAPI, Units: shareOf(LimitAPI, share), Window: LimitWindow}}
	for _, ep := range endpoints {
		if ep.budget != BudgetAPI {
			budgets = append(budgets, connector.Budget{Name: ep.budget, Units: shareOf(LimitSAPIIP, share), Window: LimitWindow})
		}
	}
	return budgets
}

// shareOf is floor(limit × share), at least 1. The share is taken as the decimal it is written as, so 6000 × 0.29
// gives 1740, not the 1739 of the binary float product.
func shareOf(limit int, share float64) int {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(share, 'f', -1, 64))
	if !ok {
		return 1
	}
	r.Mul(r, new(big.Rat).SetInt64(int64(limit)))
	n := new(big.Int).Quo(r.Num(), r.Denom()) // both positive: the quotient is the floor
	return max(1, int(n.Int64()))
}
