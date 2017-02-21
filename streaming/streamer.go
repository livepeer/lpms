package streaming

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/codec/aacparser"
	"github.com/nareix/joy4/codec/h264parser"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
)

// The ID for a stream, consists of the concatenation of the
// NodeID and a unique ID string of the
type StreamID string

func MakeStreamID(nodeID common.Hash, id string) StreamID {
	return StreamID(fmt.Sprintf("%x%v", nodeID[:], id))
}

func (self *StreamID) String() string {
	return string(*self)
}

// Given a stream ID, return it's origin nodeID and the unique stream ID
func (self *StreamID) SplitComponents() (common.Hash, string) {
	strStreamID := string(*self)
	originComponentLength := common.HashLength * 2 // 32 bytes == 64 hexadecimal digits
	return common.HexToHash(strStreamID[:originComponentLength]), strStreamID[originComponentLength:]
}

type HlsSegment struct {
	Data []byte
	Name string
}

// A stream represents one stream
type Stream struct {
	SrcVideoChan  chan *VideoChunk
	DstVideoChan  chan *VideoChunk
	M3U8Chan      chan []byte
	HlsSegChan    chan HlsSegment
	HlsSegNameMap map[string][]byte
	M3U8          []byte
	lastDstSeq    int64
	ID            StreamID
}

func (self *Stream) PutToDstVideoChan(chunk *VideoChunk) {
	livepeerChunkInMeter.Mark(1)
	//Put to the stream
	if (chunk.HLSSegName != "") && (chunk.HLSSegData != nil) {
		//Should kick out old segments when the map reaches a certain size.
		self.HlsSegNameMap[chunk.HLSSegName] = chunk.HLSSegData
	} else if chunk.M3U8 != nil {
		self.M3U8 = chunk.M3U8
	} else {
		select {
		case self.DstVideoChan <- chunk:
			if self.lastDstSeq < chunk.Seq-1 {
				fmt.Printf("Chunk skipped at %d\n", chunk.Seq)
				livepeerChunkSkipMeter.Mark(1)
			}
			self.lastDstSeq = chunk.Seq
		default:
		}
	}
}

func (self *Stream) PutToSrcVideoChan(chunk *VideoChunk) {
	select {
	case self.SrcVideoChan <- chunk:
	default:
	}
}

func (self *Stream) GetFromDstVideoChan() *VideoChunk {
	return <-self.DstVideoChan
}

func (self *Stream) GetFromSrcVideoChan() *VideoChunk {
	return <-self.SrcVideoChan
}

// The streamer brookers the video streams
type Streamer struct {
	Streams     map[StreamID]*Stream
	SelfAddress common.Hash
}

func NewStreamer(selfAddress common.Hash) (*Streamer, error) {
	glog.V(logger.Info).Infof("Setting up new streamer with self address: %x", selfAddress[:])
	return &Streamer{
		Streams:     make(map[StreamID]*Stream),
		SelfAddress: selfAddress,
	}, nil
}

func (self *Streamer) SubscribeToStream(id string) (stream *Stream, err error) {
	streamID := StreamID(id) //MakeStreamID(nodeID, id)
	glog.V(logger.Info).Infof("Subscribing to stream with ID: %v", streamID)
	return self.saveStreamForId(streamID)
}

func (self *Streamer) AddNewStream() (stream *Stream, err error) {
	//newID := // Generate random string for the stream
	uid := randomStreamID()
	streamID := MakeStreamID(self.SelfAddress, fmt.Sprintf("%x", uid))
	glog.V(logger.Info).Infof("Adding new stream with ID: %v", streamID)
	return self.saveStreamForId(streamID)
}

func (self *Streamer) saveStreamForId(streamID StreamID) (stream *Stream, err error) {
	if self.Streams[streamID] != nil {
		return nil, errors.New("Stream with this ID already exists")
	}

	self.Streams[streamID] = &Stream{
		SrcVideoChan:  make(chan *VideoChunk, 10),
		DstVideoChan:  make(chan *VideoChunk, 10),
		M3U8Chan:      make(chan []byte),
		HlsSegChan:    make(chan HlsSegment),
		HlsSegNameMap: make(map[string][]byte),
		ID:            streamID,
	}

	return self.Streams[streamID], nil
}

func (self *Streamer) GetStream(nodeID common.Hash, id string) (stream *Stream, err error) {
	// TODO, return error if it doesn't exist
	return self.Streams[MakeStreamID(nodeID, id)], nil
}

func (self *Streamer) GetStreamByStreamID(streamID StreamID) (stream *Stream, err error) {
	return self.Streams[streamID], nil
}

func (self *Streamer) GetAllStreams() []StreamID {
	keys := make([]StreamID, 0, len(self.Streams))
	for k := range self.Streams {
		keys = append(keys, k)
	}
	return keys
}

func VideoChunkToByteArr(chunk VideoChunk) []byte {
	var buf bytes.Buffer
	gob.Register(VideoChunk{})
	gob.Register(h264parser.CodecData{})
	gob.Register(aacparser.CodecData{})
	enc := gob.NewEncoder(&buf)
	err := enc.Encode(chunk)
	if err != nil {
		fmt.Println("Error converting bytearr to chunk: ", err)
	}
	return buf.Bytes()
}

func ByteArrInVideoChunk(arr []byte) VideoChunk {
	var buf bytes.Buffer
	gob.Register(VideoChunk{})
	gob.Register(h264parser.CodecData{})
	gob.Register(aacparser.CodecData{})
	gob.Register(av.Packet{})

	buf.Write(arr)
	var chunk VideoChunk
	dec := gob.NewDecoder(&buf)
	err := dec.Decode(&chunk)
	if err != nil {
		fmt.Println("Error converting bytearr to chunk: ", err)
	}
	return chunk
}

func TestChunkEncoding(chunk VideoChunk) {
	bytes := VideoChunkToByteArr(chunk)
	newChunk := ByteArrInVideoChunk(bytes)
	fmt.Println("chunk: ", chunk)
	fmt.Println("newchunk: ", newChunk)
}

func randomStreamID() common.Hash {
	rand.Seed(time.Now().UnixNano())
	var x common.Hash
	for i := 0; i < len(x); i++ {
		x[i] = byte(rand.Uint32())
	}
	return x
}
