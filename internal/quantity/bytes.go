// Package quantity keeps byte amounts exact across SQLite, Go and browsers.
package quantity

import (
	"errors"
	"math"
	"strconv"
)

type Bytes int64

var ErrRange = errors.New("字节数须为 0..9223372036854775807 的规范十进制字符串")

func Parse(s string) (Bytes, error) {
	if s == "" || len(s) > 19 || len(s) > 1 && s[0] == '0' {
		return 0, ErrRange
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, ErrRange
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, ErrRange
	}
	return Bytes(n), nil
}

func (b Bytes) String() string { return strconv.FormatInt(int64(b), 10) }
func (b Bytes) MarshalJSON() ([]byte, error) {
	if b < 0 {
		return nil, ErrRange
	}
	return []byte(`"` + b.String() + `"`), nil
}
func (b *Bytes) UnmarshalJSON(data []byte) error {
	// No escaped digits, JSON numbers or null; optional quotas use *Bytes.
	if len(data) < 3 || data[0] != '"' || data[len(data)-1] != '"' {
		return ErrRange
	}
	n, err := Parse(string(data[1 : len(data)-1]))
	if err == nil {
		*b = n
	}
	return err
}
func Add(a, b Bytes) (Bytes, error) {
	if a < 0 || b < 0 || int64(a) > math.MaxInt64-int64(b) {
		return 0, ErrRange
	}
	return a + b, nil
}
