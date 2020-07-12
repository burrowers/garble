package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	mathrand "math/rand"
	"strings"
)

// If math/rand.Seed() is not called, the generator behaves as if seeded by rand.Seed(1),
// so the generator is deterministic.

// genAesKey generates a 128-bit AES Key.
func genAesKey() []byte {
	return genRandBytes(16)
}

// genAesKey generates a 128-bit nonce.
func genNonce() []byte {
	return genRandBytes(12)
}

// genRandBytes return a random []byte with the length of size.
func genRandBytes(size int) []byte {
	buffer := make([]byte, size)

	if strings.HasPrefix(envGarbleSeed, "random;") {
		_, err := rand.Read(buffer)
		if err != nil {
			panic(fmt.Sprintf("couldn't generate random key:  %v", err))
		}
	} else {
		mathrand.Read(buffer) // error is always nil so save to ignore
	}

	return buffer
}

func genRandUInt32() uint32 {
	bytes := genRandBytes(4)
	return binary.LittleEndian.Uint32(bytes)
}

func genRandUInt32Max(max uint32) uint32 {
	return genRandUInt32() % max
}

// encAES encrypt data with key in AES GCM mode.
func encAES(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	nonce := genNonce()

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	ciphertext := aesgcm.Seal(nil, nonce, data, nil)
	encData := append(nonce, ciphertext...)
	return encData, nil
}
