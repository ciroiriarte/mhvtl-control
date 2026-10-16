package conffile

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

// Hash is the hex SHA-256 of data: the base hash an edit is made against.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ReadFile returns a file's bytes and the hash of exactly those bytes, so the
// hash always describes what was parsed.
func ReadFile(path string) (data []byte, sha string, err error) {
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	return data, Hash(data), nil
}
