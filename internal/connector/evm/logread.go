package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Event types and the ledger values of UC-303 step 8.
const (
	eventTransfer = "Transfer"
	eventDebited  = "Debited"
	eventRefunded = "Refunded"

	entryDeposit    = "DEPOSIT"
	entryWithdrawal = "WITHDRAWAL"
	entryCardDebit  = "CARD_DEBIT"
	entryCardRefund = "CARD_REFUND"
	legSingle       = "SINGLE"
	directionIn     = "IN"
	directionOut    = "OUT"
)

// Topic 0 of the events, from the frozen ABI of internal/chain/bindings.
var (
	topicTransfer = tokenABI.Events[eventTransfer].ID
	topicDebited  = controllerABI.Events[eventDebited].ID
	topicRefunded = controllerABI.Events[eventRefunded].ID
)

// chainLog is a log as eth_getLogs returns it; raw keeps the object as received for ledger_entries.raw.
type chainLog struct {
	Address     common.Address `json:"address"`
	Topics      []common.Hash  `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	BlockHash   common.Hash    `json:"blockHash"`
	TxHash      common.Hash    `json:"transactionHash"`
	Index       hexutil.Uint64 `json:"logIndex"`
	Removed     bool           `json:"removed"`
	raw         json.RawMessage
}

// externalID is tx_hash:log_index: 0x and 64 lower-case hexadecimal digits, the index in decimal.
func (l chainLog) externalID() string {
	return l.TxHash.Hex() + ":" + strconv.FormatUint(uint64(l.Index), 10)
}

// logFilter is one log filter of §2.1.2: the contracts and the topics by position; a nil position matches any.
type logFilter struct {
	addresses []common.Address
	topics    []any
}

// filters are the log filters of the connection (§2.1.2): transfers out and in of every tracked token, then the
// controller events of the wallet as the user, or every controller event for the treasury connection.
func (s *session) filters(account common.Address, treasuryConn bool) []logFilter {
	tokens := make([]common.Address, 0, len(s.src.Aliases))
	for _, a := range s.src.Aliases {
		tokens = append(tokens, common.HexToAddress(a.NativeAsset))
	}
	controller := []common.Address{common.HexToAddress(s.cfg.ControllerAddress)}
	who := common.BytesToHash(account.Bytes())
	out := []logFilter{
		{tokens, []any{topicTransfer, who}},
		{tokens, []any{topicTransfer, nil, who}},
	}
	if treasuryConn {
		return append(out, logFilter{controller, []any{[]common.Hash{topicDebited, topicRefunded}}})
	}
	return append(out,
		logFilter{controller, []any{topicDebited, nil, who}},
		logFilter{controller, []any{topicRefunded, nil, nil, who}},
	)
}

// readLogs runs the filters for blocks from to to (UC-303 step 6). Every log must be live, inside the range and
// from a contract of its filter. A log found by two filters, a self-transfer, is kept once. Order: block, index.
func (s *session) readLogs(ctx context.Context, filters []logFilter, from, to uint64) ([]chainLog, error) {
	seen := map[string]bool{}
	var out []chainLog
	for _, f := range filters {
		var raws []json.RawMessage
		query := map[string]any{"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to),
			"address": f.addresses, "topics": f.topics}
		if err := s.call(ctx, &raws, "eth_getLogs", query); err != nil {
			return nil, err
		}
		for _, r := range raws {
			var l chainLog
			if err := json.Unmarshal(r, &l); err != nil {
				return nil, fmt.Errorf("evm: %s: eth_getLogs: a malformed log", s.variable())
			}
			switch {
			case l.Removed:
				return nil, fmt.Errorf("evm: %s: eth_getLogs: log %s is removed", s.variable(), l.externalID())
			case uint64(l.BlockNumber) < from || uint64(l.BlockNumber) > to:
				return nil, fmt.Errorf("evm: %s: eth_getLogs: log %s of block %d is outside blocks %d to %d",
					s.variable(), l.externalID(), uint64(l.BlockNumber), from, to)
			case !slices.Contains(f.addresses, l.Address):
				return nil, fmt.Errorf("evm: %s: eth_getLogs: log %s of contract %s does not match its filter",
					s.variable(), l.externalID(), l.Address.Hex())
			}
			if seen[l.externalID()] {
				continue
			}
			seen[l.externalID()] = true
			l.raw = r
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b chainLog) int {
		if a.BlockNumber != b.BlockNumber {
			return cmpUint(uint64(a.BlockNumber), uint64(b.BlockNumber))
		}
		return cmpUint(uint64(a.Index), uint64(b.Index))
	})
	return out, nil
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// rangeSize is the range size of the next read: the size after splitting, never above log_range_max.
func (s *session) rangeSize() uint64 {
	s.net.mu.Lock()
	defer s.net.mu.Unlock()
	if s.net.rangeSize == 0 || s.net.rangeSize > s.cfg.LogRangeMax {
		return s.cfg.LogRangeMax
	}
	return s.net.rangeSize
}

func (s *session) setRangeSize(size uint64) {
	s.net.mu.Lock()
	s.net.rangeSize = size
	s.net.mu.Unlock()
	s.c.metrics.LogRangeBlocks(s.src.Code, size)
}

// readRange runs step 6 from next, halving the range while the provider rejects it as too large (EC-306), down to
// one block. The smaller size is kept for the source until the process stops.
func (s *session) readRange(ctx context.Context, filters []logFilter, next, final uint64) (from, to uint64, logs []chainLog, err error) {
	size := s.rangeSize()
	s.c.metrics.LogRangeBlocks(s.src.Code, size)
	for {
		from, to = logRange(next, final, size)
		logs, err = s.readLogs(ctx, filters, from, to)
		var tooLarge *tooLargeError
		if !errors.As(err, &tooLarge) {
			return from, to, logs, err
		}
		if from == to {
			return 0, 0, nil, fmt.Errorf("evm: the provider rejects the logs of the single block %d as too large (EC-306): %w", from, err)
		}
		size = max(1, (to-from+1)/2)
		s.setRangeSize(size)
		s.c.logger.WarnContext(ctx, "eth_getLogs rejected as too large: the range is halved", "source", s.src.Code,
			"from", from, "to", to, "range_blocks", size)
	}
}

// blockTimes reads the time of every block that has logs, once per block hash (UC-303 step 7).
func (s *session) blockTimes(ctx context.Context, logs []chainLog) (map[common.Hash]time.Time, error) {
	times := map[common.Hash]time.Time{}
	for _, l := range logs {
		if _, ok := times[l.BlockHash]; ok {
			continue
		}
		var h *header
		if err := s.call(ctx, &h, "eth_getBlockByHash", l.BlockHash, false); err != nil {
			return nil, err
		}
		if h == nil || h.Number != l.BlockNumber {
			return nil, fmt.Errorf("evm: %s: the endpoint has no block %s of log %s", s.variable(), l.BlockHash.Hex(), l.externalID())
		}
		times[l.BlockHash] = time.Unix(int64(h.Time), 0).UTC()
	}
	return times, nil
}

// treasuryAddress is treasury() of the controller: read by the treasury check, else read once for the pairing of
// a wallet connection when the source has no treasury connection.
func (s *session) treasuryAddress(ctx context.Context) (common.Address, error) {
	s.net.mu.Lock()
	treasury, known := s.net.treasury, s.net.treasuryKnown
	s.net.mu.Unlock()
	if known {
		return treasury, nil
	}
	treasury, err := s.controllerAddress(ctx, "treasury")
	if err != nil {
		return common.Address{}, err
	}
	s.net.mu.Lock()
	s.net.treasury, s.net.treasuryKnown = treasury, true
	s.net.mu.Unlock()
	return treasury, nil
}

// event is a decoded log of a tracked token or of the controller.
type event struct {
	log      chainLog
	name     string
	from, to common.Address // Transfer
	user     common.Address // Debited, Refunded
	authID   common.Hash    // Debited, Refunded
	refundID common.Hash    // Refunded
	amount   *big.Int
	paired   bool
}

func decode(l chainLog) (*event, error) {
	bad := fmt.Errorf("evm: log %s of %s is malformed", l.externalID(), l.Address.Hex())
	if len(l.Topics) == 0 || len(l.Data) != 32 {
		return nil, bad
	}
	e := &event{log: l, amount: new(big.Int).SetBytes(l.Data)}
	switch l.Topics[0] {
	case topicTransfer:
		if len(l.Topics) != 3 {
			return nil, bad
		}
		e.name, e.from, e.to = eventTransfer, common.BytesToAddress(l.Topics[1][:]), common.BytesToAddress(l.Topics[2][:])
	case topicDebited:
		if len(l.Topics) != 3 {
			return nil, bad
		}
		e.name, e.authID, e.user = eventDebited, l.Topics[1], common.BytesToAddress(l.Topics[2][:])
	case topicRefunded:
		if len(l.Topics) != 4 {
			return nil, bad
		}
		e.name, e.authID, e.refundID, e.user = eventRefunded, l.Topics[1], l.Topics[2], common.BytesToAddress(l.Topics[3][:])
	default:
		return nil, bad
	}
	return e, nil
}

// args are the decoded fields of §2.4 Stored log: addresses in EIP-55, bytes32 as 0x hex, amounts in base units.
func (e *event) args() map[string]string {
	switch e.name {
	case eventTransfer:
		return map[string]string{"from": e.from.Hex(), "to": e.to.Hex(), "value": e.amount.String()}
	case eventDebited:
		return map[string]string{"authId": e.authID.Hex(), "user": e.user.Hex(), "amount": e.amount.String()}
	default:
		return map[string]string{"authId": e.authID.Hex(), "refundId": e.refundID.Hex(), "user": e.user.Hex(),
			"amount": e.amount.String()}
	}
}

// pair pairs every controller event with one Transfer of its transaction: the token of the controller, the parties
// (Debited: user → treasury; Refunded: treasury → user) and the amount; the nearest by log index not paired yet.
func pair(events []*event, token, treasury common.Address) {
	for _, c := range events {
		if c.name == eventTransfer {
			continue
		}
		from, to := c.user, treasury
		if c.name == eventRefunded {
			from, to = treasury, c.user
		}
		var best *event
		var bestDist uint64
		for _, t := range events {
			if t.name != eventTransfer || t.paired || t.log.TxHash != c.log.TxHash || t.log.Address != token ||
				t.from != from || t.to != to || t.amount.Cmp(c.amount) != 0 {
				continue
			}
			dist := max(t.log.Index, c.log.Index) - min(t.log.Index, c.log.Index)
			if best == nil || uint64(dist) < bestDist {
				best, bestDist = t, uint64(dist)
			}
		}
		if best != nil {
			best.paired = true
		}
	}
}

// mapLogs is UC-303 step 8: the entries of the connection, in the order of the logs.
func (s *session) mapLogs(logs []chainLog, times map[common.Hash]time.Time, account, token, treasury common.Address,
	treasuryConn bool) ([]connector.Entry, error) {
	events := make([]*event, 0, len(logs))
	for _, l := range logs {
		e, err := decode(l)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	pair(events, token, treasury)

	var entries []connector.Entry
	for _, e := range events {
		var kind, direction string
		asset := e.log.Address
		switch {
		case e.name == eventDebited && treasuryConn:
			kind, direction, asset = entryCardDebit, directionIn, token
		case e.name == eventDebited && e.user == account:
			kind, direction, asset = entryCardDebit, directionOut, token
		case e.name == eventRefunded && treasuryConn:
			kind, direction, asset = entryCardRefund, directionOut, token
		case e.name == eventRefunded && e.user == account:
			kind, direction, asset = entryCardRefund, directionIn, token
		case e.name != eventTransfer, e.paired, e.from == e.to, e.amount.Sign() == 0:
			continue
		case e.to == account:
			kind, direction = entryDeposit, directionIn
		case e.from == account:
			kind, direction = entryWithdrawal, directionOut
		default:
			continue
		}
		alias, ok := s.alias(asset)
		if !ok {
			return nil, fmt.Errorf("evm: log %s: the token %s has no alias row", e.log.externalID(), asset.Hex())
		}
		decimals, err := tokenDecimals(alias)
		if err != nil {
			return nil, err
		}
		raw, err := storedLog(e)
		if err != nil {
			return nil, err
		}
		entries = append(entries, connector.Entry{
			ExternalID: e.log.externalID(), Leg: legSingle, Type: kind, Direction: direction,
			NativeAsset: alias.NativeAsset, Amount: FormatUnits(e.amount, decimals),
			OccurredAt: times[e.log.BlockHash], Raw: raw,
		})
	}
	return entries, nil
}

// alias returns the alias row of a token address, regardless of case.
func (s *session) alias(address common.Address) (connector.Alias, bool) {
	for _, a := range s.src.Aliases {
		if strings.EqualFold(a.NativeAsset, address.Hex()) {
			return a, true
		}
	}
	return connector.Alias{}, false
}

// storedLog is ledger_entries.raw (§2.4 Stored log): the log as received with event and args added.
func storedLog(e *event) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(e.log.raw, &obj); err != nil {
		return nil, fmt.Errorf("evm: log %s is not a JSON object", e.log.externalID())
	}
	name, _ := json.Marshal(e.name)
	args, _ := json.Marshal(e.args())
	obj["event"], obj["args"] = name, args
	return json.Marshal(obj)
}

// readPage runs UC-303 steps 6 to 9 from next, the final block being final.
func (s *session) readPage(ctx context.Context, mode connector.Mode, next, final uint64) (connector.Page, error) {
	account, err := CheckAddress(s.conn.Account)
	if err != nil {
		return connector.Page{}, fmt.Errorf("evm: the account of connection %s is not a wallet address", s.conn.ID)
	}
	who := common.HexToAddress(account)
	treasuryConn := s.cfg.TreasuryConnection != "" && strings.EqualFold(s.cfg.TreasuryConnection, s.conn.ID)

	// Step 6.
	from, to, logs, err := s.readRange(ctx, s.filters(who, treasuryConn), next, final)
	if err != nil {
		return connector.Page{}, err
	}

	// Step 7: a lagging node may answer no logs for blocks it has not seen; the header of the range end shows it.
	end, err := s.headerAt(ctx, hexutil.EncodeUint64(to))
	if err != nil {
		return connector.Page{}, err
	}
	if end == nil || uint64(end.Number) != to {
		return connector.Page{}, fmt.Errorf("evm: %s: the endpoint has no block %d, the end of blocks %d to %d: "+
			"a lagging node (EC-307)", s.variable(), to, from, to)
	}
	times, err := s.blockTimes(ctx, logs)
	if err != nil {
		return connector.Page{}, err
	}

	// Step 8.
	s.net.mu.Lock()
	token := s.net.token
	s.net.mu.Unlock()
	var treasury common.Address
	if slices.ContainsFunc(logs, func(l chainLog) bool { return len(l.Topics) > 0 && l.Topics[0] != topicTransfer }) {
		if treasury, err = s.treasuryAddress(ctx); err != nil {
			return connector.Page{}, err
		}
	}
	entries, err := s.mapLogs(logs, times, who, token, treasury, treasuryConn)
	if err != nil {
		return connector.Page{}, err
	}

	// Step 9: BACKFILL until the first page that ends at the final block.
	nextBlock := to + 1
	cur, err := json.Marshal(cursor{NextBlock: &nextBlock, LastHash: end.Hash.Hex(),
		LastTime: time.Unix(int64(end.Time), 0).UTC().Format(time.RFC3339)})
	if err != nil {
		return connector.Page{}, err
	}
	if to == final {
		mode = connector.ModeIncremental
	}
	return connector.Page{Entries: entries, Cursor: cur, Mode: mode, More: to < final}, nil
}
