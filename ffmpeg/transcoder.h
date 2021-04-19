#ifndef _LPMS_TRANSCODER_H_
#define _LPMS_TRANSCODER_H_

#include <libavutil/hwcontext.h>
#include <libavutil/rational.h>
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>

// LPMS specific errors
extern const int lpms_ERR_INPUT_PIXFMT;
extern const int lpms_ERR_INPUT_CODEC;
extern const int lpms_ERR_FILTERS;
extern const int lpms_ERR_PACKET_ONLY;
extern const int lpms_ERR_FILTER_FLUSHED;
extern const int lpms_ERR_OUTPUTS;
extern const int lpms_ERR_DTS;

struct transcode_thread;

typedef struct {
    char *name;
    AVDictionary *opts;
} component_opts;

typedef struct {
  char *fname;
  char *vfilters;
  int w, h, bitrate, gop_time;
  AVRational fps;

  component_opts muxer;
  component_opts audio;
  component_opts video;

} output_params;

typedef struct {
  AVFrame *dec_frame;
  int has_frame;
  AVPacket in_pkt;
} dframemeta;

typedef struct {
    int cnt;
    dframemeta *dframes;
} dframe_buffer;

typedef struct {
  char *fname;
  dframe_buffer *dframe_buffer;
  // Handle to a transcode thread.
  // If null, a new transcode thread is allocated.
  // The transcode thread is returned within `output_results`.
  // Must be freed with lpms_transcode_stop.
  struct transcode_thread *handle;

  // temporary addition of decode handler, revisit and clean after functional
  struct transcode_thread *dec_handle; 
  // Optional hardware acceleration
  enum AVHWDeviceType hw_type;
  char *device;
} input_params;

typedef struct {
    int frames;
    int64_t pixels;
} output_results;

struct decode_meta{
    int v_width;
    int v_height;
    int vi;
    int ai;
    enum AVPixelFormat in_pix_fmt;
    enum AVHWDeviceType hw_type;
    AVRational time_base;
    AVRational sample_aspect_ratio;
    AVRational framerate;
    AVRational r_frame_rate;
    AVBufferRef *hw_frames_ctx;
    AVFrame *last_frame_v;
    AVFrame *last_frame_a;
};
enum LPMSLogLevel {
  LPMS_LOG_TRACE    = AV_LOG_TRACE,
  LPMS_LOG_DEBUG    = AV_LOG_DEBUG,
  LPMS_LOG_VERBOSE  = AV_LOG_VERBOSE,
  LPMS_LOG_INFO     = AV_LOG_INFO,
  LPMS_LOG_WARNING  = AV_LOG_WARNING,
  LPMS_LOG_ERROR    = AV_LOG_ERROR,
  LPMS_LOG_FATAL    = AV_LOG_FATAL,
  LPMS_LOG_PANIC    = AV_LOG_PANIC,
  LPMS_LOG_QUIET    = AV_LOG_QUIET
};

void lpms_init(enum LPMSLogLevel max_level);
int  lpms_transcode(input_params *inp, output_params *params, output_results *results, int nb_outputs, output_results *decoded_results);
struct transcode_thread* lpms_transcode_new();
void lpms_transcode_stop(struct transcode_thread* handle);
int lpms_encode(input_params *inp, dframe_buffer *dframe_buffer, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results, struct decode_meta *dmeta);
int lpms_encode1(input_params *inp, dframe_buffer *dframe_buffer, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results, struct decode_meta *dmeta);
int deep_copy_avframe(AVFrame *dest, AVFrame *src);
struct decode_meta* alloc_decode_meta();
void free_decode_meta(struct decode_meta* dmeta);
// struct decode_thread* lpms_decode_new();
// void lpms_decode_stop(struct decode_thread* handle);
// void set_ictx(struct transcode_thread *h, input_ctx *ictx);
// int lpms_decode(input_params *inp,  output_results *decoded_results, dframe_buffer *dframe_buf, struct input_ctx *ictx);
#endif // _LPMS_TRANSCODER_H_
