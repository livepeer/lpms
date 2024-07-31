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
	"runtime"
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
// #include <libavutil/log.h>
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
var ErrSignCompare = errors.New("InvalidSignData")
var ErrTranscoderPixelformat = errors.New("TranscoderInvalidPixelformat")
var ErrVideoCompare = errors.New("InvalidVideoData")

// Switch to turn off logging transcoding errors, when doing test transcoding
var LogTranscodeErrors = true

type Acceleration int

const (
	Software Acceleration = iota
	Nvidia
	Amd
	Netint
)

var AccelerationNameLookup = map[Acceleration]string{
	Software: "SW",
	Nvidia:   "Nvidia",
	Amd:      "Amd",
	Netint:   "Netint",
}

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
	Netint: {
		H264: "h264_ni_enc",
		H265: "h265_ni_enc",
	},
}

type ComponentOptions struct {
	Name string
	Opts map[string]string
}

type Transcoder struct {
	handle     *C.struct_transcode_thread
	stopped    bool
	started    bool
	lastacodec string
	mu         *sync.Mutex
}

type TranscodeOptionsIn struct {
	Fname       string
	Accel       Acceleration
	Device      string
	Transmuxing bool
	Profile     VideoProfile
}

type TranscodeOptions struct {
	Oname    string
	Profile  VideoProfile
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
	Frames int
	Pixels int64
}

type TranscodeResults struct {
	Decoded MediaInfo
	Encoded []MediaInfo
}

type PixelFormat struct {
	RawValue int
}

const (
	PixelFormatNone        int = C.AV_PIX_FMT_NONE
	PixelFormatYUV420P     int = C.AV_PIX_FMT_YUV420P
	PixelFormatYUYV422     int = C.AV_PIX_FMT_YUYV422
	PixelFormatYUV422P     int = C.AV_PIX_FMT_YUV422P
	PixelFormatYUV444P     int = C.AV_PIX_FMT_YUV444P
	PixelFormatUYVY422     int = C.AV_PIX_FMT_UYVY422
	PixelFormatNV12        int = C.AV_PIX_FMT_NV12
	PixelFormatNV21        int = C.AV_PIX_FMT_NV21
	PixelFormatYUV420P10BE int = C.AV_PIX_FMT_YUV420P10BE
	PixelFormatYUV420P10LE int = C.AV_PIX_FMT_YUV420P10LE
	PixelFormatYUV422P10BE int = C.AV_PIX_FMT_YUV422P10BE
	PixelFormatYUV422P10LE int = C.AV_PIX_FMT_YUV422P10LE
	PixelFormatYUV444P10BE int = C.AV_PIX_FMT_YUV444P10BE
	PixelFormatYUV444P10LE int = C.AV_PIX_FMT_YUV444P10LE
	PixelFormatYUV420P16LE int = C.AV_PIX_FMT_YUV420P16LE
	PixelFormatYUV420P16BE int = C.AV_PIX_FMT_YUV420P16BE
	PixelFormatYUV422P16LE int = C.AV_PIX_FMT_YUV422P16LE
	PixelFormatYUV422P16BE int = C.AV_PIX_FMT_YUV422P16BE
	PixelFormatYUV444P16LE int = C.AV_PIX_FMT_YUV444P16LE
	PixelFormatYUV444P16BE int = C.AV_PIX_FMT_YUV444P16BE
	PixelFormatYUV420P12BE int = C.AV_PIX_FMT_YUV420P12BE
	PixelFormatYUV420P12LE int = C.AV_PIX_FMT_YUV420P12LE
	PixelFormatYUV422P12BE int = C.AV_PIX_FMT_YUV422P12BE
	PixelFormatYUV422P12LE int = C.AV_PIX_FMT_YUV422P12LE
	PixelFormatYUV444P12BE int = C.AV_PIX_FMT_YUV444P12BE
	PixelFormatYUV444P12LE int = C.AV_PIX_FMT_YUV444P12LE
)

// hold bit number minus 8; ColorDepthBits + 8 == bit number
type ColorDepthBits int

const (
	ColorDepth8Bit  ColorDepthBits = 0
	ColorDepth10Bit ColorDepthBits = 2
	ColorDepth12Bit ColorDepthBits = 4
	ColorDepth16Bit ColorDepthBits = 8
)

type ChromaSubsampling int

const (
	ChromaSubsampling420 ChromaSubsampling = iota
	ChromaSubsampling422
	ChromaSubsampling444
)

func (pixelFormat PixelFormat) Properties() (ChromaSubsampling, ColorDepthBits, error) {
	switch pixelFormat.RawValue {
	case C.AV_PIX_FMT_YUV420P:
		return ChromaSubsampling420, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_YUYV422:
		return ChromaSubsampling422, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_YUV422P:
		return ChromaSubsampling422, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_YUV444P:
		return ChromaSubsampling444, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_UYVY422:
		return ChromaSubsampling422, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_NV12:
		return ChromaSubsampling420, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_NV21:
		return ChromaSubsampling420, ColorDepth8Bit, nil
	case C.AV_PIX_FMT_YUV420P10BE:
		return ChromaSubsampling420, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV420P10LE:
		return ChromaSubsampling420, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV422P10BE:
		return ChromaSubsampling422, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV422P10LE:
		return ChromaSubsampling422, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV444P10BE:
		return ChromaSubsampling444, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV444P10LE:
		return ChromaSubsampling444, ColorDepth10Bit, nil
	case C.AV_PIX_FMT_YUV420P16LE:
		return ChromaSubsampling420, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV420P16BE:
		return ChromaSubsampling420, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV422P16LE:
		return ChromaSubsampling422, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV422P16BE:
		return ChromaSubsampling422, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV444P16LE:
		return ChromaSubsampling444, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV444P16BE:
		return ChromaSubsampling444, ColorDepth16Bit, nil
	case C.AV_PIX_FMT_YUV420P12BE:
		return ChromaSubsampling420, ColorDepth12Bit, nil
	case C.AV_PIX_FMT_YUV420P12LE:
		return ChromaSubsampling420, ColorDepth12Bit, nil
	case C.AV_PIX_FMT_YUV422P12BE:
		return ChromaSubsampling422, ColorDepth12Bit, nil
	case C.AV_PIX_FMT_YUV422P12LE:
		return ChromaSubsampling422, ColorDepth12Bit, nil
	case C.AV_PIX_FMT_YUV444P12BE:
		return ChromaSubsampling444, ColorDepth12Bit, nil
	case C.AV_PIX_FMT_YUV444P12LE:
		return ChromaSubsampling444, ColorDepth12Bit, nil
	default:
		return ChromaSubsampling420, ColorDepth8Bit, ErrTranscoderPixelformat
	}
}

type CodecStatus int

const (
	CodecStatusInternalError CodecStatus = -1
	CodecStatusOk            CodecStatus = 0
	CodecStatusNeedsBypass   CodecStatus = 1
	CodecStatusMissing       CodecStatus = 2
)

type MediaFormatInfo struct {
	Format         string
	Acodec, Vcodec string
	PixFormat      PixelFormat
	Width, Height  int
	FPS            float32
	DurSecs        int64
	AudioBitrate   int
}

func (f *MediaFormatInfo) ScaledHeight(width int) int {
	return int(float32(width) * float32(f.Height) / float32(f.Width))
}

func (f *MediaFormatInfo) ScaledWidth(height int) int {
	return int(float32(height) * float32(f.Width) / float32(f.Height))
}

func GetCodecInfo(fname string) (CodecStatus, MediaFormatInfo, error) {
	format := MediaFormatInfo{}
	cfname := C.CString(fname)
	defer C.free(unsafe.Pointer(cfname))
	fmtname := C.CString(strings.Repeat("0", 255))
	acodec_c := C.CString(strings.Repeat("0", 255))
	vcodec_c := C.CString(strings.Repeat("0", 255))
	defer C.free(unsafe.Pointer(fmtname))
	defer C.free(unsafe.Pointer(acodec_c))
	defer C.free(unsafe.Pointer(vcodec_c))
	var params_c C.codec_info
	params_c.format_name = fmtname
	params_c.video_codec = vcodec_c
	params_c.audio_codec = acodec_c
	params_c.pixel_format = C.AV_PIX_FMT_NONE
	status := CodecStatus(C.lpms_get_codec_info(cfname, &params_c))
	if C.strlen(fmtname) < 255 {
		format.Format = C.GoString(fmtname)
	}
	if C.strlen(acodec_c) < 255 {
		format.Acodec = C.GoString(acodec_c)
	}
	if C.strlen(vcodec_c) < 255 {
		format.Vcodec = C.GoString(vcodec_c)
	}
	format.PixFormat = PixelFormat{int(params_c.pixel_format)}
	format.Width = int(params_c.width)
	format.Height = int(params_c.height)
	format.FPS = float32(params_c.fps)
	format.DurSecs = int64(params_c.dur)
	format.AudioBitrate = int(params_c.audio_bit_rate)
	return status, format, nil
}

// GetCodecInfo opens the segment and attempts to get video and audio codec names. Additionally, first return value
// indicates whether the segment has zero video frames
func GetCodecInfoBytes(data []byte) (CodecStatus, MediaFormatInfo, error) {
	format := MediaFormatInfo{}
	status := CodecStatusInternalError
	or, ow, err := os.Pipe()
	go func() {
		br := bytes.NewReader(data)
		io.Copy(ow, br)
		ow.Close()
	}()
	if err != nil {
		return status, format, ErrEmptyData
	}
	fname := fmt.Sprintf("pipe:%d", or.Fd())
	status, format, err = GetCodecInfo(fname)

	// estimate duration from bitrate and filesize for audio
	// some formats do not have built-in track duration metadata,
	// and pipes do not have a filesize on their own which breaks ffmpeg's own
	// duration estimates. So do the estimation calculation ourselves
	// NB : mpegts has the same problem but may contain video so let's not handle that
	//      some other formats, eg ogg, show zero bitrate
	//
	// ffmpeg estimation of duration from bitrate:
	// https://github.com/FFmpeg/FFmpeg/blob/8280ec7a3213c9b7bad88aac3695be2dedd2c00b/libavformat/demux.c#L1798
	if format.DurSecs == 0 && format.AudioBitrate > 0 && (format.Format == "mp3" || format.Format == "wav" || format.Format == "aac") {
		format.DurSecs = int64(len(data) * 8 / format.AudioBitrate)
	}
	return status, format, err
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
	go func() {
		br := bytes.NewReader(data)
		io.Copy(ow, br)
		ow.Close()
	}()
	status, _, err := GetCodecInfo(fname)
	ow.Close()
	return status == CodecStatusNeedsBypass, err
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
func configEncoder(inOpts *TranscodeOptionsIn, outOpts TranscodeOptions) (string, string, string, error) {
	inDev := inOpts.Device
	outDev := outOpts.Device
	encoder := FfEncoderLookup[outOpts.Accel][outOpts.Profile.Encoder]
	switch inOpts.Accel {
	case Software:
		switch outOpts.Accel {
		case Software:
			return encoder, "scale", "", nil
		case Nvidia:
			upload := "hwupload_cuda"
			if outDev != "" {
				upload = upload + "=device=" + outDev
			}
			return encoder, upload + "," + hwScale(), hwScaleAlgo(), nil
		}
	case Nvidia:
		switch outOpts.Accel {
		case Software:
			return encoder, hwScale(), hwScaleAlgo(), nil
		case Nvidia:
			// If we encode on a different device from decode then need to transfer
			if outDev != "" && outDev != inDev {
				return "", "", "", ErrTranscoderDev // XXX not allowed
			}
			return encoder, hwScale(), hwScaleAlgo(), nil
		}
	case Netint:
		switch outOpts.Accel {
		case Software, Nvidia:
			return "", "", "", ErrTranscoderDev // XXX don't allow mix-match between NETINT and sw/nv
		case Netint:
			// Use software scale filter
			return encoder, "scale", "", nil
		}
	}
	return "", "", "", ErrTranscoderHw
}
func accelDeviceType(accel Acceleration) (C.enum_AVHWDeviceType, error) {
	switch accel {
	case Software:
		return C.AV_HWDEVICE_TYPE_NONE, nil
	case Nvidia:
		return C.AV_HWDEVICE_TYPE_CUDA, nil
	case Netint:
		return C.AV_HWDEVICE_TYPE_MEDIACODEC, nil
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

type CodingSizeLimit struct {
	WidthMin, HeightMin int
	WidthMax, HeightMax int
}

type Size struct {
	W, H int
}

func (s *Size) Valid(l *CodingSizeLimit) bool {
	if s.W < l.WidthMin || s.W > l.WidthMax || s.H < l.HeightMin || s.H > l.HeightMax {
		glog.Warningf("[not valid] profile %dx%d\n", s.W, s.H)
		return false
	}
	return true
}

func clamp(val, min, max int) int {
	if val <= min {
		return min
	}
	if val >= max {
		return max
	}
	return val
}

func (l *CodingSizeLimit) Clamp(p *VideoProfile, format MediaFormatInfo) error {
	w, h, err := VideoProfileResolution(*p)
	if err != nil {
		return err
	}
	if w <= 0 || h <= 0 {
		return fmt.Errorf("input resolution invalid; probe found w=%d h=%d", w, h)
	}
	// detect correct rotation
	outputAr := float32(w) / float32(h)
	inputAr := float32(format.Width) / float32(format.Height)
	if (inputAr > 1.0) != (outputAr > 1.0) {
		// comparing landscape to portrait, apply rotate on chosen resolution
		w, h = h, w
	}
	// Adjust to minimal encode dimensions keeping aspect ratio

	var adjustedWidth, adjustedHeight Size
	adjustedWidth.W = clamp(w, l.WidthMin, l.WidthMax)
	adjustedWidth.H = format.ScaledHeight(adjustedWidth.W)
	adjustedHeight.H = clamp(h, l.HeightMin, l.HeightMax)
	adjustedHeight.W = format.ScaledWidth(adjustedHeight.H)
	if adjustedWidth.Valid(l) {
		p.Resolution = fmt.Sprintf("%dx%d", adjustedWidth.W, adjustedWidth.H)
		return nil
	}
	if adjustedHeight.Valid(l) {
		p.Resolution = fmt.Sprintf("%dx%d", adjustedHeight.W, adjustedHeight.H)
		return nil
	}
	// Improve error message to include calculation context
	return fmt.Errorf("profile %dx%d size out of bounds %dx%d-%dx%d input=%dx%d adjusted %dx%d or %dx%d",
		w, h, l.WidthMin, l.WidthMin, l.WidthMax, l.HeightMax, format.Width, format.Height, adjustedWidth.W, adjustedWidth.H, adjustedHeight.W, adjustedHeight.H)
}

// 7th Gen NVENC limits:
var nvidiaCodecSizeLimts = map[VideoCodec]CodingSizeLimit{
	H264: {146, 50, 4096, 4096},
	H265: {132, 40, 8192, 8192},
}

func ensureEncoderLimits(outputs []TranscodeOptions, format MediaFormatInfo) error {
	// not using range to be able to make inplace modifications to outputs elements
	for i := 0; i < len(outputs); i++ {
		if outputs[i].Accel == Nvidia {
			limits, haveLimits := nvidiaCodecSizeLimts[outputs[i].Profile.Encoder]
			resolutionSpecified := outputs[i].Profile.Resolution != ""
			// Sometimes rendition Resolution is not specified. We skip this rendition.
			if haveLimits && resolutionSpecified {
				err := limits.Clamp(&outputs[i].Profile, format)
				if err != nil {
					// add more context to returned error
					p := outputs[i].Profile
					return fmt.Errorf(
						"%w; profile index=%d Resolution=%s FPS=%d/%d bps=%s codec=%d input ac=%s vc=%s w=%d h=%d",
						err, i,
						p.Resolution,
						p.Framerate, p.FramerateDen, // given FramerateDen == 0 is corrected to be 1
						p.Bitrate,
						p.Encoder,
						format.Acodec, format.Vcodec,
						format.Width, format.Height,
					)
				}
			}
		}
	}
	return nil
}

func isAudioAllDrop(ps []TranscodeOptions) bool {
	for _, p := range ps {
		if p.AudioEncoder.Name != "drop" {
			return false
		}
	}
	return true
}

// create C output params array and return it along with corresponding finalizer
// function that makes sure there are no C memory leaks
func createCOutputParams(input *TranscodeOptionsIn, ps []TranscodeOptions) ([]C.output_params, func(), error) {
	params := make([]C.output_params, len(ps))
	finalizer := func() { destroyCOutputParams(params) }
	for i, p := range ps {
		param := p.Profile
		w, h, err := VideoProfileResolution(param)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return params, finalizer, err
			}
		}
		br := strings.Replace(param.Bitrate, "k", "000", 1)
		bitrate, err := strconv.Atoi(br)
		if err != nil {
			if p.VideoEncoder.Name != "drop" && p.VideoEncoder.Name != "copy" {
				return params, finalizer, err
			}
		}
		encoder, scale_filter := p.VideoEncoder.Name, "scale"
		var interpAlgo string
		if encoder == "" {
			encoder, scale_filter, interpAlgo, err = configEncoder(input, p)
			if err != nil {
				return params, finalizer, err
			}
		}
		// preserve aspect ratio along the larger dimension when rescaling
		filters := fmt.Sprintf("%s='w=if(gte(iw,ih),%d,-2):h=if(lt(iw,ih),%d,-2)'", scale_filter, w, h)
		if interpAlgo != "" {
			filters = fmt.Sprintf("%s:interp_algo=%s", filters, interpAlgo)
		}
		if input.Accel == Nvidia && p.Accel == Software {
			// needed for hw dec -> hw rescale -> sw enc
			filters = filters + ",hwdownload,format=nv12"
		}
		if p.Accel == Nvidia && filepath.Ext(input.Fname) == ".png" {
			// If the input is PNG image(s) and we are scaling on a Nvidia device
			// we need to first convert to a pixel format that the scale_npp filter supports
			filters = "format=nv12," + filters
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

		// Set video encoder options
		// TODO understand how h264 profiles and GOP setting works for
		// NETINT encoder, and make sure we change relevant things here
		// Any other options for the encoder can also be added here
		xcoderOutParamsStr := ""
		if len(p.VideoEncoder.Name) <= 0 && len(p.VideoEncoder.Opts) <= 0 {
			p.VideoEncoder.Opts = map[string]string{
				"forced-idr": "1",
				"preset":     "slow",
				"tier":       "high",
			}
			if p.Profile.Quality != 0 {
				if p.Profile.Quality <= 63 {
					p.VideoEncoder.Opts["crf"] = strconv.Itoa(int(p.Profile.Quality))
				} else {
					glog.Warning("Cannot use CRF param, value out of range (0-63)")
				}

				// There's no direct numerical correspondence between CQ and CRF.
				// From some experiments, it seems that setting CQ = CRF + 7 gives similar visual effects.
				cq := p.Profile.Quality + 7
				if cq <= 51 {
					p.VideoEncoder.Opts["cq"] = strconv.Itoa(int(cq))
				} else {
					glog.Warning("Cannot use CQ param, value out of range (0-51)")
				}
			}
			switch p.Profile.Profile {
			case ProfileH264Baseline, ProfileH264ConstrainedHigh:
				if p.Accel != Netint {
					p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
					p.VideoEncoder.Opts["bf"] = "0"
				} else {
					xcoderOutParamsStr = "profile=high:gopPresetIdx=2"
				}
			case ProfileH264Main, ProfileH264High:
				if p.Accel != Netint {
					p.VideoEncoder.Opts["profile"] = ProfileParameters[p.Profile.Profile]
					p.VideoEncoder.Opts["bf"] = "3"
				} else {
					xcoderOutParamsStr = "profile=high"
				}
			case ProfileNone:
				if p.Accel == Nvidia {
					p.VideoEncoder.Opts["bf"] = "0"
				} else {
					p.VideoEncoder.Opts["bf"] = "3"
				}
			default:
				return params, finalizer, ErrTranscoderPrf
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
				return params, finalizer, ErrTranscoderGOP
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
			return params, finalizer, ErrTranscoderFmt
		}

		if muxName != "" {
			muxOpts.name = C.CString(muxName)
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
		oname := C.CString(p.Oname)
		xcoderOutParams := C.CString(xcoderOutParamsStr)
		params[i] = C.output_params{fname: oname, fps: fps,
			w: C.int(w), h: C.int(h), bitrate: C.int(bitrate),
			gop_time: C.int(gopMs), from: C.int(fromMs), to: C.int(toMs),
			muxer: muxOpts, audio: audioOpts, video: vidOpts,
			vfilters: vfilt, sfilters: nil, xcoderParams: xcoderOutParams}
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
		}
	}

	return params, finalizer, nil
}

func destroyCOutputParams(params []C.output_params) {
	for _, p := range params {
		// Note that _all_ memory is relased conditionally. This is because
		// creation process may fail at any point, and so params array may be
		// partially filled
		if p.fname != nil {
			C.free(unsafe.Pointer(p.fname))
		}
		if p.xcoderParams != nil {
			C.free(unsafe.Pointer(p.xcoderParams))
		}
		if p.audio.name != nil {
			C.free(unsafe.Pointer(p.audio.name))
		}
		if p.video.name != nil {
			C.free(unsafe.Pointer(p.video.name))
		}
		if p.vfilters != nil {
			C.free(unsafe.Pointer(p.vfilters))
		}
		if p.muxer.name != nil {
			C.free(unsafe.Pointer(p.muxer.name))
		}
		if p.sfilters != nil {
			C.free(unsafe.Pointer(p.sfilters))
		}

		// dictionaries are freed with special function
		if p.audio.opts != nil {
			C.av_dict_free(&p.audio.opts)
		}
		if p.muxer.opts != nil {
			C.av_dict_free(&p.muxer.opts)
		}
		if p.video.opts != nil {
			C.av_dict_free(&p.video.opts)
		}
	}
}

func hasVideoMetadata(fname string) bool {
	if strings.HasPrefix(strings.ToLower(fname), "pipe:") {
		return false
	}

	fileInfo, err := os.Stat(fname)
	if err != nil {
		return false
	}

	return !fileInfo.IsDir()
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
	var reopendemux bool
	reopendemux = false
	// don't read metadata for inputs without video metadata, because it can't seek back and av_find_input_format in the decoder will fail
	if hasVideoMetadata(input.Fname) {
		status, format, err := GetCodecInfo(input.Fname)
		if err != nil {
			return nil, err
		}
		videoTrackPresent := format.Vcodec != ""
		if status == CodecStatusOk && videoTrackPresent {
			// We don't return error in case status != CodecStatusOk because proper error would be returned later in the logic.
			// Like 'TranscoderInvalidVideo' or `No such file or directory` would be replaced by error we specify here.
			// here we require input size and aspect ratio
			err = ensureEncoderLimits(ps, format)
			if err != nil {
				return nil, err
			}
		}
		if !t.started {
			// NeedsBypass is state where video is present in container & without any frames
			videoMissing := status == CodecStatusNeedsBypass || format.Vcodec == ""
			if videoMissing {
				// Audio-only segment, fail fast right here as we cannot handle them nicely
				return nil, ErrTranscoderVid
			}
			// keep last audio codec
			t.lastacodec = format.Acodec
			// Stream is either OK or completely broken, let the transcoder handle it
			t.started = true
		} else {
			// check if we need to reopen demuxer because added audio in video
			// TODO: fixes like that are needed because handling of cfg change in
			// LPMS is a joke. We need to decide whether LPMS should support full
			// dynamic config one day and either implement it there, or implement
			// some generic workaround for the problem in Go code, such as marking
			// config changes as significant/insignificant and re-creating the instance
			// if the former type change happens
			if format.Acodec != "" && !isAudioAllDrop(ps) {
				if (t.lastacodec == "") || (t.lastacodec != "" && t.lastacodec != format.Acodec) {
					reopendemux = true
					t.lastacodec = format.Acodec
				}
			}
		}
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
			if p.From < 0 || p.To > 0 && p.From > 0 && p.To < p.From {
				glog.Warning("'To' should be after 'From'")
				return nil, ErrTranscoderClipConfig
			}
		}
	}
	if input.Transmuxing {
		t.started = true
	}
	// Output configuration
	params, finalizer, err := createCOutputParams(input, ps)
	// This prevents C memory leaks
	defer finalizer()
	// Only now can we do this
	if err != nil {
		return nil, err
	}

	// Input configuration
	var device *C.char
	if input.Device != "" {
		device = C.CString(input.Device)
		defer C.free(unsafe.Pointer(device))
	}
	fname := C.CString(input.Fname)
	defer C.free(unsafe.Pointer(fname))
	xcoderParams := C.CString("")
	defer C.free(unsafe.Pointer(xcoderParams))

	var demuxerOpts C.component_opts

	ext := filepath.Ext(input.Fname)
	// If the input has an image file extension setup the image2 demuxer
	if ext == ".png" {
		image2 := C.CString("image2")
		defer C.free(unsafe.Pointer(image2))

		demuxerOpts = C.component_opts{
			name: image2,
		}

		if input.Profile.Framerate > 0 {
			if input.Profile.FramerateDen == 0 {
				input.Profile.FramerateDen = 1
			}

			// Do not try to free in this function because in the C code avformat_open_input()
			// will destroy this
			demuxerOpts.opts = newAVOpts(map[string]string{
				"framerate": fmt.Sprintf("%d/%d", input.Profile.Framerate, input.Profile.FramerateDen),
			})
		}
	}

	inp := &C.input_params{fname: fname, hw_type: hw_type, device: device, xcoderParams: xcoderParams,
		handle: t.handle, demuxer: demuxerOpts}
	if input.Transmuxing {
		inp.transmuxing = 1
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
	if reopendemux {
		// forcefully close and open demuxer
		ret := int(C.lpms_transcode_reopen_demux(inp))
		if ret != 0 {
			if LogTranscodeErrors {
				glog.Error("Reopen demux returned : ", ErrorMap[ret])
			}
			return nil, ErrorMap[ret]
		}
	}

	ret := int(C.lpms_transcode(inp, paramsPointer, resultsPointer, C.int(len(params)), decoded))
	if ret != 0 {
		if LogTranscodeErrors {
			glog.Error("Transcoder Return : ", ErrorMap[ret])
		}
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

func hwScale() string {
	if runtime.GOOS == "windows" {
		// we don't build windows binaries with CUDA SDK, so need to use scale_cuda instead of scale_npp
		return "scale_cuda"
	} else {
		return "scale_npp"
	}
}

func hwScaleAlgo() string {
	if runtime.GOOS == "windows" {
		// we don't build windows binaries with CUDA SDK, so need to use the default scale algorithm
		return ""
	} else {
		return "super"
	}
}

func FfmpegSetLogLevel(level int) {
	C.av_log_set_level(C.int(level))
}

func FfmpegGetLogLevel() int {
	return int(C.av_log_get_level())
}
