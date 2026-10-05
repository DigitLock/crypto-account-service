package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newVault(t *testing.T, version int) (*Envelope, []byte) {
	t.Helper()
	key := randomBytes(t, 32)
	v, err := New(key, version)
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

func connectionID(t *testing.T) [16]byte {
	t.Helper()
	return [16]byte(randomBytes(t, 16))
}

// secretJSON is the plaintext the registry stores: the key and the secret as one JSON object.
func secretJSON(t *testing.T) (plaintext []byte, apiKey, apiSecret string) {
	t.Helper()
	apiKey = fmt.Sprintf("%x", randomBytes(t, 32))
	apiSecret = fmt.Sprintf("%x", randomBytes(t, 32))
	plaintext, err := json.Marshal(map[string]string{"api_key": apiKey, "api_secret": apiSecret})
	if err != nil {
		t.Fatal(err)
	}
	return plaintext, apiKey, apiSecret
}

// C1-T513 — Req: FR-103, ADR-4. The unit part; internal/grpc/api reads the stored column.
func TestT513_EncryptedAtRest(t *testing.T) {
	v, _ := newVault(t, 3)
	id := connectionID(t)
	plaintext, apiKey, apiSecret := secretJSON(t)

	block, version, err := v.Encrypt(id, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Errorf("kek version = %d, want 3", version)
	}
	for _, clear := range [][]byte{plaintext, []byte(apiKey), []byte(apiSecret)} {
		if bytes.Contains(block, clear) {
			t.Error("the block contains the secret in clear")
		}
	}
	if block[0] != 1 || len(block) != 1+12+48+12+len(plaintext)+16 {
		t.Errorf("block: format %d, length %d; want format 1 and the layout of ADR-4", block[0], len(block))
	}

	got, err := v.Decrypt(id, block, version)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt = %q, %v; want the original", got, err)
	}
}

// C1-T514 — Req: ADR-4
func TestT514_CiphertextBoundToItsRow(t *testing.T) {
	v, _ := newVault(t, 1)
	plaintext, _, _ := secretJSON(t)
	first, second := connectionID(t), connectionID(t)

	block, version, err := v.Encrypt(first, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Decrypt(second, block, version)
	if err != ErrDecrypt || got != nil {
		t.Errorf("Decrypt with another connection ID = %q, %v; want no plaintext and ErrDecrypt", got, err)
	}
}

// C1-T515 — Req: ADR-4, §3.1
func TestT515_FailClosed(t *testing.T) {
	v, key := newVault(t, 1)
	id := connectionID(t)
	plaintext, _, _ := secretJSON(t)
	block, version, err := v.Encrypt(id, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	otherKey, _ := newVault(t, 1)
	sameKeyVersion2, err := New(key, 2)
	if err != nil {
		t.Fatal(err)
	}
	type attempt struct {
		v       Vault
		block   []byte
		version int16
	}
	cases := map[string]attempt{
		"another master key":                 {otherKey, block, version},
		"stored version differs from config": {v, block, 2},
		"configured version differs":         {sameKeyVersion2, block, version},
		"empty block":                        {v, nil, version},
		"truncated block":                    {v, block[:len(block)-1], version},
		"unknown format":                     {v, append([]byte{2}, block[1:]...), version},
	}
	for i := range block {
		changed := bytes.Clone(block)
		changed[i] ^= 0x01
		cases[fmt.Sprintf("byte %d changed", i)] = attempt{v, changed, version}
	}
	for name, c := range cases {
		got, err := c.v.Decrypt(id, c.block, c.version)
		if err != ErrDecrypt || got != nil {
			t.Errorf("%s: Decrypt = %q, %v; want no plaintext and ErrDecrypt", name, got, err)
		}
	}
	if ErrDecrypt.Error() != "vault: cannot decrypt the secret" {
		t.Errorf("error %q is not the fixed message", ErrDecrypt)
	}

	for _, bad := range []struct {
		key     []byte
		version int
	}{{key[:31], 1}, {key, 0}, {key, 32768}} {
		if _, err := New(bad.key, bad.version); err == nil {
			t.Errorf("New accepted a %d-byte key with version %d", len(bad.key), bad.version)
		}
	}
}

// C1-T516 — Req: ADR-4
func TestT516_FreshKeyMaterial(t *testing.T) {
	v, _ := newVault(t, 1)
	id := connectionID(t)
	plaintext, _, _ := secretJSON(t)

	first, _, err := v.Encrypt(id, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := v.Encrypt(id, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of the same secret are equal")
	}
	// Nonces and the wrapped data key differ too, not only the last bytes.
	if bytes.Equal(first[1:13], second[1:13]) || bytes.Equal(first[13:61], second[13:61]) ||
		bytes.Equal(first[61:73], second[61:73]) {
		t.Error("a nonce or the wrapped data key repeats")
	}

	t.Run("Secret prints redacted", func(t *testing.T) {
		s := NewSecret("value-" + fmt.Sprintf("%x", randomBytes(t, 8)))
		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "s", s)
		js, _ := json.Marshal(struct{ S Secret[string] }{s})
		out := fmt.Sprintf("%v %+v %#v %s %q %x", s, s, s, s, s, s) + buf.String() + string(js)
		if bytes.Contains([]byte(out), []byte(s.Value())) {
			t.Errorf("Secret printed its value: %s", out)
		}
	})
}
