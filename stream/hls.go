package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ericxtang/m3u8"
)

var ErrNotFound = errors.New("Not Found")
var ErrBadHLSBuffer = errors.New("BadHLSBuffer")

type HLSDemuxer interface {
	PollPlaylist(ctx context.Context) (m3u8.MediaPlaylist, error)
	WaitAndPopSegment(ctx context.Context, name string) ([]byte, error)
	WaitAndGetSegment(ctx context.Context, name string) ([]byte, error)
}

type HLSMuxer interface {
	WriteSegment(seqNo uint64, name string, duration float64, s []byte) error
}

//TODO: Write tests, set buffer size, kick out segments / playlists if too full
type HLSBuffer struct {
	plCache  *m3u8.MediaPlaylist
	sq       *ConcurrentMap
	lock     sync.Locker
	Capacity uint
}

func NewHLSBuffer(winSize, segCap uint) *HLSBuffer {
	m := NewCMap()
	// return &HLSBuffer{plCacheNew: false, segCache: &Queue{}, HoldTime: time.Second, sq: &m, lock: &sync.Mutex{}}
	pl, _ := m3u8.NewMediaPlaylist(winSize, segCap)
	return &HLSBuffer{plCache: pl, sq: &m, lock: &sync.Mutex{}, Capacity: segCap}
}

func (b *HLSBuffer) WriteSegment(seqNo uint64, name string, duration float64, s []byte) error {
	b.lock.Lock()

	b.sq.Set(name, &HLSSegment{SeqNo: seqNo, Name: name, Duration: duration, Data: s})
	err := b.plCache.InsertSegment(seqNo, &m3u8.MediaSegment{SeqId: seqNo, Duration: duration, URI: name})
	if err != nil {
		return err
	}

	// if b.plCache.Count() > b.plCache.WinSize() { //Evit oldest segment
	// 	toRm := b.plCache.Segments[b.plCache.Count()-b.plCache.WinSize()-1]
	// 	// fmt.Println("Evicting %v", toRm)
	// 	b.sq.Remove(toRm.URI)
	// }

	b.lock.Unlock()
	return nil
}

func (b *HLSBuffer) LatestPlaylist() (*m3u8.MediaPlaylist, error) {
	return b.plCache, nil
}

func (b *HLSBuffer) WaitAndPopSegment(ctx context.Context, name string) ([]byte, error) {
	bt, e := b.WaitAndGetSegment(ctx, name)
	if bt != nil {
		b.sq.Remove(name)
	}
	return bt, e
}

func (b *HLSBuffer) WaitAndGetSegment(ctx context.Context, name string) ([]byte, error) {
	for {
		// fmt.Printf("HLSBuffer %v: segment keys: %v.  Current name: %v\n", &b, b.sq.Keys(), name)
		seg, found := b.sq.Get(name)
		// glog.Infof("GetSegment: %v, %v", name, found)
		if found {
			return seg.(*HLSSegment).Data, nil
			// return seg.([]byte), nil
		}

		time.Sleep(time.Second * 1)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			//Fall through here so we can loop back
		}
	}
}
