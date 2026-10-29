package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"strings"
)

const ciphertextVersion byte = 1

// ciphertextV1AADPrefix 是 v1 密文格式的一部分，不是当前项目显示名
// 数据库中已有密文的认证标签绑定这些字节，品牌改名不能修改该值
// 如需改变命名域，必须另设格式版本并显式迁移已有密文
const ciphertextV1AADPrefix = "heago-scaffold/settings:v1\x00"

// The version byte leaves room for a future explicit key/cipher migration.
// It does not implement automatic rotation: changing the master file before
// re-encrypting existing rows must fail closed rather than discard credentials.
func newCipher(key string) (cipher.AEAD, error) {
	// A random 32-byte file key is accepted as Base64, rather than silently
	// hashing a human password into a weaker-looking but equally weak AES key.
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(raw) != 32 {
		return nil, ErrInvalidInput
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, ErrInvalidInput
	}
	return cipher.NewGCM(block)
}

func additionalData(key string) []byte {
	// Binding ciphertext to both its purpose and setting key prevents a valid
	// encrypted value being copied into another row without detection.
	return []byte(ciphertextV1AADPrefix + key)
}

func (s *Service) seal(key string, value []byte) ([]byte, error) {
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrUnavailable
	}
	// Every write, including clearing a secret, gets a fresh nonce. Empty
	// secrets remain authenticated so a wrong master key is detected at startup.
	payload := append([]byte{ciphertextVersion}, nonce...)
	return s.cipher.Seal(payload, nonce, value, additionalData(key)), nil
}

func (s *Service) open(key string, payload []byte) ([]byte, error) {
	// Check framing before slicing attacker-controlled database bytes. GCM's
	// authentication tag also protects an encrypted empty string from tampering.
	nonceSize := s.cipher.NonceSize()
	if len(payload) < 1+nonceSize+s.cipher.Overhead() || payload[0] != ciphertextVersion {
		return nil, ErrUnavailable
	}
	plain, err := s.cipher.Open(nil, payload[1:1+nonceSize], payload[1+nonceSize:], additionalData(key))
	if err != nil {
		// Authentication failures do not expose key material or database bytes
		// through an upstream API error, even when the record was tampered with.
		return nil, ErrUnavailable
	}
	return plain, nil
}
