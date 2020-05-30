package main

import (
	"crypto/aes"
	"crypto/cipher"
	"math/rand"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano()) // TODO: Use build ID as seed
}

// genAesKey generates a 128bit AES Key
func genAesKey() []byte {
	return genRandBytes(16)
}

// genAesKey generates a 128bit nonce
func genNonce() []byte {
	return genRandBytes(12)
}

// genRandBytes return a random []byte with the length of size
func genRandBytes(size int) []byte {
	buffer := make([]byte, size)
	rand.Read(buffer) // error is always nil so save to ignore
	return buffer
}

// encAes encrypt data with AesKey in AES gcm mode
func encAes(data []byte, AesKey []byte) ([]byte, error) {
	block, _ := aes.NewCipher(AesKey)
	nonce := genNonce()

	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	ciphertext := aesgcm.Seal(nil, nonce, data, nil)
	encData := append(nonce, ciphertext...)
	return encData, nil
}
