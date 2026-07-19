package stashgram

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"os"

	"golang.org/x/crypto/pbkdf2"
)

// SessionCrypto handles encryption/decryption with a password, using
// AES-256-GCM with a PBKDF2-derived key. Encrypt/Decrypt (base64) are used
// for session-string-style data; EncryptBytes/DecryptBytes (raw bytes) are
// used for per-chunk file encryption, where base64's ~33% overhead on top of
// an already-uploaded file isn't worth paying.
type SessionCrypto struct {
	password string
}

// NewSessionCrypto creates a new crypto helper for the given password.
func NewSessionCrypto(password string) *SessionCrypto {
	return &SessionCrypto{password: password}
}

// EncryptBytes encrypts raw bytes and returns raw ciphertext as
// salt(16) || nonce(12) || ciphertext+tag — no text encoding.
func (c *SessionCrypto) EncryptBytes(plaintext []byte) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	key := pbkdf2.Key([]byte(c.password), salt, 100000, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	combined := make([]byte, 0, len(salt)+len(nonce)+len(ciphertext))
	combined = append(combined, salt...)
	combined = append(combined, nonce...)
	combined = append(combined, ciphertext...)
	return combined, nil
}

// DecryptBytes reverses EncryptBytes.
func (c *SessionCrypto) DecryptBytes(combined []byte) ([]byte, error) {
	if len(combined) < 16+12 {
		return nil, errors.New("ciphertext too short")
	}
	salt := combined[:16]
	nonce := combined[16:28]
	ciphertext := combined[28:]

	key := pbkdf2.Key([]byte(c.password), salt, 100000, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// Encrypt takes raw bytes and returns a base64-encoded ciphertext.
func (c *SessionCrypto) Encrypt(plaintext []byte) (string, error) {
	combined, err := c.EncryptBytes(plaintext)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(combined), nil
}

// Decrypt takes a base64-encoded ciphertext and returns the original bytes.
func (c *SessionCrypto) Decrypt(encoded string) ([]byte, error) {
	combined, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return c.DecryptBytes(combined)
}

// HashFile computes the SHA-256 hash of a file using streaming reads.
// Memory usage is kept low (64KB buffer).
func HashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()
	buf := make([]byte, 64*1024)

	for {
		n, err := file.Read(buf)
		if n > 0 {
			if _, writeErr := hasher.Write(buf[:n]); writeErr != nil {
				return "", writeErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// HashFile is kept as a method too, for existing callers of *SessionCrypto.
func (c *SessionCrypto) HashFile(path string) (string, error) {
	return HashFile(path)
}

// ---------------------------------------------------------------------------
// Random helpers used for obfuscation: random per-chunk passwords, and
// random Telegram-facing filenames. (RandomInt64 is kept for general use
// even though the old random-chunk-size feature that used it has been
// removed — see uploadLoop/planChunks in filesystem.go.)
// ---------------------------------------------------------------------------

// RandomHex returns a cryptographically random hex string from n random
// bytes (2n hex characters). Used for obfuscated chunk filenames.
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// RandomPassword generates a cryptographically random 256-bit per-chunk
// password, hex-encoded.
func RandomPassword() (string, error) {
	return RandomHex(32)
}

// RandomInt64 returns a cryptographically random int64 in [min, max]
// inclusive.
func RandomInt64(min, max int64) (int64, error) {
	if max <= min {
		return min, nil
	}
	span := big.NewInt(max - min + 1)
	n, err := rand.Int(rand.Reader, span)
	if err != nil {
		return 0, err
	}
	return min + n.Int64(), nil
}
