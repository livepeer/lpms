#ifndef _LPMS_DECODER_H_
#define _LPMS_DECODER_H_

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include <libavutil/opt.h>
#include "transcoder.h"

struct input_ctx {
  AVFormatContext *ic; // demuxer required
  AVCodecContext  *vc; // video decoder optional
  AVCodecContext  *ac; // audo  decoder optional
  int vi, ai; // video and audio stream indices
  int dv, da; // flags whether to drop video or audio

  // Hardware decoding support
  AVBufferRef *hw_device_ctx;
  enum AVHWDeviceType hw_type;
  char *device;
  char *xcoderParams;

  // Decoder flush
  AVPacket *first_pkt;
  int flushed;
  int flushing;
  // The diff of `packets sent - frames recv` serves as an estimate of
  // internally buffered packets by the decoder.  We're done flushing when this
  // becomes 0.
  uint16_t pkt_diff;
  // We maintain a count of sentinel packets sent without receiving any
  // valid frames back, and stop flushing if it crosses SENTINEL_MAX.
  // FIXME This is needed due to issue #155 - input/output frame mismatch.
#define SENTINEL_MAX 5
  uint16_t sentinel_count;

  // Filter flush
  AVFrame *last_frame_v, *last_frame_a;

  // transmuxing specific fields:
  // last non-zero duration
  int64_t last_duration[MAX_OUTPUT_SIZE];
  // keep track of last dts in each stream.
  // used while transmuxing, to skip packets with invalid dts.
  int64_t last_dts[MAX_OUTPUT_SIZE];
  //
  int64_t dts_diff[MAX_OUTPUT_SIZE];
  //
  int discontinuity[MAX_OUTPUT_SIZE];
  // Transmuxing mode. Close output in lpms_transcode_stop instead of
  // at the end of lpms_transcode call.
  int transmuxing;
  // In HW transcoding, demuxer is opened once and used,
  // so it is necessary to check whether the input pixel format does not change in the middle.
  enum AVPixelFormat last_format;
};

enum FreeInputPolicy {
  FORCE_CLOSE_HW_DECODER,
  PRESERVE_HW_DECODER
};

// Exported methods
int demux_in(struct input_ctx *ictx, AVPacket *pkt);
int decode_in(struct input_ctx *ictx, AVPacket *pkt, AVFrame *frame, int *stream_index);
int flush_in(struct input_ctx *ictx, AVFrame *frame, int *stream_index);
int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt, int *stream_index);
int open_input(input_params *params, struct input_ctx *ctx, ReadContext *rctx);
void free_input(struct input_ctx *inctx, enum FreeInputPolicy policy);
// this should perhaps be moved to some utility file, as it is not decoder
// specific
enum AVPixelFormat hw2pixfmt(AVCodecContext *ctx);

// Utility functions
static inline int is_flush_frame(AVFrame *frame)
{
  return -1 == frame->pts;
}

#endif // _LPMS_DECODER_H_
