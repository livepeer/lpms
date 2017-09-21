package stream

import (
	"context"

	"github.com/ericxtang/m3u8"
	"github.com/nareix/joy4/av"
)

type VideoStream interface {
	GetStreamID() string
	GetStreamFormat() VideoFormat
	String() string
}

type HLSVideoManifest interface {
	GetManifestID() string
	GetVideoFormat() VideoFormat
	GetManifest() (*m3u8.MasterPlaylist, error)
	GetVideoStream(strmID string) (HLSVideoStream, error)
	AddVideoStream(strmID string, variant *m3u8.Variant) error
	DeleteVideoStream(strmID string) error
	String() string
}

//HLSVideoStream contains the master playlist, media playlists in it, and the segments in them.  Each media playlist also has a streamID.
//You can only add media playlists to the stream.
type HLSVideoStream interface {
	VideoStream
	GetStreamPlaylist() (*m3u8.MediaPlaylist, error)
	GetStreamVariant() *m3u8.Variant
	GetHLSSegment(segName string) (*HLSSegment, error)
	AddHLSSegment(seg *HLSSegment) error
	SetSubscriber(f func(seg *HLSSegment, eof bool))
	End()
}

type RTMPVideoStream interface {
	VideoStream
	ReadRTMPFromStream(ctx context.Context, dst av.MuxCloser) error
	WriteRTMPToStream(ctx context.Context, src av.DemuxCloser) error
}
