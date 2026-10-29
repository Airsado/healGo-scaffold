package settings

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
)

func TestHistoricalV1CiphertextSurvivesProjectRename(t *testing.T) {
	// 全部为公开测试材料：key 为 00..1f，nonce 为 00..0b
	// 固定 nonce 只用于复验向量，生产 seal 仍使用随机 nonce
	const publicKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	const settingKey = "transport.api_key"
	const plaintext = `"legacy-secret-value"`
	// 用固定字节冻结改名前的 AAD 与载荷，不引用生产格式常量
	// 若测试与实现一起按项目名重新生成，普通 roundtrip 无法发现历史数据失效
	const aadHex = "686561676f2d73636166666f6c642f73657474696e67733a7631007472616e73706f72742e6170695f6b6579"
	const payloadHex = "01000102030405060708090a0b656eb37ca486bb36fe24f4f9d49d551be2baf251d2d5d8355f64bb663e4d59f6303c720b9d"
	decodeHex := func(value string) []byte {
		t.Helper()
		decoded, err := hex.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	payload := decodeHex(payloadHex)
	nonce := decodeHex("000102030405060708090a0b")
	rawKey, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	// 独立按 AES-256-GCM 和历史格式复验公开向量
	// 载荷布局固定为版本字节 01、12 字节 nonce、密文及 16 字节认证标签
	block, err := aes.NewCipher(rawKey)
	if err != nil {
		t.Fatal(err)
	}
	historicalCipher, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	reconstructed := historicalCipher.Seal(append([]byte{1}, nonce...), nonce, []byte(plaintext), decodeHex(aadHex))
	if !bytes.Equal(reconstructed, payload) {
		t.Fatal("historical v1 fixture does not match its published inputs")
	}
	// 当前实现必须能打开固定的历史载荷，不能只验证当前 seal/open 互相兼容
	currentCipher, err := newCipher(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{cipher: currentCipher}
	got, err := service.open(settingKey, payload)
	if err != nil || string(got) != plaintext {
		t.Fatal("current implementation cannot open historical v1 ciphertext", err)
	}
	// 保持密文与 setting key 的绑定，另一个配置项不能复用此载荷
	if _, err := service.open("transport.other_key", payload); !errors.Is(err, ErrUnavailable) {
		t.Fatal("historical ciphertext was accepted for another setting key")
	}
}
