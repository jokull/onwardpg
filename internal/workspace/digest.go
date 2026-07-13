package workspace

import (
	"encoding/binary"
	"hash"
)

// writeDigestFrame makes concatenated content receipts unambiguous even when
// names or file bodies contain delimiter bytes.
func writeDigestFrame(destination hash.Hash, value []byte) {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(value)))
	_, _ = destination.Write(length[:n])
	_, _ = destination.Write(value)
}
