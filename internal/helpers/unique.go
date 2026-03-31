package helpers

import (
	"crypto/sha256"
	"math/big"
	"strings"
)

func UniqueID(s string) string {
	hash := sha256.Sum256([]byte(strings.ToLower(s)))
	return new(big.Int).SetBytes(hash[:]).Text(36)[:10]
}
