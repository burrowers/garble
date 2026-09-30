// Copyright (c) 2026, The Garble Authors.
// See LICENSE for licensing information.

package main

import (
	"fmt"
	"go/token"
	"math/big"
	"slices"
	"strings"
)

func parseWordList(text string) ([]string, int, error) {
	var words []string
	for line, word := range strings.Split(text, "\n") {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}
		if len(word) > 8 {
			return nil, 0, fmt.Errorf("wordlist line %d: words must be at most 8 letters", line+1)
		}
		for _, b := range []byte(word) {
			if !isLower(b) {
				return nil, 0, fmt.Errorf("wordlist line %d: expected lowercase ASCII letters", line+1)
			}
		}
		words = append(words, word)
	}
	slices.Sort(words)
	words = slices.Compact(words)
	if len(words) < 2 || len(words) > 65536 {
		return nil, 0, fmt.Errorf("wordlist requires 2 to 65536 distinct words")
	}
	count := 0
	space := uint64(1)
	for count < 2 || space < 1<<48 {
		space *= uint64(len(words))
		count++
	}
	maxLength := 0
	for _, word := range words {
		if len(word) > maxLength {
			maxLength = len(word)
		}
	}
	if count*(maxLength+1)-1 > 128 {
		return nil, 0, fmt.Errorf("wordlist would produce names longer than 128 bytes; supply more distinct or shorter words")
	}
	return words, count, nil
}

func wordListName(sum []byte, name string) string {
	words := sharedCache.Words
	value := new(big.Int).SetBytes(sum)
	base := big.NewInt(int64(len(words)))
	remainder := new(big.Int)
	parts := make([]string, sharedCache.WordCount)
	for i := range parts {
		value.QuoRem(value, base, remainder)
		parts[i] = words[remainder.Int64()]
	}
	result := strings.Join(parts, "_")
	if token.IsIdentifier(name) && token.IsExported(name) {
		result = string(toUpper(result[0])) + result[1:]
	}
	return result
}
