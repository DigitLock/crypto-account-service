package registry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

// The alias of the tracked token, as the seed data of SRS — EVM Connector §2.4: MockUSDC.
const (
	TrackedTokenAsset    = "USDC"
	TrackedTokenDecimals = 6
)

// ErrSourceNotEVM refuses a change of casctl source set on a source of another kind (EC-122). An unknown source
// is ErrSourceNotFound, as for Create.
var ErrSourceNotEVM = errors.New("the source is not of kind EVM")

// SetSourceConfigValue writes one key of sources.config of an EVM source and keeps the other keys, in one
// transaction (SRS — Core UC-105 row 8). value is the JSON value, checked by the caller. It returns the previous
// value: a JSON string unquoted, any other JSON as written, empty when the key was not set. No audit row: sources
// is not tenant data.
func (r *Registry) SetSourceConfigValue(ctx context.Context, code, key string, value json.RawMessage) (string, error) {
	var previous string
	err := r.inTx(ctx, func(q *repository.Queries) error {
		src, err := lockEVMSource(ctx, q, code)
		if err != nil {
			return err
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(src.Config, &config); err != nil {
			return errors.New("sources.config of the source is not a JSON object")
		}
		if old, ok := config[key]; ok {
			previous = string(old)
			var s string
			if json.Unmarshal(old, &s) == nil {
				previous = s
			}
		}
		return q.SetSourceConfigValue(ctx, repository.SetSourceConfigValueParams{ID: src.ID, Key: key, Value: value})
	})
	return previous, err
}

// SetTrackedToken writes the alias of the tracked token of an EVM source in one transaction (SRS — Core UC-105
// row 8; S3 D-27): address, in EIP-55 form and checked by the caller, → USDC, decimals 6, replacing the USDC
// alias of an earlier address. It returns the previous addresses, empty when there was none. No audit row:
// asset_aliases is not tenant data.
func (r *Registry) SetTrackedToken(ctx context.Context, code, address string) (string, error) {
	var previous []string
	err := r.inTx(ctx, func(q *repository.Queries) error {
		src, err := lockEVMSource(ctx, q, code)
		if err != nil {
			return err
		}
		alias := repository.ListSourceAliasesOfAssetParams{SourceID: src.ID, Asset: TrackedTokenAsset}
		if previous, err = q.ListSourceAliasesOfAsset(ctx, alias); err != nil {
			return err
		}
		if err := q.DeleteSourceAliasesOfAsset(ctx, repository.DeleteSourceAliasesOfAssetParams(alias)); err != nil {
			return err
		}
		decimals := int16(TrackedTokenDecimals)
		return q.InsertSourceAlias(ctx, repository.InsertSourceAliasParams{
			SourceID: src.ID, NativeAsset: address, Asset: TrackedTokenAsset, Decimals: &decimals,
		})
	})
	if err != nil {
		return "", err
	}
	return strings.Join(previous, ", "), nil
}

// lockEVMSource returns the source row, locked until the end of the transaction, or the error of EC-122.
func lockEVMSource(ctx context.Context, q *repository.Queries, code string) (repository.Source, error) {
	src, err := q.LockSourceByCode(ctx, code)
	if err != nil {
		return repository.Source{}, notFound(err, ErrSourceNotFound)
	}
	if src.Kind != connector.KindEVM {
		return repository.Source{}, ErrSourceNotEVM
	}
	return src, nil
}
