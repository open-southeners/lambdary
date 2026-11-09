package queue

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

// md5Hex is the MD5OfMessageBody SDKs check a sent or received body against.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Transport type bytes in SQS's message attribute digest.
const (
	transportString = 1
	transportBinary = 2
)

// attributesMD5 computes MD5OfMessageAttributes as SQS does: attributes sorted
// by name, each encoded as its length-prefixed name and data type, a
// transport type byte, then its length-prefixed value. "" when there are no
// attributes, since SQS then omits the digest.
func attributesMD5(attrs map[string]Attribute) string {
	if len(attrs) == 0 {
		return ""
	}

	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	h := md5.New()

	writeBytes := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		h.Write(n[:])
		h.Write(b)
	}

	for _, name := range names {
		a := attrs[name]

		writeBytes([]byte(name))
		writeBytes([]byte(a.DataType))

		if a.StringValue != nil && !strings.HasPrefix(a.DataType, "Binary") {
			h.Write([]byte{transportString})
			writeBytes([]byte(*a.StringValue))
		} else {
			h.Write([]byte{transportBinary})
			writeBytes(a.BinaryValue)
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}
