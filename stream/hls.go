package stream

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/kz26/m3u8"
)

var ErrNotFound = errors.New("Not Found")
var ErrBadHLSBuffer = errors.New("BadHLSBuffer")

type HLSDemuxer interface {
	//This method should ONLY push a playlist onto a chan when it's a NEW playlist
	PollPlaylist(ctx context.Context) (m3u8.MediaPlaylist, error)
	//This method should ONLY push a segment onto a chan when it's a NEW segment
	WaitAndPopSegment(ctx context.Context, name string) ([]byte, error)
	WaitAndGetSegment(ctx context.Context, name string) ([]byte, error)
}

type HLSMuxer interface {
	WritePlaylist(m3u8.MediaPlaylist) error
	WriteSegment(name string, s []byte) error
}

//TODO: Write tests, set buffer size, kick out segments / playlists if too full
type HLSBuffer struct {
	plCacheNew bool
	segCache   []string
	// pq       *Queue
	plCache  m3u8.MediaPlaylist
	sq       *ConcurrentMap
	lock     sync.Locker
	Capacity uint
}

func NewHLSBuffer(segCap uint) *HLSBuffer {
	m := NewCMap()
	// return &HLSBuffer{plCacheNew: false, segCache: &Queue{}, HoldTime: time.Second, sq: &m, lock: &sync.Mutex{}}
	return &HLSBuffer{plCacheNew: false, segCache: make([]string, 0, segCap), sq: &m, lock: &sync.Mutex{}, Capacity: segCap}
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
	if len(b.segCache) == cap(b.segCache) { //Evict the oldest segment
		b.sq.Remove(b.segCache[0])
		b.segCache = b.segCache[1:]
	}
	b.segCache = append(b.segCache, name)
	b.sq.Set(name, s)
	// ks := ""
	// for _, k := range b.sq.Keys() {
	// 	ks += ", " + strings.Split(k, "_")[1]
	// }
	// glog.Infof("Writing seg %v (%v), now sq: %v", strings.Split(name, "_")[1], len(s), ks)
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

func (b *HLSBuffer) LatestPlaylist() (m3u8.MediaPlaylist, error) {
	return b.plCache, nil
}

func (b *HLSBuffer) GeneratePlaylist() (m3u8.MediaPlaylist, error) {
	if len(b.segCache) == 0 {
		return m3u8.MediaPlaylist{}, ErrBufferEmpty
	}

	pl, _ := m3u8.NewMediaPlaylist(uint(len(b.segCache)), uint(len(b.segCache)))
	// glog.Infof("Generating Playlist: %v", b.sq.Keys())
	ks := b.segCache

	sort.Slice(ks, func(i, j int) bool {
		ii, _ := strconv.Atoi(strings.Split(strings.Split(ks[i], "_")[1], ".")[0])
		ji, _ := strconv.Atoi(strings.Split(strings.Split(ks[j], "_")[1], ".")[0])
		return ii < ji
	})

	for _, k := range ks {
		pl.Append(k, 2, "")
	}

	return *pl, nil
}

func (b *HLSBuffer) WaitAndPopSegment(ctx context.Context, name string) ([]byte, error) {
	bt, e := b.WaitAndGetSegment(ctx, name)
	if bt != nil {
		idx := -1
		for i := 0; i < len(b.segCache); i++ {
			if b.segCache[i] == name {
				idx = i
				break
			}
		}
		if idx == -1 {
			glog.Errorf("Can't find %v in cache", name)
			return nil, ErrBadHLSBuffer
		}
		b.segCache = append(b.segCache[:idx], b.segCache[idx+1:]...)
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
