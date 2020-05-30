package main

import (
	"crypto/aes"
	"crypto/cipher"
)

var key []byte = []byte{1, 2, 3}

func main() {
	decryptName([]byte{1, 2, 3})
}

func decryptName(ciphertext []byte) string {
	block, _ := aes.NewCipher(key)
	aesgcm, _ := cipher.NewGCM(block)
	plaintext, _ := aesgcm.Open(nil, ciphertext[:12], ciphertext[12:], nil)
	return string(plaintext)
}
