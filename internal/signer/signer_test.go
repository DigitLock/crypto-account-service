package signer

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// newKey returns a key generated at run time and its signer.
func newKey(t *testing.T) ([]byte, *KeySigner) {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	raw := crypto.FromECDSA(k)
	s, err := New(vault.NewSecret(raw))
	if err != nil {
		t.Fatal(err)
	}
	if s.Address() != crypto.PubkeyToAddress(k.PublicKey) {
		t.Fatal("Address is not the account of the key")
	}
	return raw, s
}

func TestSignTxRecoversToAddress(t *testing.T) {
	_, s := newKey(t)
	to := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	for _, chainID := range []int64{31337, 84532} {
		for name, tx := range map[string]*types.Transaction{
			"EIP-1559": types.NewTx(&types.DynamicFeeTx{
				ChainID: big.NewInt(chainID), Nonce: 7, GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2),
				Gas: 100000, To: &to, Value: big.NewInt(0), Data: []byte{0x01, 0x02},
			}),
			"legacy": types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(1), Gas: 21000, To: &to}),
		} {
			t.Run(fmt.Sprintf("%s on %d", name, chainID), func(t *testing.T) {
				signed, err := s.SignTx(tx, big.NewInt(chainID))
				if err != nil {
					t.Fatal(err)
				}
				from, err := types.Sender(types.LatestSignerForChainID(big.NewInt(chainID)), signed)
				if err != nil {
					t.Fatal(err)
				}
				if from != s.Address() {
					t.Errorf("signed transaction recovers to %s, want %s", from, s.Address())
				}
				if signed.ChainId().Int64() != chainID {
					t.Errorf("chain ID of the signed transaction = %d, want %d", signed.ChainId(), chainID)
				}
			})
		}
	}
}

func TestSignTxRejectsMissingChainID(t *testing.T) {
	_, s := newKey(t)
	tx := types.NewTx(&types.LegacyTx{Gas: 21000})
	for _, id := range []*big.Int{nil, big.NewInt(0), big.NewInt(-1)} {
		if _, err := s.SignTx(tx, id); err == nil {
			t.Errorf("SignTx accepted chain ID %v", id)
		}
	}
}

func TestNewRejectsInvalidKey(t *testing.T) {
	order := crypto.S256().Params().N.Bytes()
	for name, key := range map[string][]byte{
		"zero":            make([]byte, 32),
		"curve order":     order,
		"31 bytes":        bytes.Repeat([]byte{0x11}, 31),
		"33 bytes":        bytes.Repeat([]byte{0x11}, 33),
		"empty":           nil,
		"above the order": bytes.Repeat([]byte{0xff}, 32),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New(vault.NewSecret(key))
			if !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("New = %v, want ErrInvalidKey", err)
			}
			if len(key) > 0 && strings.Contains(err.Error(), hex.EncodeToString(key)) {
				t.Error("the error contains the key")
			}
		})
	}
}

func TestFormattingNeverShowsTheKey(t *testing.T) {
	raw, s := newKey(t)
	secrets := []string{hex.EncodeToString(raw), strings.ToUpper(hex.EncodeToString(raw)), string(raw),
		new(big.Int).SetBytes(raw).String(), new(big.Int).SetBytes(raw).Text(16)}

	var out []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		out = append(out, fmt.Sprintf(verb, s), fmt.Sprintf(verb, *s))
		out = append(out, fmt.Sprintf(verb, Signer(s)))
		out = append(out, fmt.Sprintf(verb, struct{ S *KeySigner }{s}))
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("start", "signer", s)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("start", "signer", s)
	js, err := json.Marshal(struct{ S *KeySigner }{s})
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, buf.String(), string(js))

	for _, o := range out {
		for _, secret := range secrets {
			if strings.Contains(o, secret) {
				t.Fatalf("output contains the key: %q", o)
			}
		}
		if !strings.Contains(o, redacted) {
			t.Errorf("output is not redacted: %q", o)
		}
	}
}
