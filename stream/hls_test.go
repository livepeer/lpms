package stream

import (
	"context"
	"fmt"
	"testing"
)

func TestHLSBufferGeneratePlaylist(t *testing.T) {
	b := NewHLSBuffer(5)
	b.WriteSegment("s_9.ts", nil)
	b.WriteSegment("s_10.ts", nil)
	b.WriteSegment("s_11.ts", nil)
	b.WriteSegment("s_13.ts", nil)
	b.WriteSegment("s_12.ts", nil)
	pl, err := b.GeneratePlaylist()

	if err != nil {
		t.Errorf("Got error %v when generating playlist", err)
	}

	if len(pl.Segments) != 5 {
		t.Errorf("Expecting 3 segments, got %v", len(pl.Segments))
	}

	for i := 0; i < 5; i++ {
		if pl.Segments[i].URI != fmt.Sprintf("s_%v.ts", i+9) {
			t.Errorf("Unexpected order: %v, %v", pl.Segments[i].URI, fmt.Sprintf("s_%v.ts", i+9))
		}
	}
}

func TestHLSPushPop(t *testing.T) {
	b := NewHLSBuffer(5)
	//Insert 6 segments - 1 should be evicted
	b.WriteSegment("s_9.ts", []byte{0})
	b.WriteSegment("s_10.ts", []byte{0})
	b.WriteSegment("s_11.ts", []byte{0})
	b.WriteSegment("s_12.ts", []byte{0})
	b.WriteSegment("s_13.ts", []byte{0})
	b.WriteSegment("s_14.ts", []byte{0})

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("s_%v.ts", i+10) //Should start from 10
		seg, err := b.WaitAndPopSegment(ctx, name)
		if err != nil {
			t.Errorf("Error retrieving segment")
		}
		if seg == nil {
			t.Errorf("Segment is nil, expecting a non-nil segment")
		}
	}

	segLen := len(b.segCache)
	if segLen != 0 {
		t.Errorf("Expecting length of buffer to be 0, got %v", segLen)
	}

	segLen = len(b.sq.Keys())
	if segLen != 0 {
		t.Errorf("Expecting length of buffer to be 0, got %v", segLen)
	}

}
