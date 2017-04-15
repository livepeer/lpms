package stream

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kz26/m3u8"
)

var ErrNotFound = errors.New("Not Found")

type HLSDemuxer interface {
	//This method should ONLY push a playlist onto a chan when it's a NEW playlist
	PollPlaylist(ctx context.Context) (m3u8.MediaPlaylist, error)
	//This method should ONLY push a segment onto a chan when it's a NEW segment
	WaitAndPopSegment(ctx context.Context, name string) ([]byte, error)
}

type HLSMuxer interface {
	WritePlaylist(m3u8.MediaPlaylist) error
	WriteSegment(name string, s []byte) error
}

//TODO: Write tests, set buffer size, kick out segments / playlists if too full
type HLSBuffer struct {
	HoldTime   time.Duration
	plCacheNew bool
	segCache   *Queue
	// pq       *Queue
	plCache m3u8.MediaPlaylist
	sq      *ConcurrentMap
	lock    sync.Locker
}

func NewHLSBuffer() *HLSBuffer {
	m := NewCMap()
	return &HLSBuffer{plCacheNew: false, segCache: &Queue{}, HoldTime: time.Second, sq: &m, lock: &sync.Mutex{}}
}

func (b *HLSBuffer) WritePlaylist(p m3u8.MediaPlaylist) error {
	// fmt.Println("Writing playlist")
	b.lock.Lock()
	b.plCache = p
	b.plCacheNew = true
	b.lock.Unlock()
	return nil
}

func (b *HLSBuffer) WriteSegment(name string, s []byte) error {
	b.lock.Lock()
	b.segCache.Put(name)
	b.sq.Set(name, s)
	b.lock.Unlock()
	return nil
}

func (b *HLSBuffer) WaitAndPopPlaylist(ctx context.Context) (m3u8.MediaPlaylist, error) {
	for {
		b.lock.Lock()
		if b.plCacheNew {
			defer b.lock.Unlock()

			b.plCacheNew = false
			return b.plCache, nil
		}
		b.lock.Unlock()

		time.Sleep(time.Second * 1)
		select {
		case <-ctx.Done():
			return m3u8.MediaPlaylist{}, ctx.Err()
		default:
			//Fall through here so we can loop back
		}
	}
}

func (b *HLSBuffer) WaitAndPopSegment(ctx context.Context, name string) ([]byte, error) {
	for {
		// fmt.Printf("HLSBuffer %v: segment keys: %v.  Current name: %v\n", &b, b.sq.Keys(), name)
		seg, found := b.sq.Get(name)
		// glog.Infof("GetSegment: %v, %v", name, found)
		if found {
			b.sq.Remove(name)
			return seg.([]byte), nil
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
