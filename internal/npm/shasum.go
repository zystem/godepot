package npm

import (
	"crypto/sha1" // #nosec G505 -- npm names the sha1 of a tarball (dist.shasum); it is a name, not a defence: the sha512 is checked whenever the upstream names one
	"encoding/hex"
	"hash"
)

const sha1Size = sha1.Size

func sha1New() hash.Hash { return sha1.New() } // #nosec G401 -- see the import

func sha1Hex(b []byte) string {
	s := sha1.Sum(b) // #nosec G401 -- see the import
	return hex.EncodeToString(s[:])
}
