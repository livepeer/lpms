package stream

import (
	"context"
	"errors"
	"io"
	"reflect"

	"sync"

	"github.com/golang/glog"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/pubsub"
)

var ErrWrongFormat = errors.New("WrongVideoFormat")
var ErrStreamSubscriber = errors.New("StreamSubscriberError")

type StreamSubscriber struct {
	stream          Stream
	lock            sync.Mutex
	rtmpSubscribers map[string]av.Muxer
	rtmpHeader      []av.CodecData
	hlsSubscribers  map[string]HLSMuxer
}

func NewStreamSubscriber(s Stream) *StreamSubscriber {
	return &StreamSubscriber{stream: s, rtmpSubscribers: make(map[string]av.Muxer), hlsSubscribers: make(map[string]HLSMuxer)}
}

func (s *StreamSubscriber) SubscribeRTMP(muxID string, mux av.Muxer) error {
	if len(s.hlsSubscribers) != 0 {
		glog.Errorf("Cannot add RTMP subscriber.  Already have HLS subscribers.")
		return ErrWrongFormat
	}

	if s.rtmpHeader != nil {
		mux.WriteHeader(s.rtmpHeader)
	}

	s.lock.Lock()
	s.rtmpSubscribers[muxID] = mux
	s.lock.Unlock()
	// glog.Infof("subscriber length: %v", len(s.rtmpSubscribers))
	return nil
}

func (s *StreamSubscriber) UnsubscribeRTMP(muxID string) error {
	if s.rtmpSubscribers[muxID] == nil {
		return ErrNotFound
	}
	delete(s.rtmpSubscribers, muxID)
	return nil
}

func (s *StreamSubscriber) HasSubscribers() bool {
	rs := len(s.rtmpSubscribers)
	hs := len(s.hlsSubscribers)

	return rs+hs > 0
}

func (s *StreamSubscriber) StartRTMPWorker(ctx context.Context) error {
	// glog.Infof("Starting RTMP worker")
	q := pubsub.NewQueue()
	go s.stream.ReadRTMPFromStream(ctx, q)

	m := q.Oldest()
	// glog.Infof("Waiting for rtmp header in worker")
	headers, _ := m.Streams()
	// glog.Infof("StartRTMPWorker: rtmp headers: %v", headers)
	s.rtmpHeader = headers
	for _, rtmpMux := range s.rtmpSubscribers {
		rtmpMux.WriteHeader(headers)
	}

	for {
		pkt, err := m.ReadPacket()

		// glog.Infof("Writing packet %v", pkt.Data)
		if err != nil {
			if err == io.EOF {
				// glog.Info("Got EOF, stopping RTMP subscribers now.")
				for _, rtmpMux := range s.rtmpSubscribers {
					rtmpMux.WriteTrailer()
				}
				return err
			}
			glog.Errorf("Error while reading RTMP in subscriber worker: %v", err)
			return err
		}

		// glog.Infof("subsciber len: %v", len(s.rtmpSubscribers))
		for _, rtmpMux := range s.rtmpSubscribers {
			rtmpMux.WritePacket(pkt)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func (s *StreamSubscriber) SubscribeHLS(muxID string, mux HLSMuxer) error {
	if len(s.rtmpSubscribers) != 0 {
		glog.Errorf("Cannot add HLS subscriber.  Already have RTMP subscribers.")
		return ErrWrongFormat
	}

	// fmt.Println("adding mux to subscribers")
	if s.hlsSubscribers[muxID] != nil {
		glog.Errorf("Subscription already exists for %v: %v", muxID, reflect.TypeOf(s.hlsSubscribers))
		return ErrStreamSubscriber
	}

	s.hlsSubscribers[muxID] = mux
	return nil
}

func (s *StreamSubscriber) UnsubscribeHLS(muxID string) error {
	if s.hlsSubscribers[muxID] == nil {
		return ErrNotFound
	}

	delete(s.hlsSubscribers, muxID)
	return nil
}

func (s *StreamSubscriber) StartHLSWorker(ctx context.Context) error {
	for {
		seg, err := s.stream.ReadHLSSegment()
		if err != nil {
			glog.Errorf("Error reading segment in HLS subscribe worker")
			return err
		}

		for _, hlsmux := range s.hlsSubscribers {
			// glog.Infof("Writing segment %v to muxes", strings.Split(seg.Name, "_")[1])
			hlsmux.WriteSegment(seg.Name, seg.Data)
		}

		select {
		case <-ctx.Done():
			glog.Errorf("Canceling HLS Worker.")
			return ctx.Err()
		default:
		}
	}
}
