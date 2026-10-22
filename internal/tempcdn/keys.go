// Package tempcdn defines the shared contract with the independent cleanup Worker.
package tempcdn

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
)

const DataPrefix = "tmp/v1/"
const JobPrefix = "jobs/v1/"

var keyPattern = regexp.MustCompile(`^tmp/v1/([0-9]{13})/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+\.[a-zA-Z0-9]+$`)

func Expiry(key string) (int64, bool) {
	m := keyPattern.FindStringSubmatch(key)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	return n, err == nil && n > 0
}

func JobKey(key string, due int64) string {
	return fmt.Sprintf("%s%013d/%x.json", JobPrefix, due, sha256.Sum256([]byte(key)))
}
