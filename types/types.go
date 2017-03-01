package types

import "time"

type HlsSegment struct {
	Data []byte
	Name string
}

type Download struct {
	URI           string
	TotalDuration time.Duration
}

type BroadcastReq struct {
	formats  []string
	bitrates []string
	codecin  string
	codecout []string
}
