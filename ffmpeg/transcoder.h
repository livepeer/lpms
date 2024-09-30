#ifndef _LPMS_TRANSCODER_H_
#define _LPMS_TRANSCODER_H_

#include <libavutil/hwcontext.h>
#include <libavutil/rational.h>
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include "logging.h"

// LPMS specific errors
extern const int lpms_ERR_INPUT_PIXFMT;
extern const int lpms_ERR_INPUT_CODEC;
extern const int lpms_ERR_INPUT_NOKF;
extern const int lpms_ERR_FILTERS;
extern const int lpms_ERR_PACKET_ONLY;
extern const int lpms_ERR_FILTER_FLUSHED;
extern const int lpms_ERR_OUTPUTS;
extern const int lpms_ERR_UNRECOVERABLE;

struct transcode_thread;

typedef struct {
    char *name;
    AVDictionary *opts;
} component_opts;

typedef struct {
  char *fname;
  char *vfilters;
  char *sfilters;
  int w, h, bitrate, gop_time, from, to;
  AVRational fps;
  char *xcoderParams;
  component_opts muxer;
  component_opts audio;
  component_opts video;
  AVDictionary *metadata;
} output_params;

typedef struct {
  char *fname;

  // Handle to a transcode thread.
  // If null, a new transcode thread is allocated.
  // The transcode thread is returned within `output_results`.
  // Must be freed with lpms_transcode_stop.
  struct transcode_thread *handle;

  // Optional hardware acceleration
  enum AVHWDeviceType hw_type;
  char *device;
  char *xcoderParams;

  // Optional demuxer opts
  component_opts demuxer;
  // Optional video decoder + opts
  component_opts video;

  // concatenates multiple inputs into the same output
  int transmuxing;
} input_params;

#define MAX_CLASSIFY_SIZE 10
#define MAX_OUTPUT_SIZE 10

typedef struct {
    int frames;
    int64_t pixels;
} output_results;

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
int lpms_transcode(input_params *inp, output_params *params, output_results *results, int nb_outputs, output_results *decoded_results);
int lpms_transcode_reopen_demux(input_params *inp);
struct transcode_thread* lpms_transcode_new();
void lpms_transcode_stop(struct transcode_thread* handle);
void lpms_transcode_discontinuity(struct transcode_thread *handle);

#endif // _LPMS_TRANSCODER_H_
