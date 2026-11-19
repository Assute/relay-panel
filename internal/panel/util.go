// language: Go 1.23, file: internal/panel/util.go
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"time"
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// probeTCP 探活：3 秒内能建立 TCP 连接就算活
func probeTCP(host string, port int, d time.Duration) bool {
	if port == 0 {
		return true
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, itoa(port)), d)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func humanBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return itoa(int(v)) + " B"
	}
	div, exp := int64(unit), 0
	for n := v / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	frac := float64(v) / float64(div)
	s := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	return ftoa(frac, 2) + " " + s
}

func ftoa(f float64, prec int) string {
	neg := f < 0
	if neg {
		f = -f
	}
	// 四舍五入到 prec 位
	pow := 1.0
	for i := 0; i < prec; i++ {
		pow *= 10
	}
	v := int64(f*pow + 0.5)
	ip := v / int64(pow)
	fp := v % int64(pow)
	s := itoa(int(ip)) + "."
	fs := itoa(int(fp))
	for i := len(fs); i < prec; i++ {
		s += "0"
	}
	s += fs
	if neg {
		s = "-" + s
	}
	return s
}
