package store

import (
	"errors"
	"strconv"
	"strings"
)

func archiveKeyIdentity(key string) (int64, string, error) {
	parts := strings.Split(key, "/")
	if len(parts) >= 6 && parts[0] == "accounts" {
		parts = parts[2:]
	}
	if len(parts) < 4 || parts[0] != "albums" || parts[2] == "" {
		return 0, "", errors.New("归档对象缺少相册定位")
	}
	album, err := strconv.ParseInt(parts[1], 10, 64)
	return album, parts[2], err
}
