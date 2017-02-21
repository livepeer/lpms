package streaming

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestStreamID(t *testing.T) {
	nodeID := common.HexToHash("0x3ee489d01ab49caf1be0c824f2c913705e97d359ebbdd19a7389700cb8b7114d")
	streamID := "c8fdf676c39cb0a01133562c4fd81743e012a6107dc544e3555a24296aeaed23"

	res := MakeStreamID(nodeID, streamID)
	expected := "3ee489d01ab49caf1be0c824f2c913705e97d359ebbdd19a7389700cb8b7114dc8fdf676c39cb0a01133562c4fd81743e012a6107dc544e3555a24296aeaed23"

	if res.String() != expected {
		t.Errorf("MakeStreamID returned %v and should have returned %v", res, expected)
	}

	rn, rs := res.SplitComponents()
	if rn != nodeID || rs != streamID {
		t.Errorf("SplitComponents returned %v, %v", rn, rs)
	}
}

func TestStreamerRegistry(t *testing.T) {
	addr := randomStreamID()
	streamID := randomStreamID()
	streamer, _ := NewStreamer(addr)

	firstStream, _ := streamer.AddNewStream()
	_, rs := firstStream.ID.SplitComponents()

	if len(streamer.Streams) != 1 {
		t.Errorf("AddNewStream() didn't add a stream to the streamer")
	}

	resStream, _ := streamer.GetStream(addr, rs)
	if resStream != firstStream {
		t.Errorf("GetStream() didn't return the expected stream")
	}

	// Subscribe to stream
	sid := MakeStreamID(addr, streamID.Str())
	_, err := streamer.SubscribeToStream(sid.String())
	if err != nil {
		t.Errorf("Got an error subscribing to a new stream. %v", err)
	}

	_, err = streamer.SubscribeToStream(sid.String())
	if err == nil {
		t.Errorf("Didn't get an error subscribing to the same stream twice and should have.")
	}
}
