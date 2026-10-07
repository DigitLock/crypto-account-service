package repository

import (
	"context"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Written by hand, not by sqlc: the alias rows as the Source of a connector carries them (SRS — Core Connector
// contract; S3 D-31).

// AliasesBySource returns the alias rows of every source by source code, in one query: for the places that build
// the Source of many rows.
func (q *Queries) AliasesBySource(ctx context.Context) (map[string][]connector.Alias, error) {
	rows, err := q.ListAliases(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]connector.Alias{}
	for _, r := range rows {
		out[r.SourceCode] = append(out[r.SourceCode], connector.Alias{
			NativeAsset: r.NativeAsset, Asset: r.Asset, Decimals: r.Decimals,
		})
	}
	return out, nil
}

// AliasesOfSource returns the alias rows of one source.
func (q *Queries) AliasesOfSource(ctx context.Context, code string) ([]connector.Alias, error) {
	rows, err := q.ListAliasesOfSource(ctx, code)
	if err != nil {
		return nil, err
	}
	out := make([]connector.Alias, 0, len(rows))
	for _, r := range rows {
		out = append(out, connector.Alias{NativeAsset: r.NativeAsset, Asset: r.Asset, Decimals: r.Decimals})
	}
	return out, nil
}
