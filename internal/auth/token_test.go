package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

var tokenFormat = regexp.MustCompile(`^cas_[0-9a-f]{12}_[0-9a-f]{64}$`)

// C1-T411 — Req: UC-105. The format part without a database; cmd/casctl checks issued tokens.
func TestT411_TokenFormat(t *testing.T) {
	token1, keyID1, hash1, err := New()
	if err != nil {
		t.Fatal(err)
	}
	token2, keyID2, _, err := New()
	if err != nil {
		t.Fatal(err)
	}

	for _, tok := range []string{token1, token2} {
		if !tokenFormat.MatchString(tok) {
			t.Errorf("token %q does not match cas_<12 hex>_<64 hex>", tok)
		}
	}
	if token1 == token2 || keyID1 == keyID2 {
		t.Error("two tokens are equal")
	}

	keyID, secret, err := Parse(token1)
	if err != nil {
		t.Fatal(err)
	}
	if keyID != keyID1 || "cas_"+keyID+"_"+hex.EncodeToString(secret) != token1 {
		t.Error("Parse does not return the parts of the token")
	}
	want := sha256.Sum256(secret)
	if hex.EncodeToString(hash1) != hex.EncodeToString(want[:]) {
		t.Error("hash is not the SHA-256 of the 32 secret bytes")
	}
	if !Verify(secret, hash1) {
		t.Error("Verify rejects the right secret")
	}
	secret[0] ^= 1
	if Verify(secret, hash1) {
		t.Error("Verify accepts a wrong secret")
	}

	valid := token1
	for name, bad := range map[string]string{
		"empty":              "",
		"no prefix":          strings.TrimPrefix(valid, "cas_"),
		"other prefix":       "cat_" + valid[4:],
		"upper-case key_id":  "cas_ABCDEF012345" + valid[16:],
		"upper-case secret":  valid[:17] + "A" + valid[18:],
		"short secret":       valid[:len(valid)-2],
		"long secret":        valid + "00",
		"separator missing":  valid[:16] + "0" + valid[17:],
		"non-hex character":  valid[:20] + "g" + valid[21:],
		"Bearer not removed": "Bearer " + valid,
	} {
		if _, _, err := Parse(bad); err != ErrMalformed {
			t.Errorf("%s: Parse = %v, want ErrMalformed", name, err)
		}
	}
}
