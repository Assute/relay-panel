// language: Go 1.23, file: internal/panel/auth.go
// 管理员登录：PBKDF2-HMAC-SHA256（标准库实现，无第三方依赖）+ 内存 session。
package panel

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"sync"
	"time"
)

const pbkdf2Iter = 120_000

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(s) }
func hexEncode(b []byte) string          { return hex.EncodeToString(b) }

// HashPassword 返回 "salt$hash" 形式（都是 hex）
func HashPassword(pw string) string {
	salt := randHex(16)
	sb, _ := hexDecode(salt)
	h := pbkdf2SHA256([]byte(pw), sb, pbkdf2Iter, 32)
	return salt + "$" + hexEncode(h)
}

func VerifyPassword(pw, stored string) bool {
	for i := 0; i < len(stored); i++ {
		if stored[i] == '$' {
			salt, _ := hexDecode(stored[:i])
			want, err := hexDecode(stored[i+1:])
			if err != nil {
				return false
			}
			got := pbkdf2SHA256([]byte(pw), salt, pbkdf2Iter, len(want))
			return subtle.ConstantTimeCompare(got, want) == 1
		}
	}
	return false
}

// ---------- session ----------

type session struct {
	user string
	exp  time.Time
}

type SessionStore struct {
	mu sync.Mutex
	m  map[string]session
}

func NewSessionStore() *SessionStore {
	s := &SessionStore{m: map[string]session{}}
	go func() {
		for range time.Tick(10 * time.Minute) {
			now := time.Now()
			s.mu.Lock()
			for k, v := range s.m {
				if now.After(v.exp) {
					delete(s.m, k)
				}
			}
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *SessionStore) Issue(user string) string {
	tok := randHex(24)
	s.mu.Lock()
	s.m[tok] = session{user: user, exp: time.Now().Add(12 * time.Hour)}
	s.mu.Unlock()
	return tok
}

func (s *SessionStore) Get(tok string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[tok]
	if !ok || time.Now().After(v.exp) {
		return "", false
	}
	return v.user, true
}

func (s *SessionStore) Revoke(tok string) {
	s.mu.Lock()
	delete(s.m, tok)
	s.mu.Unlock()
}
