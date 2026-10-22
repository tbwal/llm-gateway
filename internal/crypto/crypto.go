// Package crypto 密钥加密层：用 AES-256-GCM 把渠道的 API Key 加密后落库
//
// 为什么需要这一层？
//   - 渠道的 api_key（sk-xxx）是上游服务商的"钱袋子"，明文落库意味着：
//     数据库一旦泄露（拖库/备份外流/测试库上传），所有上游额度全被偷。
//     加密后即使库被拖走，攻击者拿到的只是一串不可逆的密文。
//
// 为什么选 AES-GCM？
//   - AES-GCM 是"认证加密"（AEAD，Authenticated Encryption with Associated Data）：
//     不仅加密内容，还附带完整性校验。密文被篡改一个字节，解密直接报错
//     （而不是解出乱码），天然防住"改密文 / 伪造密文"这一类攻击。
//   - Go 标准库 crypto/aes + crypto/cipher 直接支持，零第三方依赖，纯 Go 可交叉编译。
//
// 参考实现：借鉴了 gpt-load（internal/platform/encryption）的 AES-GCM 思路：
//
//	随机 nonce 前置拼接 + hex 编码存储；本实现额外加了"上下文 AAD 域隔离"，
//	并内置"旧明文数据兼容"（DecryptOrRaw），适配本项目迁移期。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Service 加密服务接口
//
// 设计说明（依赖倒置）：业务层（handler）只依赖这个接口，不依赖具体实现。
// 好处：① 单元测试可注入 mock 实现；② 未来升级算法（如换成 KMS / 信封加密）
// 时，handler 一行都不用改。
type Service interface {
	// Encrypt 加密：plaintext（明文 API Key）→ 十六进制密文字符串
	Encrypt(plaintext string) (string, error)
	// Decrypt 解密：密文字符串 → 明文；数据非法或密钥不匹配时报错
	Decrypt(ciphertext string) (string, error)
	// DecryptOrRaw 解密，兼容旧明文：看着不像密文的数据原样返回
	// （迁移期兜底：老库里可能还躺着未加密的明文密钥）
	DecryptOrRaw(s string) (string, error)
}

// DefaultSecretKey 开发默认密钥（64 个 hex 字符 = 32 字节 = AES-256）
//
// ⚠️ 仅供本地开发使用！生产环境必须用环境变量 LLM_GATEWAY_SECRET_KEY 覆盖，
// 否则等于"密码是出厂默认密码"。
//
// ⚠️ 密钥即命根：换密钥 = 旧密文全部解不开。所以生产密钥要妥善保存、不能丢，
// 也不能让它在环境变量里"飘忽不定"（比如 CI 每次随机生成）。
const DefaultSecretKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// contextAAD（Additional Authenticated Data，附加认证数据）：加密时绑定的"上下文标签"
//
// 作用：密文只能在本项目"渠道 API Key"这个场景里解开。
// 即使两个系统误用了同一个密钥，彼此也解不开对方的密文——这叫"域隔离"。
//
// ⚠️ 它参与认证，变更后所有旧密文都会解密失败（效果等同换密钥），定死后不要改。
var contextAAD = []byte("llm-gateway/channel-api-key/v1")

// aesGCMService AES-GCM 加密服务实现
type aesGCMService struct {
	gcm cipher.AEAD // 底层 AEAD 实例（Go 文档明确：AEAD 并发安全，可多 goroutine 共用）
}

// NewService 创建加密服务
//
// secretKey 密钥材料的三种形态：
//   - 64 位 hex 字符 → 直接解码为 32 字节密钥（AES-256，推荐用法）
//   - 其他任意非空字符串 → 用 SHA-256 压缩成 32 字节（宽松兼容）
//   - 空字符串 → 返回错误（拒绝"没有密钥就加密"的误操作）
func NewService(secretKey string) (Service, error) {
	key, err := deriveKey(secretKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("创建 AES 密码器失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM 模式失败: %w", err)
	}
	return &aesGCMService{gcm: gcm}, nil
}

// deriveKey 把用户提供的密钥材料统一成 32 字节 AES 密钥
func deriveKey(secretKey string) ([]byte, error) {
	if secretKey == "" {
		return nil, errors.New("加密密钥不能为空：请设置环境变量 LLM_GATEWAY_SECRET_KEY（64 位 hex）")
	}
	// ① 标准用法：64 位 hex → 恰好 32 字节（AES-256）
	if len(secretKey) == 64 {
		key, err := hex.DecodeString(secretKey)
		if err != nil {
			return nil, fmt.Errorf("密钥格式错误：64 位 hex 中包含非法字符: %w", err)
		}
		return key, nil
	}
	// ② 宽松用法：任意长度密钥材料 → SHA-256 压缩成 32 字节
	//    （hex 解码失败会走到这里，所以上面错误分支不会误伤正常用途）
	sum := sha256.Sum256([]byte(secretKey))
	return sum[:], nil
}

// Encrypt 加密：随机 nonce + GCM 密封，输出 hex( nonce ‖ 密文‖tag )
//
// nonce 每次随机 → 同一明文两次加密结果完全不同（防重放、防密文模式泄露）。
// nonce 无需保密，随密文一起存，解密时原样取出即可。
func (s *aesGCMService) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, s.gcm.NonceSize()) // AES-GCM 标准 nonce = 12 字节
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成随机 nonce 失败: %w", err)
	}
	// Seal(dst, nonce, plaintext, aad)：密文+认证标签追加到 nonce 之后
	// 输出 = nonce ‖ ciphertext ‖ tag
	sealed := s.gcm.Seal(nonce, nonce, []byte(plaintext), contextAAD)
	return hex.EncodeToString(sealed), nil
}

// Decrypt 解密：拆出 nonce 和密文，用 GCM 解开并校验完整性
//
// 任何一步不合法（非 hex / 长度不够 / 密钥不匹配 / 被篡改）都会返回错误，
// 绝不返回"半解出的乱码"。
func (s *aesGCMService) Decrypt(ciphertext string) (string, error) {
	data, err := hex.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 hex: %w", err)
	}
	// 最短合法密文 = nonce(12) + tag(16) = 28 字节（内容可为空，GCM 允许空明文）
	minLen := s.gcm.NonceSize() + s.gcm.Overhead()
	if len(data) < minLen {
		return "", errors.New("密文长度非法")
	}
	nonce, sealed := data[:s.gcm.NonceSize()], data[s.gcm.NonceSize():]
	plain, err := s.gcm.Open(nil, nonce, sealed, contextAAD)
	if err != nil {
		// 这一步是 GCM 认证失败：密钥不匹配 或 密文被篡改
		return "", fmt.Errorf("解密失败（密钥不匹配或密文被篡改）: %w", err)
	}
	return string(plain), nil
}

// IsEncrypted 判断字符串是否是"本服务加密出的密文格式"
//
// 判断依据：① hex 可解码；② 长度 >= (nonce 12 + tag 16) * 2 = 56 个字符。
// 用于兼容旧明文数据：老库里存的是 sk-xxx 明文，不是合法密文 → 原样使用。
func (s *aesGCMService) IsEncrypted(candidate string) bool {
	if len(candidate) < (s.gcm.NonceSize()+s.gcm.Overhead())*2 {
		return false
	}
	_, err := hex.DecodeString(candidate)
	return err == nil
}

// DecryptOrRaw 解密；若字符串不是密文格式（旧明文数据），原样返回
func (s *aesGCMService) DecryptOrRaw(candidate string) (string, error) {
	if !s.IsEncrypted(candidate) {
		return candidate, nil // 旧明文兼容：不是本服务加密的，直接当明文用
	}
	return s.Decrypt(candidate)
}
