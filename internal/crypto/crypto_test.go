package crypto

import (
	"encoding/hex"
	"strings"
	"testing"
)

// TestNewServiceRejectsEmptyKey 空密钥必须报错——"没有密钥就加密"是安全事故
func TestNewServiceRejectsEmptyKey(t *testing.T) {
	if _, err := NewService(""); err == nil {
		t.Fatal("NewService(\"\") error = nil, want error")
	}
}

// TestEncryptDecryptRoundTrip 加解密往返：解密(加密(x)) == x
func TestEncryptDecryptRoundTrip(t *testing.T) {
	svc, err := NewService(DefaultSecretKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	for _, plaintext := range []string{
		"sk-test-123",
		"sk-ant-api03-abcDEF1234567890-xyz",
		"", // 空串也应当能安全往返（虽然业务上不会存空密钥）
		"包含中文与特殊字符 !@#$%^&*()_+",
	} {
		ciphertext, err := svc.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q) error = %v", plaintext, err)
		}
		if ciphertext == plaintext {
			t.Fatalf("Encrypt(%q) 返回了明文本身", plaintext)
		}
		got, err := svc.Decrypt(ciphertext)
		if err != nil {
			t.Fatalf("Decrypt(Encrypt(%q)) error = %v", plaintext, err)
		}
		if got != plaintext {
			t.Fatalf("Decrypt 结果 = %q, want %q", got, plaintext)
		}
	}
}

// TestEncryptProducesDifferentCiphertext 随机 nonce：同明文两次加密结果必须不同
// （防止密文模式泄露：如果两次结果一样，攻击者就能判断"两条记录是同一个密钥"）
func TestEncryptProducesDifferentCiphertext(t *testing.T) {
	svc, err := NewService(DefaultSecretKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	first, err := svc.Encrypt("sk-same-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	second, err := svc.Encrypt("sk-same-value")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if first == second {
		t.Fatal("同一明文两次加密结果相同：nonce 可能未随机化")
	}
}

// TestDecryptWithWrongKeyFails 换密钥后旧密文必须解不开（这是"密钥即命根"的体现）
func TestDecryptWithWrongKeyFails(t *testing.T) {
	svcA, err := NewService("aaaa...") // 任意密钥材料，走 SHA-256 派生
	if err != nil {
		t.Fatalf("NewService(A) error = %v", err)
	}
	svcB, err := NewService("bbbb...")
	if err != nil {
		t.Fatalf("NewService(B) error = %v", err)
	}

	ciphertext, err := svcA.Encrypt("sk-secret")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	if _, err := svcB.Decrypt(ciphertext); err == nil {
		t.Fatal("用错误密钥解密成功，want error")
	}
}

// TestDecryptTamperedFails GCM 认证特性：篡改密文任何一个字节都必须解密失败
func TestDecryptTamperedFails(t *testing.T) {
	svc, err := NewService(DefaultSecretKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	ciphertext, err := svc.Encrypt("sk-secret")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}

	// 篡改密文中间的某个字符（把字符值 +1，保证仍是合法 hex 但内容变了）
	raw, _ := hex.DecodeString(ciphertext)
	raw[len(raw)/2] ^= 0x01
	tampered := hex.EncodeToString(raw)
	if tampered == ciphertext {
		t.Fatal("篡改失败：字符没变")
	}
	if _, err := svc.Decrypt(tampered); err == nil {
		t.Fatal("篡改后的密文解密成功，want error（GCM 认证应拦截）")
	}
}

// TestDecryptInvalidInputFails 非 hex / 太短的输入必须报错，不能静默返回
func TestDecryptInvalidInputFails(t *testing.T) {
	svc, err := NewService(DefaultSecretKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	for _, bad := range []string{
		"",                           // 空
		"sk-test-123",                // 明文混进来
		"zzzzzzzzzzzzzzzzzzzzzzzz",   // 非 hex
		"0123456789abcdef0123456789", // hex 但太短（不够 nonce+tag）
	} {
		if _, err := svc.Decrypt(bad); err == nil {
			t.Fatalf("Decrypt(%q) error = nil, want error", bad)
		}
	}
}

// TestDecryptOrRawLegacyCompat 旧明文兼容：不是密文格式的数据原样返回
// （迁移期：老库里可能还躺着未加密的明文密钥）
func TestDecryptOrRawLegacyCompat(t *testing.T) {
	svc, err := NewService(DefaultSecretKey)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	// ① 明文（sk- 开头、非 hex）→ 原样返回
	legacy := "sk-legacy-plaintext"
	got, err := svc.DecryptOrRaw(legacy)
	if err != nil {
		t.Fatalf("DecryptOrRaw(明文) error = %v", err)
	}
	if got != legacy {
		t.Fatalf("DecryptOrRaw(明文) = %q, want 原样 %q", got, legacy)
	}
	impl := svc.(*aesGCMService) // 白盒测试：IsEncrypted 是内部方法，不进接口
	if impl.IsEncrypted(legacy) {
		t.Fatal("明文被误判为密文")
	}

	// ② 新密文 → 正确解密
	ciphertext, err := svc.Encrypt("sk-new-key")
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	got, err = svc.DecryptOrRaw(ciphertext)
	if err != nil {
		t.Fatalf("DecryptOrRaw(密文) error = %v", err)
	}
	if got != "sk-new-key" {
		t.Fatalf("DecryptOrRaw(密文) = %q, want sk-new-key", got)
	}
}

// TestDeriveKeyForms 密钥派生三种形态
func TestDeriveKeyForms(t *testing.T) {
	// ① 64 位 hex → 恰好 32 字节（直接解码）
	key, err := deriveKey(DefaultSecretKey)
	if err != nil {
		t.Fatalf("deriveKey(64hex) error = %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("deriveKey(64hex) 长度 = %d, want 32", len(key))
	}

	// ② 任意字符串 → SHA-256 派生出 32 字节
	key2, err := deriveKey("my-arbitrary-secret")
	if err != nil {
		t.Fatalf("deriveKey(任意串) error = %v", err)
	}
	if len(key2) != 32 {
		t.Fatalf("deriveKey(任意串) 长度 = %d, want 32", len(key2))
	}

	// ③ 64 位长度但不是合法 hex → 报错（否则会被误当成密文密钥）
	if _, err := deriveKey(strings.Repeat("z", 64)); err == nil {
		t.Fatal("deriveKey(64位非hex) error = nil, want error")
	}

	// ④ 空串 → 报错
	if _, err := deriveKey(""); err == nil {
		t.Fatal("deriveKey(\"\") error = nil, want error")
	}
}

// TestDefaultSecretKeyIsValidHex 开发默认密钥本身必须是合法 64 位 hex
func TestDefaultSecretKeyIsValidHex(t *testing.T) {
	raw, err := hex.DecodeString(DefaultSecretKey)
	if err != nil {
		t.Fatalf("DefaultSecretKey 不是合法 hex: %v", err)
	}
	if len(raw) != 32 {
		t.Fatalf("DefaultSecretKey 解码长度 = %d, want 32", len(raw))
	}
}
