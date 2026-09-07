package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode"
)

func New(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%x_%s", prefix, time.Now().UTC().UnixMilli(), hex.EncodeToString(b[:]))
}

func Short(value string) string {
	if i := strings.LastIndexByte(value, '_'); i >= 0 && len(value)-i > 9 {
		return value[i+1 : i+9]
	}
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

func Slug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	dash := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			dash = false
			continue
		}
		if !dash && out.Len() > 0 {
			out.WriteByte('-')
			dash = true
		}
	}
	slug := strings.Trim(out.String(), "-")
	if slug == "" {
		slug = "work"
	}
	if len(slug) > 40 {
		slug = strings.TrimRight(slug[:40], "-")
	}
	return slug
}
