// Contains the metrics collected for LivePeer

package streaming

import (
	"github.com/ethereum/go-ethereum/metrics"
)

var (
	livepeerChunkSkipMeter   = metrics.NewMeter("livepeer/chunks/skip")
	livepeerChunkInMeter     = metrics.NewMeter("livepeer/chunks/in")
	livepeerChunkBufferTimer = metrics.NewMeter("livepeer/chunks/buffer")

	livepeerPacketSkipMeter   = metrics.NewMeter("livepeer/packets/skip")
	livepeerPacketBufferTimer = metrics.NewTimer("livepeer/packets/buffer")
	livepeerPacketReqTimer    = metrics.NewTimer("livepeer/packets/req")
	livepeerPacketInMeter     = metrics.NewMeter("livepeer/packets/in")

	livepeerStreamReqMeter     = metrics.NewMeter("livepeer/streams/req")
	livepeerStreamTimeoutMeter = metrics.NewMeter("livepeer/streams/timeout")
	// How do we keep track of the video length for EACH stream? (Can't create a new timer for every new stream)
	livepeerStreamLengthTimer = metrics.NewMeter("livepeer/streams/length") // This is the TOTAL length of ALL the videos

)
