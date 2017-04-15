package segmenter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/golang/glog"
	"github.com/kz26/m3u8"
	"github.com/livepeer/lpms/stream"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/format/rtmp"
)

var ErrSegmenterTimeout = errors.New("SegmenterTimeout")

type SegmenterOptions struct {
	EnforceKeyframe bool //Enforce each segment starts with a keyframe
	SegLength       time.Duration
}

type VideoSegment struct {
	Codec  av.CodecType
	Format stream.VideoFormat
	Length time.Duration
	Data   []byte
	Name   string
}

type VideoPlaylist struct {
	Format stream.VideoFormat
	// Data   []byte
	Data *m3u8.MediaPlaylist
}

type VideoSegmenter interface{}

//FFMpegVideoSegmenter segments a RTMP stream by invoking FFMpeg and monitoring the file system.
type FFMpegVideoSegmenter struct {
	WorkDir        string
	LocalRtmpUrl   string
	StrmID         string
	curSegment     int
	curPlaylist    *m3u8.MediaPlaylist
	curPlWaitTime  time.Duration
	curSegWaitTime time.Duration
	SegLen         time.Duration
}

func NewFFMpegVideoSegmenter(workDir string, strmID string, localRtmpUrl string, segLen time.Duration) *FFMpegVideoSegmenter {
	return &FFMpegVideoSegmenter{WorkDir: workDir, StrmID: strmID, LocalRtmpUrl: localRtmpUrl, SegLen: segLen}
}

//RTMPToHLS invokes the FFMpeg command to do the segmenting.  This method blocks unless killed.
func (s *FFMpegVideoSegmenter) RTMPToHLS(ctx context.Context, opt SegmenterOptions) error {
	//Set up local workdir
	if _, err := os.Stat(s.WorkDir); os.IsNotExist(err) {
		err := os.Mkdir(s.WorkDir, 0700)
		if err != nil {
			return err
		}
	}

	//Test to make sure local RTMP is running.
	rtmpMux, err := rtmp.Dial(s.LocalRtmpUrl)
	if err != nil {
		glog.Errorf("Video Segmenter Error: %v.  Make sure local RTMP stream is available for segmenter.", err)
		rtmpMux.Close()
		return err
	}
	rtmpMux.Close()

	//Invoke the FFMpeg command
	// fmt.Println("ffmpeg", "-i", fmt.Sprintf("rtmp://localhost:%v/stream/%v", "1935", "test"), "-vcodec", "copy", "-acodec", "copy", "-bsf:v", "h264_mp4toannexb", "-f", "segment", "-muxdelay", "0", "-segment_list", "./tmp/stream.m3u8", "./tmp/stream_%d.ts")
	plfn := fmt.Sprintf("%s/%s.m3u8", s.WorkDir, s.StrmID)
	tsfn := s.WorkDir + "/" + s.StrmID + "_%d.ts"

	//This command needs to be manually killed, because ffmpeg doesn't seem to quit after getting a rtmp EOF
	cmd := exec.Command("ffmpeg", "-i", s.LocalRtmpUrl, "-vcodec", "copy", "-acodec", "copy", "-bsf:v", "h264_mp4toannexb", "-f", "segment", "-muxdelay", "0", "-segment_list", plfn, tsfn)
	err = cmd.Start()
	if err != nil {
		glog.Errorf("Cannot start ffmpeg command.")
		return err
	}

	ec := make(chan error, 1)
	go func() { ec <- cmd.Wait() }()

	select {
	case ffmpege := <-ec:
		glog.Errorf("Error from ffmpeg: %v", ffmpege)
		return ffmpege
	case <-ctx.Done():
		//Can't close RTMP server, joy4 doesn't support it.
		//server.Stop()
		cmd.Process.Kill()
		return ctx.Err()
	}
}

//PollSegment monitors the filesystem and returns a new segment as it becomes available
func (s *FFMpegVideoSegmenter) PollSegment(ctx context.Context) (*VideoSegment, error) {
	var length time.Duration
	curTsfn := s.WorkDir + "/" + s.StrmID + "_" + strconv.Itoa(s.curSegment) + ".ts"
	nextTsfn := s.WorkDir + "/" + s.StrmID + "_" + strconv.Itoa(s.curSegment+1) + ".ts"
	seg, err := s.pollSegment(ctx, curTsfn, nextTsfn, time.Millisecond*100)
	if err != nil {
		return nil, err
	}

	name := s.StrmID + "_" + strconv.Itoa(s.curSegment) + ".ts"
	if s.curPlaylist != nil && s.curPlaylist.Segments[s.curSegment] != nil {
		//This is ridiculous - but it's how we can round floats in Go
		sec, _ := strconv.Atoi(fmt.Sprintf("%.0f", s.curPlaylist.Segments[s.curSegment].Duration))
		length = time.Duration(sec) * 1000 * time.Millisecond
	}

	s.curSegment = s.curSegment + 1
	glog.Infof("Segment: %v, len:%v", name, len(seg))
	return &VideoSegment{Codec: av.H264, Format: stream.HLS, Length: length, Data: seg, Name: name}, err
}

//PollPlaylist monitors the filesystem and returns a new playlist as it becomes available
func (s *FFMpegVideoSegmenter) PollPlaylist(ctx context.Context) (*VideoPlaylist, error) {
	plfn := fmt.Sprintf("%s/%s.m3u8", s.WorkDir, s.StrmID)
	var lastPl []byte
	if s.curPlaylist == nil {
		lastPl = nil
	} else {
		lastPl = s.curPlaylist.Encode().Bytes()
	}

	pl, err := s.pollPlaylist(ctx, plfn, time.Millisecond*100, lastPl)
	if err != nil {
		return nil, err
	}

	p, err := m3u8.NewMediaPlaylist(50000, 50000)
	err = p.DecodeFrom(bytes.NewReader(pl), true)
	if err != nil {
		return nil, err
	}

	s.curPlaylist = p
	return &VideoPlaylist{Format: stream.HLS, Data: p}, err
}

func (s *FFMpegVideoSegmenter) pollPlaylist(ctx context.Context, fn string, sleepTime time.Duration, lastFile []byte) (f []byte, err error) {
	for {
		if _, err := os.Stat(fn); err == nil {
			if err != nil {
				return nil, err
			}

			content, err := ioutil.ReadFile(fn)
			if err != nil {
				return nil, err
			}

			//The m3u8 package has some bugs, so the translation isn't 100% correct...
			p, err := m3u8.NewMediaPlaylist(50000, 50000)
			err = p.DecodeFrom(bytes.NewReader(content), true)
			if err != nil {
				return nil, err
			}
			curFile := p.Encode().Bytes()

			// fmt.Printf("p.Segments: %v\n", p.Segments[0])
			// fmt.Printf("lf: %s \ncf: %s \ncomp:%v\n\n", lastFile, curFile, bytes.Compare(lastFile, curFile))
			if lastFile == nil || bytes.Compare(lastFile, curFile) != 0 {
				s.curPlWaitTime = 0
				return content, nil
			}
		}

		select {
		case <-ctx.Done():
			fmt.Println("ctx.Done()!!!")
			return nil, ctx.Err()
		default:
		}

		if s.curPlWaitTime >= 10*s.SegLen {
			return nil, ErrSegmenterTimeout
		}
		time.Sleep(sleepTime)
		s.curPlWaitTime = s.curPlWaitTime + sleepTime
	}

}

func (s *FFMpegVideoSegmenter) pollSegment(ctx context.Context, curFn string, nextFn string, sleepTime time.Duration) (f []byte, err error) {
	for {
		//Because FFMpeg keeps appending to the current segment until it's full before moving onto the next segment, we monitor the existance of
		//the next file as a signal for the completion of the current segment.
		if _, err := os.Stat(nextFn); err == nil {
			content, err := ioutil.ReadFile(curFn)
			if err != nil {
				return nil, err
			}
			s.curSegWaitTime = 0
			return content, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if s.curSegWaitTime > 10*s.SegLen {
			return nil, ErrSegmenterTimeout
		}

		time.Sleep(sleepTime)
		s.curSegWaitTime = s.curSegWaitTime + sleepTime
	}
}
