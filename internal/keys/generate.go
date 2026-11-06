package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/big"
	"strings"
)

const (
	keyPrefix    = "cmp_"
	keyEntropy   = 32
	encodedChars = 43
	keyLength    = len(keyPrefix) + encodedChars
	digestSize   = sha256.Size

	idPrefix  = "k_"
	idEntropy = 16

	// The alphabet is in ASCII order, so comparing two encodings of equal length
	// as strings gives the same answer as comparing the integers they encode.
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// maxEncoded is the encoding of 2^256-1. 62^43 is slightly larger than 2^256, so a
// well-formed string can still represent more than 256 bits.
var maxEncoded = encodeBase62([]byte(strings.Repeat("\xff", keyEntropy)))

func encodeBase62(raw []byte) string {
	value := new(big.Int).SetBytes(raw)
	base := big.NewInt(int64(len(base62Alphabet)))
	remainder := new(big.Int)
	out := make([]byte, encodedChars)
	for i := len(out) - 1; i >= 0; i-- {
		value.DivMod(value, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

func decodeBase62(encoded string) []byte {
	value := new(big.Int)
	base := big.NewInt(int64(len(base62Alphabet)))
	digit := new(big.Int)
	for i := range len(encoded) {
		digit.SetInt64(int64(strings.IndexByte(base62Alphabet, encoded[i])))
		value.Mul(value, base)
		value.Add(value, digit)
	}
	return value.FillBytes(make([]byte, keyEntropy))
}

func generateKey(entropy io.Reader) (Key, error) {
	raw := make([]byte, keyEntropy)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return Key{}, ErrEntropy
	}
	return Key{value: keyPrefix + encodeBase62(raw)}, nil
}

func generateID(entropy io.Reader) (string, error) {
	raw := make([]byte, idEntropy)
	if _, err := io.ReadFull(entropy, raw); err != nil {
		return "", ErrEntropy
	}
	return idPrefix + hex.EncodeToString(raw), nil
}

// ParseKey validates the syntax of a presented key. The caller reports every
// failure the same way, so the error says nothing about which rule was broken.
func ParseKey(presented string) (Key, error) {
	if len(presented) != keyLength || !strings.HasPrefix(presented, keyPrefix) {
		return Key{}, ErrInvalidKey
	}
	encoded := presented[len(keyPrefix):]
	for i := range len(encoded) {
		if !isBase62(encoded[i]) {
			return Key{}, ErrInvalidKey
		}
	}
	if encoded > maxEncoded {
		return Key{}, ErrInvalidKey
	}
	return Key{value: presented}, nil
}

func isBase62(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func digestOf(value string) Digest {
	return Digest(sha256.Sum256([]byte(value)))
}

func digestHex(d Digest) string {
	return hex.EncodeToString(d[:])
}
