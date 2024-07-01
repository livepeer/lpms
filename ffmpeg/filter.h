#ifndef _LPMS_FILTER_H_
#define _LPMS_FILTER_H_

#include <libavfilter/avfilter.h>
#include "decoder.h"

struct filter_ctx {
  int active;
  AVFilterGraph *graph;
  AVFrame *frame;
  AVFilterContext *sink_ctx;
  AVFilterContext *src_ctx;

  uint8_t *hwframes; // GPU frame pool data

  // Input timebase for this filter
  AVRational time_base;

  // The fps filter expects monotonically increasing PTS, which might not hold
  // for our input segments (they may be out of order, or have dropped frames).
  // So we set a custom PTS before sending the frame to the filtergraph that is
  // uniformly and monotonically increasing.
  int64_t custom_pts;

  // Previous PTS to be used to manually calculate duration for custom_pts
  int64_t prev_frame_pts;

  // Count of complete segments that have been processed by this filtergraph
  int segments_complete;

  // We need to update the post-filtergraph PTS before sending the frame for
  // encoding because we modified the input PTS.
  // We do this by calculating the difference between our custom PTS and actual
  // PTS for the first-frame of every segment, and then applying this diff to
  // every subsequent frame in the segment.
  int64_t pts_diff;

  // When draining the filtergraph, we inject fake frames.
  // These frames have monotonically increasing timestamps at the same interval
  // as a normal stream of frames. The custom_pts is set to more than usual jump
  // when we have a small segment and haven't encoded anything yet but need to
  // flush the filtergraph.
  // We mark this boolean as flushed when done flushing.
  int flushed;
  int flushing;
};

struct output_ctx {
  char *fname;         // required output file name
  char *vfilters;      // required output video filters
  char *sfilters;      // required output signature filters
  int width, height, bitrate; // w, h, br required
  AVRational fps;
  AVFormatContext *oc; // muxer required
  AVCodecContext  *vc; // video decoder optional
  AVCodecContext  *ac; // audo  decoder optional
  int vi, ai; // video and audio stream indices
  int dv, da; // flags whether to drop video or audio
  struct filter_ctx vf, af, sf;

  // Optional hardware encoding support
  enum AVHWDeviceType hw_type;

  // muxer and encoder information (name + options)
  component_opts *muxer;
  component_opts *video;
  component_opts *audio;

  int64_t drop_ts;     // preroll audio ts to drop

  int64_t last_audio_dts;     //dts of the last audio packet sent to the muxer

  int64_t last_video_dts;     //dts of the last video packet sent to the muxer

  int64_t gop_time, gop_pts_len, next_kf_pts; // for gop reset

  int64_t clip_from, clip_to, clip_from_pts, clip_to_pts, clip_started, clip_start_pts, clip_start_pts_found; // for clipping
  int64_t clip_audio_from_pts, clip_audio_to_pts, clip_audio_start_pts, clip_audio_start_pts_found; // for clipping

  output_results  *res; // data to return for this output
  char *xcoderParams;
};

int init_video_filters(struct input_ctx *ictx, struct output_ctx *octx);
int init_audio_filters(struct input_ctx *ictx, struct output_ctx *octx);
int init_signature_filters(struct output_ctx *octx, AVFrame *inf);
int filtergraph_write(AVFrame *inf, struct input_ctx *ictx, struct output_ctx *octx, struct filter_ctx *filter, int is_video);
int filtergraph_read(struct input_ctx *ictx, struct output_ctx *octx, struct filter_ctx *filter, int is_video);
void free_filter(struct filter_ctx *filter);

// UTILS
static inline int is_copy(char *encoder) {
  return encoder && !strcmp("copy", encoder);
}

static inline int is_drop(char *encoder) {
  return !encoder || !strcmp("drop", encoder) || !strcmp("", encoder);
}

static inline int needs_decoder(char *encoder) {
  // Checks whether the given "encoder" depends on having a decoder.
  // Do this by enumerating special cases that do *not* need encoding
  return !(is_copy(encoder) || is_drop(encoder));
}


#endif // _LPMS_FILTER_H_
