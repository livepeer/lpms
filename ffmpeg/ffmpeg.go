package ffmpeg

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	pb "github.com/livepeer/lpms/ffmpeg/proto"
)

// #cgo pkg-config: libavformat libavfilter libavcodec libavutil libswscale
// #include <stdlib.h>
// #include "transcoder.h"
// #include "extras.h"
import "C"

var ErrTranscoderRes = errors.New("TranscoderInvalidResolution")
var ErrTranscoderHw = errors.New("TranscoderInvalidHardware")
var ErrTranscoderInp = errors.New("TranscoderInvalidInput")
var ErrTranscoderClipConfig = errors.New("TranscoderInvalidClipConfig")
var ErrTranscoderVid = errors.New("TranscoderInvalidVideo")
var ErrTranscoderStp = errors.New("TranscoderStopped")
var ErrTranscoderFmt = errors.New("TranscoderUnrecognizedFormat")
var ErrTranscoderPrf = errors.New("TranscoderUnrecognizedProfile")
var ErrTranscoderGOP = errors.New("TranscoderInvalidGOP")
var ErrTranscoderDev = errors.New("TranscoderIncompatibleDevices")
var ErrEmptyData = errors.New("EmptyData")
var ErrDNNInitialize = errors.New("DetectorInitializationError")
var ErrSignCompare = errors.New("InvalidSignData")
var ErrTranscoderPixelformat = errors.New("TranscoderInvalidPixelformat")
var ErrVideoCompare = errors.New("InvalidVideoData")

type Acceleration int

const (
	Software Acceleration = iota
	Nvidia
	Amd
)

var FfEncoderLookup = map[Acceleration]map[VideoCodec]string{
	Software: {
		H264: "libx264",
		H265: "libx265",
		VP8:  "libvpx",
		VP9:  "libvpx-vp9",
	},
	Nvidia: {
		H264: "h264_nvenc",
		H265: "hevc_nvenc",
	},
}

type ComponentOptions struct {
	Name string
	Opts map[string]string
}

type Transcoder struct {
	handle  *C.struct_transcode_thread
	stopped bool
	started bool
	mu      *sync.Mutex
}

type TranscodeOptionsIn struct {
	Fname       string
	Accel       Acceleration
	Device      string
	Transmuxing bool
}

type TranscodeOptions struct {
	Oname    string
	Profile  VideoProfile
	Detector DetectorProfile
	Accel    Acceleration
	Device   string
	CalcSign bool
	From     time.Duration
	To       time.Duration

	Muxer        ComponentOptions
	VideoEncoder ComponentOptions
	AudioEncoder ComponentOptions
}

type MediaInfo struct {
	Frames     int
	Pixels     int64
	DetectData DetectData
}

type TranscodeResults struct {
	Decoded MediaInfo
	Encoded []MediaInfo
}

type PixelFormat struct {
	RawValue int
}

type ColorDepthBits int

type ChromaSubsampling int

const (
	ChromaSubsampling420 ChromaSubsampling = iota
	ChromaSubsampling422
	ChromaSubsampling444
)

func (self PixelFormat) Properties() (ChromaSubsampling, ColorDepthBits, error) {
	switch self.RawValue {
	case C.AV_PIX_FMT_YUV420P:
		return ChromaSubsampling420, 8, nil
	case C.AV_PIX_FMT_YUYV422:
		return ChromaSubsampling422, 8, nil
	case C.AV_PIX_FMT_YUV422P:
		return ChromaSubsampling422, 8, nil
	case C.AV_PIX_FMT_YUV444P:
		return ChromaSubsampling444, 8, nil
	case C.AV_PIX_FMT_UYVY422:
		return ChromaSubsampling422, 8, nil
	case C.AV_PIX_FMT_NV12:
		return ChromaSubsampling420, 8, nil
	case C.AV_PIX_FMT_NV21:
		return ChromaSubsampling420, 8, nil
	case C.AV_PIX_FMT_YUV420P10BE:
		return ChromaSubsampling420, 10, nil
	case C.AV_PIX_FMT_YUV420P10LE:
		return ChromaSubsampling420, 10, nil
	case C.AV_PIX_FMT_YUV422P10BE:
		return ChromaSubsampling422, 10, nil
	case C.AV_PIX_FMT_YUV422P10LE:
		return ChromaSubsampling422, 10, nil
	case C.AV_PIX_FMT_YUV444P10BE:
		return ChromaSubsampling444, 10, nil
	case C.AV_PIX_FMT_YUV444P10LE:
		return ChromaSubsampling444, 10, nil
	case C.AV_PIX_FMT_YUV420P16LE:
		return ChromaSubsampling420, 16, nil
	case C.AV_PIX_FMT_YUV420P16BE:
		return ChromaSubsampling420, 16, nil
	case C.AV_PIX_FMT_YUV422P16LE:
		return ChromaSubsampling422, 16, nil
	case C.AV_PIX_FMT_YUV422P16BE:
		return ChromaSubsampling422, 16, nil
	case C.AV_PIX_FMT_YUV444P16LE:
		return ChromaSubsampling444, 16, nil
	case C.AV_PIX_FMT_YUV444P16BE:
		return ChromaSubsampling444, 16, nil
	case C.AV_PIX_FMT_YUV420P12BE:
		return ChromaSubsampling420, 12, nil
	case C.AV_PIX_FMT_YUV420P12LE:
		return ChromaSubsampling420, 12, nil
	case C.AV_PIX_FMT_YUV422P12BE:
		return ChromaSubsampling422, 12, nil
	case C.AV_PIX_FMT_YUV422P12LE:
		return ChromaSubsampling422, 12, nil
	case C.AV_PIX_FMT_YUV444P12BE:
		return ChromaSubsampling444, 12, nil
	case C.AV_PIX_FMT_YUV444P12LE:
		return ChromaSubsampling444, 12, nil
	default:
		return 0, 0, ErrTranscoderPixelformat
	}
}

type GetCodecStatus int

const (
	GetCodecInternalError  GetCodecStatus = -1
	GetCodecOk             GetCodecStatus = 0
	GetCodecNeedsBypass    GetCodecStatus = 1
	GetCodecStreamsMissing GetCodecStatus = 2
)

func GetCodecInfo(fname string) (GetCodecStatus, string, string, PixelFormat, error) {
	var acodec, vcodec string
	vpixel_format_c := C.int(-1)
	cfname := C.CString(fname)
	defer C.free(unsafe.Pointer(cfname))
	acodec_c := C.CString(strings.Repeat("0", 255))
	vcodec_c := C.CString(strings.Repeat("0", 255))
	defer C.free(unsafe.Pointer(acodec_c))
	defer C.free(unsafe.Pointer(vcodec_c))
	status := GetCodecStatus(C.lpms_get_codec_info(cfname, vcodec_c, acodec_c, &vpixel_format_c))
	if C.strlen(acodec_c) < 255 {
		acodec = C.GoString(acodec_c)
	}
	if C.strlen(vcodec_c) < 255 {
		vcodec = C.GoString(vcodec_c)
	}
	pixelFormat := PixelFormat{int(vpixel_format_c)}
	return status, acodec, vcodec, pixelFormat, nil
}

// GetCodecInfo opens the segment and attempts to get video and audio codec names. Additionally, first return value
// indicates whether the segment has zero video frames
func GetCodecInfoBytes(data []byte) (GetCodecStatus, string, string, PixelFormat, error) {
	var acodec, vcodec string
	var pixelFormat PixelFormat
	status := GetCodecInternalError
	or, ow, err := os.Pipe()
	go func() {
		br := bytes.NewReader(data)
		io.Copy(ow, br)
		ow.Close()
	}()
	if err != nil {
		return status, acodec, vcodec, pixelFormat, ErrEmptyData
	}
	fname := fmt.Sprintf("pipe:%d", or.Fd())
	status, acodec, vcodec, pixelFormat, err = GetCodecInfo(fname)
	return status, acodec, vcodec, pixelFormat, err
}

// HasZeroVideoFrameBytes  opens video and returns true if it has video stream with 0-frame
func HasZeroVideoFrameBytes(data []byte) (bool, error) {
	if len(data) == 0 {
		return false, ErrEmptyData
	}
	or, ow, err := os.Pipe()
	if err != nil {
		return false, err
	}
	fname := fmt.Sprintf("pipe:%d", or.Fd())
	cfname := C.CString(fname)
	defer C.free(unsafe.Pointer(cfname))
	go func() {
		br := bytes.NewReader(data)
		io.Copy(ow, br)
		ow.Close()
	}()
	vpixel_format_c := C.int(-1)
	acodec_c := C.CString(strings.Repeat("0", 255))
	vcodec_c := C.CString(strings.Repeat("0", 255))
	defer C.free(unsafe.Pointer(acodec_c))
	defer C.free(unsafe.Pointer(vcodec_c))
	bres := int(C.lpms_get_codec_info(cfname, vcodec_c, acodec_c, &vpixel_format_c))
	ow.Close()
	return bres == 1, nil
}

// compare two signature files whether those matches or not
func CompareSignatureByPath(fname1 string, fname2 string) (bool, error) {
	if len(fname1) <= 0 || len(fname2) <= 0 {
		return false, nil
	}
	cfpath1 := C.CString(fname1)
	defer C.free(unsafe.Pointer(cfpath1))
	cfpath2 := C.CString(fname2)
	defer C.free(unsafe.Pointer(cfpath2))

	res := int(C.lpms_compare_sign_bypath(cfpath1, cfpath2))

	if res > 0 {
		return true, nil
	} else if res == 0 {
		return false, nil
	} else {
		return false, ErrSignCompare
	}
}

// compare two signature buffers whether those matches or not
func CompareSignatureByBuffer(data1 []byte, data2 []byte) (bool, error) {

	pdata1 := unsafe.Pointer(&data1[0])
	pdata2 := unsafe.Pointer(&data2[0])

	res := int(C.lpms_compare_sign_bybuffer(pdata1, C.int(len(data1)), pdata2, C.int(len(data2))))

	if res > 0 {
		return true, nil
	} else if res == 0 {
		return false, nil
	} else {
		return false, ErrSignCompare
	}
}

// compare two vidoe files whether those matches or not
func CompareVideoByPath(fname1 string, fname2 string) (bool, error) {
	if len(fname1) <= 0 || len(fname2) <= 0 {
		return false, nil
	}
	cfpath1 := C.CString(fname1)
	defer C.free(unsafe.Pointer(cfpath1))
	cfpath2 := C.CString(fname2)
	defer C.free(unsafe.Pointer(cfpath2))

	res := int(C.lpms_compare_video_bypath(cfpath1, cfpath2))

	if res == 0 {
		return true, nil
	} else if res == 1 {
		return false, nil
	} else {
		return false, ErrVideoCompare
	}
}

// compare two video buffers whether those matches or not
func CompareVideoByBuffer(data1 []byte, data2 []byte) (bool, error) {

	pdata1 := unsafe.Pointer(&data1[0])
	pdata2 := unsafe.Pointer(&data2[0])

	res := int(C.lpms_compare_video_bybuffer(pdata1, C.int(len(data1)), pdata2, C.int(len(data2))))

	if res == 0 {
		return true, nil
	} else if res == 1 {
		return false, nil
	} else {
		return false, ErrVideoCompare
	}
}

func RTMPToHLS(localRTMPUrl string, outM3U8 string, tmpl string, seglen_secs string, seg_start int) error {
	inp := C.CString(localRTMPUrl)
	outp := C.CString(outM3U8)
	ts_tmpl := C.CString(tmpl)
	seglen := C.CString(seglen_secs)
	segstart := C.CString(fmt.Sprintf("%v", seg_start))
	ret := int(C.lpms_rtmp2hls(inp, outp, ts_tmpl, seglen, segstart))
	C.free(unsafe.Pointer(inp))
	C.free(unsafe.Pointer(outp))
	C.free(unsafe.Pointer(ts_tmpl))
	C.free(unsafe.Pointer(seglen))
	C.free(unsafe.Pointer(segstart))
	if ret != 0 {
		glog.Infof("RTMP2HLS Transmux Return : %v\n", Strerror(ret))
		return ErrorMap[ret]
	}
	return nil
}

func Transcode(input string, workDir string, ps []VideoProfile) error {

	opts := make([]TranscodeOptions, len(ps))
	for i, param := range ps {
		oname := path.Join(workDir, fmt.Sprintf("out%v%v", i, filepath.Base(input)))
		opt := TranscodeOptions{
			Oname:   oname,
			Profile: param,
			Accel:   Software,
		}
		opts[i] = opt
	}
	inopts := &TranscodeOptionsIn{
		Fname: input,
		Accel: Software,
	}
	return Transcode2(inopts, opts)
}

func newAVOpts(opts map[string]string) *C.AVDictionary {
	var dict *C.AVDictionary
	for key, value := range opts {
		k := C.CString(key)
		v := C.CString(value)
		defer C.free(unsafe.Pointer(k))
		defer C.free(unsafe.Pointer(v))
		C.av_dict_set(&dict, k, v, 0)
	}
	return dict
}

// return encoding specific options for the given accel
func configEncoder(inOpts *TranscodeOptionsIn, outOpts TranscodeOptions, inDev, outDev string) (string, string, error) {
	encoder := FfEncoderLookup[outOpts.Accel][outOpts.Profile.Encoder]
	switch inOpts.Accel {
	case Software:
		switch outOpts.Accel {
		case Software:
			return encoder, "scale", nil
		case Nvidia:
			upload := "hwupload_cuda"
			if outDev != "" {
				upload = upload + "=device=" + outDev
			}
			return encoder, upload + ",scale_cuda", nil
		}
	case Nvidia:
		switch outOpts.Accel {
		case Software:
			return encoder, "scale_cuda", nil
		case Nvidia:
			// If we encode on a different device from decode then need to transfer
			if outDev != "" && outDev != inDev {
				return "", "", ErrTranscoderDev // XXX not allowed
			}
			return encoder, "scale_cuda", nil
		}
	}
	return "", "", ErrTranscoderHw
}
func accelDeviceType(accel Acceleration) (C.enum_AVHWDeviceType, error) {
	switch accel {
	case Software:
		return C.AV_HWDEVICE_TYPE_NONE, nil
	case Nvidia:
		return C.AV_HWDEVICE_TYPE_CUDA, nil

	}
	return C.AV_HWDEVICE_TYPE_NONE, ErrTranscoderHw
}

func Transcode2(input *TranscodeOptionsIn, ps []TranscodeOptions) error {
	_, err := Transcode3(input, ps)
	return err
}

func Transcode3(input *TranscodeOptionsIn, ps []TranscodeOptions) (*TranscodeResults, error) {
	t := NewTranscoder()
	defer t.StopTranscoder()
	return t.Transcode(input, ps)
}

func (t *Transcoder) Transcode(input *TranscodeOptionsIn, ps []TranscodeOptions) (*TranscodeResults, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.handle == nil {
		return nil, ErrTranscoderStp
	}
	if input == nil {
		return nil, ErrTranscoderInp
	}
	hw_type, err := accelDeviceType(input.Accel)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		if p.From != 0 || p.To != 0 {
			if p.VideoEncoder.Name == "drop" || p.VideoEncoder.Name == "copy" {
				glog.Warning("Could clip only when transcoding video")
				return nil, ErrTranscoderClipConfig
			}
			if p.From < 0 || p.To < p.From {
				glog.Warning("'To' should be after 'From'")
				return nil, ErrTranscoderClipConfig

			}
		}
	}
	fname := C.CString(input.Fname)
	defer C.free(unsafe.Pointer(fname))
	if input.Transmuxing {
		t.started = true
	}
	if !t.started {
		status, _, vcodec, _, _ := GetCodecInfo(input.Fname)
		// TODO: Check following condition, is vcodec == "" ?
		videoMissing := status == GetCodecNeedsBypass || vcodec == ""
		if videoMissing {
			// Audio-only segment, fail fast right here as we cannot handle them nicely
			return nil, ErrTranscoderVid
		}
		// Stream is either OK or completely broken, let the transcoder handle it
		t.started = true
	}
	params := make([]C.output_params, len(ps))
	for i, p := range ps {
		if p.Detector != nil {
			// We don't do any encoding for detector profiles
			// Adding placeholder values to pass checks for these everywhere
			p.Oname = "/dev/null"
			p.Profile = P144p30fps16x9
			p.Muxer = ComponentOptions{Name: "mpegts"}
		}
		oname := C.CString(p.Oname)
		defer C.free(unsafe.Pointer(oname))

		param := p.Profile
		w, h, err := VideoProfileResolution(param)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return nil, err
			}
		}
		br := strings.Replace(param.Bitrate, "k", "000", 1)
		bitrate, err := strconv.Atoi(br)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return nil, err
			}
		}
		encoder, scale_filter := p.VideoEncoder.Name, "scale"
		if encoder == "" {
			encoder, scale_filter, err = configEncoder(input, p, input.Device, p.Device)
			if err != nil {
				return nil, err
			}
		}
		// preserve aspect ratio along the larger dimension when rescaling
		var filters string
		filters = fmt.Sprintf("%s='w=if(gte(iw,ih),%d,-2):h=if(lt(iw,ih),%d,-2)'", scale_filter, w, h)
		if input.Accel != Software && p.Accel == Software {
			// needed for hw dec -> hw rescale -> sw enc
			filters = filters + ",hwdownload,format=nv12"
		}
		// set FPS denominator to 1 if unset by user
		if param.FramerateDen == 0 {
			param.FramerateDen = 1
		}
		// Add fps filter *after* scale filter because otherwise we could
		// be scaling duplicate frames unnecessarily. This becomes a DoS vector
		// when a user submits two frames that are "far apart" in pts and
		// the fps filter duplicates frames to fill out the difference to maintain
		// a consistent frame rate.
		// Once we allow for alternating segments, this issue should be mitigated
		// and the fps filter can come *before* the scale filter to minimize work
		// when going from high fps to low fps (much more common when transcoding
		// than going from low fps to high fps)
		var fps C.AVRational
		if param.Framerate > 0 {
			filters += fmt.Sprintf(",fps=%d/%d", param.Framerate, param.FramerateDen)
			fps = C.AVRational{num: C.int(param.Framerate), den: C.int(param.FramerateDen)}
		}
		// if has a detector profile, ignore all video options
		if p.Detector != nil {
			switch p.Detector.Type() {
			case SceneClassification:
				detectorProfile := p.Detector.(*SceneClassificationProfile)
				// Set samplerate using select filter to prevent unnecessary HW->SW copying
				filters = fmt.Sprintf("select='not(mod(n\\,%v))'", detectorProfile.SampleRate)
				if input.Accel != Software {
					filters += ",hwdownload,format=nv12"
				}
			}
		}
		var muxOpts C.component_opts
		var muxName string
		switch p.Profile.Format {
		case FormatNone:
			muxOpts = C.component_opts{
				// don't free this bc of avformat_write_header API
				opts: newAVOpts(p.Muxer.Opts),
			}
			muxName = p.Muxer.Name
		case FormatMPEGTS:
			muxName = "mpegts"
		case FormatMP4:
			muxName = "mp4"
			muxOpts = C.component_opts{
				opts: newAVOpts(map[string]string{"movflags": "faststart"}),
			}
		default:
			return nil, ErrTranscoderFmt
		}
		if muxName != "" {
			muxOpts.name = C.CString(muxName)
			defer C.free(unsafe.Pointer(muxOpts.name))
		}
		// Set video encoder options
		if len(p.VideoEncoder.Name) <= 0 && len(p.VideoEncoder.Opts) <= 0 {
			p.VideoEncoder.Opts = map[string]string{
				"forced-idr": "1",
			}
			switch p.Profile.Profile {
			case ProfileH264Baseline, ProfileH264ConstrainedHigh:
				p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
				p.VideoEncoder.Opts["bf"] = "0"
			case ProfileH264Main, ProfileH264High:
				p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
				p.VideoEncoder.Opts["bf"] = "3"
			case ProfileNone:
				if p.Accel == Nvidia {
					p.VideoEncoder.Opts["bf"] = "0"
				} else {
					p.VideoEncoder.Opts["bf"] = "3"
				}
			default:
				return nil, ErrTranscoderPrf
			}
			if p.Profile.Framerate == 0 && p.Accel == Nvidia {
				// When the decoded video contains non-monotonic increases in PTS (common with OBS)
				// & when B-frames are enabled nvenc struggles at calculating correct DTS
				// XXX so we disable B-frames altogether to avoid PTS < DTS errors
				if p.VideoEncoder.Opts["bf"] != "0" {
					p.VideoEncoder.Opts["bf"] = "0"
					glog.Warning("Forcing max_b_frames=0 for nvenc, as it can't handle those well with timestamp passthrough")
				}
			}
		}
		gopMs := 0
		if param.GOP != 0 {
			if param.GOP <= GOPInvalid {
				return nil, ErrTranscoderGOP
			}
			// Check for intra-only
			if param.GOP == GOPIntraOnly {
				p.VideoEncoder.Opts["g"] = "0"
			} else {
				if param.Framerate > 0 {
					gop := param.GOP.Seconds()
					interval := strconv.Itoa(int(gop * float64(param.Framerate)))
					p.VideoEncoder.Opts["g"] = interval
				} else {
					gopMs = int(param.GOP.Milliseconds())
				}
			}
		}
		vidOpts := C.component_opts{
			name: C.CString(encoder),
			opts: newAVOpts(p.VideoEncoder.Opts),
		}
		audioEncoder := p.AudioEncoder.Name
		if audioEncoder == "" {
			audioEncoder = "aac"
		}
		audioOpts := C.component_opts{
			name: C.CString(audioEncoder),
			opts: newAVOpts(p.AudioEncoder.Opts),
		}
		fromMs := int(p.From.Milliseconds())
		toMs := int(p.To.Milliseconds())
		vfilt := C.CString(filters)
		defer C.free(unsafe.Pointer(vidOpts.name))
		defer C.free(unsafe.Pointer(audioOpts.name))
		defer C.free(unsafe.Pointer(vfilt))
		isDNN := C.int(0)
		if p.Detector != nil {
			isDNN = C.int(1)
		}
		params[i] = C.output_params{fname: oname, fps: fps,
			w: C.int(w), h: C.int(h), bitrate: C.int(bitrate),
			gop_time: C.int(gopMs), from: C.int(fromMs), to: C.int(toMs),
			muxer: muxOpts, audio: audioOpts, video: vidOpts,
			vfilters: vfilt, sfilters: nil, is_dnn: isDNN}
		if p.CalcSign {
			//signfilter string
			escapedOname := ffmpegStrEscape(p.Oname)
			signfilter := fmt.Sprintf("signature=filename='%s.bin'", escapedOname)
			if p.Accel == Nvidia {
				//hw frame -> cuda signature -> sign.bin
				signfilter = fmt.Sprintf("signature_cuda=filename='%s.bin'", escapedOname)
			}
			sfilt := C.CString(signfilter)
			params[i].sfilters = sfilt
			defer C.free(unsafe.Pointer(sfilt))
		}
		defer func(param *C.output_params) {
			// Work around the ownership rules:
			// ffmpeg normally takes ownership of the following AVDictionary options
			// However, if we don't pass these opts to ffmpeg, then we need to free
			if param.muxer.opts != nil {
				C.av_dict_free(&param.muxer.opts)
			}
			if param.audio.opts != nil {
				C.av_dict_free(&param.audio.opts)
			}
			if param.video.opts != nil {
				C.av_dict_free(&param.video.opts)
			}
		}(&params[i])
	}
	var device *C.char
	if input.Device != "" {
		device = C.CString(input.Device)
		defer C.free(unsafe.Pointer(device))
	}
	inp := &C.input_params{fname: fname, hw_type: hw_type, device: device,
		handle: t.handle}
	if input.Transmuxing {
		inp.transmuxe = 1
	}
	results := make([]C.output_results, len(ps))
	decoded := &C.output_results{}
	var (
		paramsPointer  *C.output_params
		resultsPointer *C.output_results
	)
	if len(params) > 0 {
		paramsPointer = (*C.output_params)(&params[0])
		resultsPointer = (*C.output_results)(&results[0])
	}
	ret := int(C.lpms_transcode(inp, paramsPointer, resultsPointer, C.int(len(params)), decoded))
	if ret != 0 {
		glog.Error("Transcoder Return : ", ErrorMap[ret])
		if ret == int(C.lpms_ERR_UNRECOVERABLE) {
			panic(ErrorMap[ret])
		}
		return nil, ErrorMap[ret]
	}
	tr := make([]MediaInfo, len(ps))
	for i, r := range results {
		tr[i] = MediaInfo{
			Frames: int(r.frames),
			Pixels: int64(r.pixels),
		}
		// add detect result
		if ps[i].Detector != nil {
			switch ps[i].Detector.Type() {
			case SceneClassification:
				detector := ps[i].Detector.(*SceneClassificationProfile)
				res := make(SceneClassificationData)
				for j, class := range detector.Classes {
					res[class.ID] = float64(r.probs[j])
				}
				tr[i].DetectData = res
			}
		}
	}
	dec := MediaInfo{
		Frames: int(decoded.frames),
		Pixels: int64(decoded.pixels),
	}
	return &TranscodeResults{Encoded: tr, Decoded: dec}, nil
}

func (t *Transcoder) Discontinuity() {
	t.mu.Lock()
	defer t.mu.Unlock()
	C.lpms_transcode_discontinuity(t.handle)
}

func NewTranscoder() *Transcoder {
	return &Transcoder{
		handle: C.lpms_transcode_new(),
		mu:     &sync.Mutex{},
	}
}

func (t *Transcoder) StopTranscoder() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	C.lpms_transcode_stop(t.handle)
	t.handle = nil // prevent accidental reuse
	t.stopped = true
}

type LogLevel C.enum_LPMSLogLevel

const (
	FFLogTrace   = C.LPMS_LOG_TRACE
	FFLogDebug   = C.LPMS_LOG_DEBUG
	FFLogVerbose = C.LPMS_LOG_VERBOSE
	FFLogInfo    = C.LPMS_LOG_INFO
	FFLogWarning = C.LPMS_LOG_WARNING
	FFLogError   = C.LPMS_LOG_ERROR
	FFLogFatal   = C.LPMS_LOG_FATAL
	FFLogPanic   = C.LPMS_LOG_PANIC
	FFLogQuiet   = C.LPMS_LOG_QUIET
)

func InitFFmpegWithLogLevel(level LogLevel) {
	C.lpms_init(C.enum_LPMSLogLevel(level))
}

func InitFFmpeg() {
	InitFFmpegWithLogLevel(FFLogWarning)
}

func NewTranscoderWithDetector(detector DetectorProfile, deviceid string) (*Transcoder, error) {
	switch detector.Type() {
	case SceneClassification:
		detectorProfile := detector.(*SceneClassificationProfile)
		backendConfigs := createBackendConfig(deviceid)
		dnnOpt := &C.lvpdnn_opts{
			modelpath:       C.CString(detectorProfile.ModelPath),
			inputname:       C.CString(detectorProfile.Input),
			outputname:      C.CString(detectorProfile.Output),
			backend_configs: C.CString(backendConfigs),
		}
		defer C.free(unsafe.Pointer(dnnOpt.modelpath))
		defer C.free(unsafe.Pointer(dnnOpt.inputname))
		defer C.free(unsafe.Pointer(dnnOpt.outputname))
		defer C.free(unsafe.Pointer(dnnOpt.backend_configs))
		handle := C.lpms_transcode_new_with_dnn(dnnOpt)
		if handle != nil {
			return &Transcoder{
				handle: handle,
				mu:     &sync.Mutex{},
			}, nil
		}
	}
	return nil, ErrDNNInitialize
}

func createBackendConfig(deviceid string) string {
	configProto := &pb.ConfigProto{GpuOptions: &pb.GPUOptions{AllowGrowth: true}}
	bytes, err := proto.Marshal(configProto)
	if err != nil {
		glog.Errorf("Unable to convert deviceid %v to Tensorflow config protobuf\n", err)
		return ""
	}
	sessConfigOpt := fmt.Sprintf("device_id=%s&sess_config=0x", deviceid)
	// serialize TF config proto as hex
	for i := len(bytes) - 1; i >= 0; i-- {
		sessConfigOpt += hex.EncodeToString(bytes[i : i+1])
	}
	return sessConfigOpt
}

func ffmpegStrEscape(origStr string) string {
	tmpStr := strings.ReplaceAll(origStr, "\\", "\\\\")
	outStr := strings.ReplaceAll(tmpStr, ":", "\\:")
	return outStr
}
