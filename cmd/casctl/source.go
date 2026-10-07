package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// Keys of casctl source set (SRS — Core UC-105 row 8; S3 D-17, S3 D-27).
const (
	keyControllerAddress = "controller_address"
	keyBackfillFloor     = "backfill_floor"
	keyTokenAddress      = "token_address"
)

// Errors of source set (EC-122). None quotes the value.
var (
	errUnknownSourceKey = errors.New("the key must be one of controller_address, backfill_floor, token_address")
	errInvalidAddress   = errors.New("the value must be an address: 0x and 40 hexadecimal characters, " +
		"with a valid EIP-55 checksum when in mixed case, not the zero address")
	errInvalidBlock = errors.New("backfill_floor must be a block number: an integer from 0 to 9223372036854775807")
)

// setSourceValue checks one key=value of an EVM source and writes it through the registry: controller_address
// in EIP-55 form and backfill_floor as a JSON number to sources.config; token_address in EIP-55 form to the
// alias of the tracked token. It returns the previous value, empty when unset, and the new value as stored.
func setSourceValue(ctx context.Context, reg *registry.Registry, code, key, value string) (previous, stored string, err error) {
	switch key {
	case keyControllerAddress, keyTokenAddress:
		// UC-301: the input is not trimmed.
		address, err := evm.CheckAddress(value)
		if err != nil {
			return "", "", errInvalidAddress
		}
		if key == keyTokenAddress {
			previous, err = reg.SetTrackedToken(ctx, code, address)
			return previous, address, err
		}
		raw, _ := json.Marshal(address)
		previous, err = reg.SetSourceConfigValue(ctx, code, key, raw)
		return previous, address, err
	case keyBackfillFloor:
		// Digits only: no sign, no fraction, no exponent, no space. Leading zeros are dropped.
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n > math.MaxInt64 || strings.TrimLeft(value, "0123456789") != "" {
			return "", "", errInvalidBlock
		}
		stored = strconv.FormatUint(n, 10)
		previous, err = reg.SetSourceConfigValue(ctx, code, key, json.RawMessage(stored))
		return previous, stored, err
	default:
		return "", "", errUnknownSourceKey
	}
}
