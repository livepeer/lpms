package streaming

import (
	"github.com/ethereum/go-ethereum/swarm/storage"
	"github.com/nareix/joy4/av"
)

const (
	RequestStreamMsgID = iota
	DeliverStreamMsgID
	EOFStreamMsgID
)

// VideoChunk is an encapsulation for video packets / headers.
// It is used to pass video data around using the streamer
// for now, Id=100 means it's a request, Id=200 means it's a data chunk, Id=300 means it's EOF (end of stream)
type VideoChunk struct {
	ID            int64
	Seq           int64
	Key           storage.Key
	HeaderStreams []av.CodecData
	Packet        av.Packet
	HLSSegData    []byte
	HLSSegName    string
	M3U8          []byte
}
