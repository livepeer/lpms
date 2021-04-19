#ifndef _LPMS_DECODER_H_
#define _LPMS_DECODER_H_

#include <libavformat/avformat.h>
#include <libavcodec/avcodec.h>
#include "transcoder.h"

#define MAX_CHUNK_CNT 10
#define MAX_DFRAME_CNT 1000
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
};

// struct decode_thread {
//   int initialized;
//   int gpuid;
//   int chunkcnt;
//   struct input_ctx ictx;
//   struct dframemeta *dec_chunk[MAX_CHUNK_CNT]; 
// };

struct decode_thread* lpms_decode_new();
void lpms_decode_stop(struct decode_thread* handle);


// Exported methods
int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt);
enum AVPixelFormat hw2pixfmt(AVCodecContext *ctx);
int open_input(input_params *params, struct input_ctx *ctx);
int open_video_decoder(input_params *params, struct input_ctx *ctx);
int open_audio_decoder(input_params *params, struct input_ctx *ctx);
void free_input(struct input_ctx *inctx);

int lpms_decode(input_params *inp,  output_results *decoded_results, dframe_buffer *dframe_buf, struct input_ctx *ictx, struct decode_meta *dmeta);
// Utility functions
inline int is_flush_frame(AVFrame *frame)
{
  return -1 == frame->pts;
}
void set_ictx(struct transcode_thread *h, struct input_ctx *ictx);
#endif // _LPMS_DECODER_H_
