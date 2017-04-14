package stream

import (
	"context"
	"errors"
	"io"
	"reflect"
	"runtime/debug"

	"time"

	"github.com/golang/glog"
	"github.com/kz26/m3u8"
	"github.com/nareix/joy4/av"
)

var ErrBufferFull = errors.New("Stream Buffer Full")
var ErrBufferEmpty = errors.New("Stream Buffer Empty")
var ErrBufferItemType = errors.New("Buffer Item Type Not Recognized")
var ErrDroppedRTMPStream = errors.New("RTMP Stream Stopped Without EOF")
var ErrHttpReqFailed = errors.New("Http Request Failed")

type VideoFormat uint32

var (
	HLS  = MakeVideoFormatType(avFormatTypeMagic + 1)
	RTMP = MakeVideoFormatType(avFormatTypeMagic + 2)
)

func MakeVideoFormatType(base uint32) (c VideoFormat) {
	c = VideoFormat(base) << videoFormatOtherBits
	return
}

const avFormatTypeMagic = 577777
const videoFormatOtherBits = 1

type RTMPEOF struct{}

type streamBuffer struct {
	q *Queue
}

func newStreamBuffer() *streamBuffer {
	return &streamBuffer{q: NewQueue(1000)}
}

func (b *streamBuffer) push(in interface{}) error {
	b.q.Put(in)
	return nil
}

func (b *streamBuffer) poll(wait time.Duration) (interface{}, error) {
	results, err := b.q.Poll(1, wait)
	if err != nil {
		return nil, err
	}
	result := results[0]
	return result, nil
}

func (b *streamBuffer) pop() (interface{}, error) {
	results, err := b.q.Get(1)
	if err != nil {
		return nil, err
	}
	result := results[0]
	return result, nil
}

func (b *streamBuffer) len() int64 {
	return b.q.Len()
}

type HLSSegment struct {
	Name string
	Data []byte
}

// type ChannelStream interface {
// 	RTMPPackets() <-chan av.Packet
// 	RTMPHeader() []av.CodecData
// 	HLSPlaylists() <-chan m3u8.MediaPlaylist
// 	HLSSegments() <-chan HLSSegment
// 	// ConsumeRTMP(headers []av.CodecData, pktChan <-chan av.Packet)
// 	ConsumeRTMP(ChannelRTMPDemuxer)
// 	ConsumeHLS(plChan <-chan m3u8.MediaPlaylist, segChan <-chan HLSSegment)
// }

// type CS struct {
// 	m av.Muxer
// }

// func (s *CS) ConsumeRTMP(d ChannelRTMPDemuxer) {
// 	s.headers = d.Streams()
// 	pktChan, errChan = d.ReadPackets()
// 	for {
// 		select {
// 		case pkt := <-pktChan:
// 			s.m.WritePacket(pkt)
// 		}
// 	}
// }

// type ChannelRTMPMuxer interface {
// 	WriteHeader(header []av.CodecData) error
// 	WritePackets(ctx context.Context, pkt <-chan av.Packet) error
// 	WriteTrailer() error
// }

// type ChannelRTMPDemuxer interface {
// 	Streams() []av.CodecData
// 	ReadPackets() (chan<- av.Packet, chan<- error)
// }

type Stream interface {
	GetStreamID() string
	Len() int64
	// NewStream() Stream
	ReadRTMPFromStream(ctx context.Context, dst av.MuxCloser) error
	WriteRTMPToStream(ctx context.Context, src av.DemuxCloser) error
	WriteHLSPlaylistToStream(pl m3u8.MediaPlaylist) error
	WriteHLSSegmentToStream(seg HLSSegment) error
	ReadHLSFromStream(ctx context.Context, buffer HLSMuxer) error
}

type VideoStream struct {
	StreamID    string
	RTMPTimeout time.Duration
	HLSTimeout  time.Duration
	buffer      *streamBuffer
}

func (s *VideoStream) Len() int64 {
	return s.buffer.len()
}

func NewVideoStream(id string) *VideoStream {
	return &VideoStream{buffer: newStreamBuffer(), StreamID: id}
}

func (s *VideoStream) GetStreamID() string {
	return s.StreamID
}

//ReadRTMPFromStream reads the content from the RTMP stream out into the dst.
func (s *VideoStream) ReadRTMPFromStream(ctx context.Context, dst av.MuxCloser) error {
	defer dst.Close()

	//TODO: Make sure to listen to ctx.Done()
	for {
		item, err := s.buffer.poll(s.RTMPTimeout)
		if err != nil {
			return err
		}

		switch item.(type) {
		case []av.CodecData:
			headers := item.([]av.CodecData)
			err = dst.WriteHeader(headers)
			if err != nil {
				glog.Infof("Error writing RTMP header from Stream %v to mux", s.StreamID)
				return err
			}
		case av.Packet:
			packet := item.(av.Packet)
			err = dst.WritePacket(packet)
			if err != nil {
				glog.Infof("Error writing RTMP packet from Stream %v to mux: %v", s.StreamID, err)
				return err
			}
		case RTMPEOF:
			err := dst.WriteTrailer()
			if err != nil {
				glog.Infof("Error writing RTMP trailer from Stream %v", s.StreamID)
				return err
			}
			return io.EOF
		default:
			glog.Infof("Cannot recognize buffer iteam type: ", reflect.TypeOf(item))
			debug.PrintStack()
			return ErrBufferItemType
		}
	}
}

func (s *VideoStream) WriteRTMPHeader(h []av.CodecData) {
	s.buffer.push(h)
}

func (s *VideoStream) WriteRTMPPacket(p av.Packet) {
	s.buffer.push(p)
}

//WriteRTMPToStream writes a video stream from src into the stream.
func (s *VideoStream) WriteRTMPToStream(ctx context.Context, src av.DemuxCloser) error {
	defer src.Close()

	c := make(chan error, 1)
	go func() {
		c <- func() error {
			header, err := src.Streams()
			if err != nil {
				return err
			}
			err = s.buffer.push(header)
			if err != nil {
				return err
			}

			// var lastKeyframe av.Packet
			for {
				packet, err := src.ReadPacket()
				if err == io.EOF {
					s.buffer.push(RTMPEOF{})
					return err
				} else if err != nil {
					return err
				} else if len(packet.Data) == 0 { //TODO: Investigate if it's possible for packet to be nil (what happens when RTMP stopped publishing because of a dropped connection? Is it possible to have err and packet both nil?)
					return ErrDroppedRTMPStream
				}

				if packet.IsKeyFrame {
					// lastKeyframe = packet
				}

				err = s.buffer.push(packet)
				if err == ErrBufferFull {
					//TODO: Delete all packets until last keyframe, insert headers in front - trying to get rid of streaming artifacts.
				}
			}
		}()
	}()

	select {
	case <-ctx.Done():
		glog.Infof("Finished writing RTMP to Stream %v", s.StreamID)
		return ctx.Err()
	case err := <-c:
		return err
	}
}

func (s *VideoStream) WriteHLSPlaylistToStream(pl m3u8.MediaPlaylist) error {
	return s.buffer.push(pl)
}

func (s *VideoStream) WriteHLSSegmentToStream(seg HLSSegment) error {
	return s.buffer.push(seg)
}

//ReadHLSFromStream reads an HLS stream into an HLSBuffer
func (s *VideoStream) ReadHLSFromStream(ctx context.Context, mux HLSMuxer) error {
	for {
		// fmt.Printf("Buffer len: %v\n", s.buffer.len())
		item, err := s.buffer.poll(s.HLSTimeout)
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		switch item.(type) {
		case m3u8.MediaPlaylist:
			mux.WritePlaylist(item.(m3u8.MediaPlaylist))
		case HLSSegment:
			mux.WriteSegment(item.(HLSSegment).Name, item.(HLSSegment).Data)
		default:
			return ErrBufferItemType
		}
	}
}
