package stream

import (
	"errors"

	"github.com/ericxtang/m3u8"
	"github.com/golang/glog"
)

var ErrVideoManifest = errors.New("ErrVideoManifest")

type BasicHLSVideoManifest struct {
	streamMap     map[string]HLSVideoStream
	manifestCache *m3u8.MasterPlaylist
	id            string
	winSize       uint
}

func NewBasicHLSVideoManifest(id string, wSize uint) *BasicHLSVideoManifest {
	pl := m3u8.NewMasterPlaylist()
	return &BasicHLSVideoManifest{
		streamMap:     make(map[string]HLSVideoStream),
		manifestCache: pl,
		id:            id,
		winSize:       wSize,
	}
}

func (m *BasicHLSVideoManifest) GetManifestID() string { return m.id }

func (m *BasicHLSVideoManifest) GetVideoFormat() VideoFormat { return HLS }

func (m *BasicHLSVideoManifest) GetManifest() (*m3u8.MasterPlaylist, error) {
	return m.manifestCache, nil
}

func (m *BasicHLSVideoManifest) GetVideoStream(strmID string) (HLSVideoStream, error) {
	strm, ok := m.streamMap[strmID]
	if !ok {
		return nil, ErrNotFound
	}
	return strm, nil
}

func (m *BasicHLSVideoManifest) AddVideoStream(strmID string, variant *m3u8.Variant) (*BasicHLSVideoStream, error) {
	_, ok := m.streamMap[strmID]
	if ok {
		return nil, ErrVideoManifest
	}

	//Check if the same Bandwidth & Resolution already exists
	for _, strm := range m.streamMap {
		v := strm.GetStreamVariant()
		if v.Bandwidth == variant.Bandwidth && v.Resolution == variant.Resolution {
			glog.Errorf("Variant with Bandwidth %v and Resolution %v already exists", v.Bandwidth, v.Resolution)
			return nil, ErrVideoManifest
		}
	}

	//Add to the map
	m.manifestCache.Append(variant.URI, variant.Chunklist, variant.VariantParams)
	strm := NewBasicHLSVideoStream(strmID, variant, m.winSize)
	m.streamMap[strmID] = strm
	return strm, nil
}

func (m *BasicHLSVideoManifest) DeleteVideoStream(strmID string) error {
	delete(m.streamMap, strmID)
	return nil
}

func (m *BasicHLSVideoManifest) String() string { return "" }
