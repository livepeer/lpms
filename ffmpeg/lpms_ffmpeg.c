#include "lpms_ffmpeg.h"

#include <libavcodec/avcodec.h>

#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavfilter/buffersrc.h>
#include <libavutil/opt.h>
#include <libavutil/pixdesc.h>

#include <pthread.h>

// Not great to appropriate internal API like this...
const int lpms_ERR_INPUT_PIXFMT = FFERRTAG('I','N','P','X');
const int lpms_ERR_FILTERS = FFERRTAG('F','L','T','R');
const int lpms_ERR_PACKET_ONLY = FFERRTAG('P','K','O','N');
const int lpms_ERR_OUTPUTS = FFERRTAG('O','U','T','P');

//
// Internal transcoder data structures
//
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

  int64_t next_pts_a, next_pts_v;

  // Decoder flush
  AVPacket *first_pkt;
  int flushed;
};

struct filter_ctx {
  int active;
  AVFilterGraph *graph;
  AVFrame *frame;
  AVFilterContext *sink_ctx;
  AVFilterContext *src_ctx;

  uint8_t *hwframes; // GPU frame pool data
};

struct output_ctx {
  char *fname;         // required output file name
  char *vfilters;      // required output video filters
  int width, height, bitrate; // w, h, br required
  AVRational fps;
  AVFormatContext *oc; // muxer required
  AVCodecContext  *vc; // video decoder optional
  AVCodecContext  *ac; // audo  decoder optional
  int vi, ai; // video and audio stream indices
  int dv, da; // flags whether to drop video or audio
  struct filter_ctx vf, af;

  // Optional hardware encoding support
  enum AVHWDeviceType hw_type;

  // muxer and encoder information (name + options)
  component_opts *muxer;
  component_opts *video;
  component_opts *audio;

  int64_t drop_ts;     // preroll audio ts to drop

  output_results  *res; // data to return for this output

};

#define MAX_OUTPUT_SIZE 10

struct transcode_thread {
  int initialized;

  struct input_ctx ictx;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];

  int nb_outputs;

};

void lpms_init()
{
  av_log_set_level(AV_LOG_WARNING);
}

//
// Segmenter
//

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char* seg_time, char *seg_start)
{
#define r2h_err(str) {\
  if (!ret) ret = 1; \
  errstr = str; \
  goto handle_r2h_err; \
}
  char *errstr          = NULL;
  int ret               = 0;
  AVFormatContext *ic   = NULL;
  AVFormatContext *oc   = NULL;
  AVOutputFormat *ofmt  = NULL;
  AVStream *ist         = NULL;
  AVStream *ost         = NULL;
  AVDictionary *md      = NULL;
  AVCodec *codec        = NULL;
  int64_t prev_ts[2]    = {AV_NOPTS_VALUE, AV_NOPTS_VALUE};
  int stream_map[2]     = {-1, -1};
  int got_video_kf      = 0;
  AVPacket pkt;

  ret = avformat_open_input(&ic, listen, NULL, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to open input\n");
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to find any input streams\n");

  ofmt = av_guess_format(NULL, outf, NULL);
  if (!ofmt) r2h_err("Could not deduce output format from file extension\n");
  ret = avformat_alloc_output_context2(&oc, ofmt, NULL, outf);
  if (ret < 0) r2h_err("Unable to allocate output context\n");

  // XXX accommodate cases where audio or video is empty
  stream_map[0] = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (stream_map[0] < 0) r2h_err("segmenter: Unable to find video stream\n");
  stream_map[1] = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
  if (stream_map[1] < 0) r2h_err("segmenter: Unable to find audio stream\n");

  ist = ic->streams[stream_map[0]];
  ost = avformat_new_stream(oc, NULL);
  if (!ost) r2h_err("segmenter: Unable to allocate output video stream\n");
  avcodec_parameters_copy(ost->codecpar, ist->codecpar);
  ist = ic->streams[stream_map[1]];
  ost = avformat_new_stream(oc, NULL);
  if (!ost) r2h_err("segmenter: Unable to allocate output audio stream\n");
  avcodec_parameters_copy(ost->codecpar, ist->codecpar);

  av_dict_set(&md, "hls_time", seg_time, 0);
  av_dict_set(&md, "hls_segment_filename", ts_tmpl, 0);
  av_dict_set(&md, "start_number", seg_start, 0);
  av_dict_set(&md, "hls_flags", "delete_segments", 0);
  ret = avformat_write_header(oc, &md);
  if (ret < 0) r2h_err("Error writing header\n");

  av_init_packet(&pkt);
  while (1) {
    ret = av_read_frame(ic, &pkt);
    if (ret == AVERROR_EOF) {
      av_interleaved_write_frame(oc, NULL); // flush
      break;
    } else if (ret < 0) r2h_err("Error reading\n");
    // rescale timestamps
    if (pkt.stream_index == stream_map[0]) pkt.stream_index = 0;
    else if (pkt.stream_index == stream_map[1]) pkt.stream_index = 1;
    else goto r2hloop_end;
    ist = ic->streams[stream_map[pkt.stream_index]];
    ost = oc->streams[pkt.stream_index];
    int64_t dts_next = pkt.dts, dts_prev = prev_ts[pkt.stream_index];
    if (oc->streams[pkt.stream_index]->codecpar->codec_type == AVMEDIA_TYPE_VIDEO &&
        AV_NOPTS_VALUE == dts_prev &&
        (pkt.flags & AV_PKT_FLAG_KEY)) got_video_kf = 1;
    if (!got_video_kf) goto r2hloop_end; // skip everyting until first video KF
    if (AV_NOPTS_VALUE == dts_prev) dts_prev = dts_next;
    else if (dts_next <= dts_prev) goto r2hloop_end; // drop late packets
    pkt.pts = av_rescale_q_rnd(pkt.pts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    pkt.dts = av_rescale_q_rnd(pkt.dts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    if (!pkt.duration) pkt.duration = dts_next - dts_prev;
    pkt.duration = av_rescale_q(pkt.duration, ist->time_base, ost->time_base);
    prev_ts[pkt.stream_index] = dts_next;
    // write the thing
    ret = av_interleaved_write_frame(oc, &pkt);
    if (ret < 0) r2h_err("segmenter: Unable to write output frame\n");
r2hloop_end:
    av_packet_unref(&pkt);
  }
  ret = av_write_trailer(oc);
  if (ret < 0) r2h_err("segmenter: Unable to write trailer\n");

handle_r2h_err:
  if (errstr) fprintf(stderr, "%s", errstr);
  if (ic) avformat_close_input(&ic);
  if (oc) avformat_free_context(oc);
  if (md) av_dict_free(&md);
  return ret == AVERROR_EOF ? 0 : ret;
}

//
// Transcoder
//

static void free_filter(struct filter_ctx *filter)
{
  if (filter->frame) av_frame_free(&filter->frame);
  if (filter->graph) avfilter_graph_free(&filter->graph);
}

static void free_output(struct output_ctx *octx)
{
  if (octx->oc) {
    if (!(octx->oc->oformat->flags & AVFMT_NOFILE) && octx->oc->pb) {
      avio_closep(&octx->oc->pb);
    }
    avformat_free_context(octx->oc);
    octx->oc = NULL;
  }
  if (octx->vc && AV_HWDEVICE_TYPE_NONE == octx->hw_type) avcodec_free_context(&octx->vc);
  if (octx->ac) avcodec_free_context(&octx->ac);
  free_filter(&octx->vf);
  free_filter(&octx->af);
}

static int is_copy(char *encoder) {
  return encoder && !strcmp("copy", encoder);
}

static int is_drop(char *encoder) {
  return !encoder || !strcmp("drop", encoder) || !strcmp("", encoder);
}

static int needs_decoder(char *encoder) {
  // Checks whether the given "encoder" depends on having a decoder.
  // Do this by enumerating special cases that do *not* need encoding
  return !(is_copy(encoder) || is_drop(encoder));
}

static int is_flush_frame(AVFrame *frame)
{
  return -1 == frame->pts;
}

static void send_first_pkt(struct input_ctx *ictx)
{
  if (ictx->flushed || !ictx->first_pkt) return;

  int ret = avcodec_send_packet(ictx->vc, ictx->first_pkt);
  if (ret < 0) {
    char errstr[AV_ERROR_MAX_STRING_SIZE];
    av_strerror(ret, errstr, sizeof errstr);
    fprintf(stderr, "Error sending flush packet : %s\n", errstr);
  }
}

static enum AVPixelFormat hw2pixfmt(AVCodecContext *ctx)
{
  const AVCodec *decoder = ctx->codec;
  struct input_ctx *params = (struct input_ctx*)ctx->opaque;
  for (int i = 0;; i++) {
    const AVCodecHWConfig *config = avcodec_get_hw_config(decoder, i);
    if (!config) {
      fprintf(stderr, "Decoder %s does not support hw decoding\n", decoder->name);
      return AV_PIX_FMT_NONE;
    }
    if (config->methods & AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX &&
        config->device_type == params->hw_type) {
      return  config->pix_fmt;
    }
  }
  return AV_PIX_FMT_NONE;
}

static int set_vaapi_hwframe_ctx(AVCodecContext *ctx, AVBufferRef *hw_device_ctx)
{
  AVBufferRef *hw_frames_ref;
  AVHWFramesContext *frames_ctx = NULL;
  int err = 0;

  if (!(hw_frames_ref = av_hwframe_ctx_alloc(hw_device_ctx))) {
    fprintf(stderr, "Failed to create VAAPI frame context.\n");
    return -1;
  }
  frames_ctx = (AVHWFramesContext *)(hw_frames_ref->data);
  frames_ctx->format    = AV_PIX_FMT_VAAPI;
  frames_ctx->sw_format = AV_PIX_FMT_NV12;
  frames_ctx->width     = ctx->width;
  frames_ctx->height    = ctx->height;
  frames_ctx->initial_pool_size = 20;
  if ((err = av_hwframe_ctx_init(hw_frames_ref)) < 0) {
    fprintf(stderr, "Failed to initialize VAAPI frame context."
            "Error code: %s\n",av_err2str(err));
    av_buffer_unref(&hw_frames_ref);
    return err;
  }
  ctx->hw_frames_ctx = av_buffer_ref(hw_frames_ref);
  if (!ctx->hw_frames_ctx)
    err = AVERROR(ENOMEM);

  av_buffer_unref(&hw_frames_ref);
  return err;
}

static enum AVPixelFormat get_hw_pixfmt(AVCodecContext *vc, const enum AVPixelFormat *pix_fmts)
{
  AVHWFramesContext *frames;
  int ret;

  // XXX Ideally this would be auto initialized by the HW device ctx
  //     However the initialization doesn't occur in time to set up filters
  //     So we do it here. Also see avcodec_get_hw_frames_parameters
  av_buffer_unref(&vc->hw_frames_ctx);
  vc->hw_frames_ctx = av_hwframe_ctx_alloc(vc->hw_device_ctx);
  if (!vc->hw_frames_ctx) {
    fprintf(stderr, "Unable to allocate hwframe context for decoding\n");
    return AV_PIX_FMT_NONE;
  }

  frames = (AVHWFramesContext*)vc->hw_frames_ctx->data;
  frames->format = hw2pixfmt(vc);
  frames->sw_format = vc->sw_pix_fmt;
  frames->width = vc->width;
  frames->height = vc->height;

  // May want to allocate extra HW frames if we encounter samples where
  // the defaults are insufficient. Raising this increases GPU memory usage
  // For now, the defaults seems OK.
  //vc->extra_hw_frames = 16 + 1; // H.264 max refs

  ret = av_hwframe_ctx_init(vc->hw_frames_ctx);
  if (AVERROR(ENOSYS) == ret) ret = lpms_ERR_INPUT_PIXFMT; // most likely
  if (ret < 0) {
    fprintf(stderr,"Unable to initialize a hardware frame pool\n");
    return AV_PIX_FMT_NONE;
  }

/*
fprintf(stderr, "selected format: hw %s sw %s\n",
av_get_pix_fmt_name(frames->format), av_get_pix_fmt_name(frames->sw_format));
const enum AVPixelFormat *p;
for (p = pix_fmts; *p != -1; p++) {
fprintf(stderr,"possible format: %s\n", av_get_pix_fmt_name(*p));
}
*/

  return frames->format;
}

static enum AVPixelFormat get_vaapi_format(AVCodecContext *ctx, const enum AVPixelFormat *pix_fmts)
{
  const enum AVPixelFormat *p;

  for (p = pix_fmts; *p != AV_PIX_FMT_NONE; p++) {
    if (*p == AV_PIX_FMT_VAAPI)
      return *p;
  }

  fprintf(stderr, "Unable to decode this file using VA-API.\n");
  return AV_PIX_FMT_NONE;
}

static int init_video_filters(struct input_ctx *ictx, struct output_ctx *octx)
{
#define filters_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto init_video_filters_cleanup; \
}
    char args[512];
    int ret = 0;
    const AVFilter *buffersrc  = avfilter_get_by_name("buffer");
    const AVFilter *buffersink = avfilter_get_by_name("buffersink");
    AVFilterInOut *outputs = avfilter_inout_alloc();
    AVFilterInOut *inputs  = avfilter_inout_alloc();
    AVRational time_base = ictx->ic->streams[ictx->vi]->time_base;
    enum AVPixelFormat pix_fmts[] = { AV_PIX_FMT_YUV420P, AV_PIX_FMT_CUDA, AV_PIX_FMT_VAAPI, AV_PIX_FMT_NONE }; // XXX ensure the encoder allows this
    struct filter_ctx *vf = &octx->vf;
    char *filters_descr = octx->vfilters;
    enum AVPixelFormat in_pix_fmt = ictx->vc->pix_fmt;

    if (!needs_decoder(octx->video->name)) return 0; // no need for filters

    vf->graph = avfilter_graph_alloc();
    if (!outputs || !inputs || !vf->graph) {
      ret = AVERROR(ENOMEM);
      filters_err("Unble to allocate filters\n");
    }
    if (ictx->vc->hw_device_ctx) in_pix_fmt = hw2pixfmt(ictx->vc);

    /* buffer video source: the decoded frames from the decoder will be inserted here. */
    snprintf(args, sizeof args,
            "video_size=%dx%d:pix_fmt=%d:time_base=%d/%d:pixel_aspect=%d/%d",
            ictx->vc->width, ictx->vc->height, in_pix_fmt,
            time_base.num, time_base.den,
            ictx->vc->sample_aspect_ratio.num, ictx->vc->sample_aspect_ratio.den);

    ret = avfilter_graph_create_filter(&vf->src_ctx, buffersrc,
                                       "in", args, NULL, vf->graph);
    if (ret < 0) filters_err("Cannot create video buffer source\n");
    if (ictx->vc && ictx->vc->hw_frames_ctx) {
      // XXX a bit problematic in that it's set before decoder is fully ready
      AVBufferSrcParameters *srcpar = av_buffersrc_parameters_alloc();
      srcpar->hw_frames_ctx = ictx->vc->hw_frames_ctx;
      vf->hwframes = ictx->vc->hw_frames_ctx->data;
      av_buffersrc_parameters_set(vf->src_ctx, srcpar);
      av_freep(&srcpar);
    }

    /* buffer video sink: to terminate the filter chain. */
    ret = avfilter_graph_create_filter(&vf->sink_ctx, buffersink,
                                       "out", NULL, NULL, vf->graph);
    if (ret < 0) filters_err("Cannot create video buffer sink\n");

    ret = av_opt_set_int_list(vf->sink_ctx, "pix_fmts", pix_fmts,
                              AV_PIX_FMT_NONE, AV_OPT_SEARCH_CHILDREN);
    if (ret < 0) filters_err("Cannot set output pixel format\n");

    /*
     * Set the endpoints for the filter graph. The filter_graph will
     * be linked to the graph described by filters_descr.
     */

    /*
     * The buffer source output must be connected to the input pad of
     * the first filter described by filters_descr; since the first
     * filter input label is not specified, it is set to "in" by
     * default.
     */
    outputs->name       = av_strdup("in");
    outputs->filter_ctx = vf->src_ctx;
    outputs->pad_idx    = 0;
    outputs->next       = NULL;

    /*
     * The buffer sink input must be connected to the output pad of
     * the last filter described by filters_descr; since the last
     * filter output label is not specified, it is set to "out" by
     * default.
     */
    inputs->name       = av_strdup("out");
    inputs->filter_ctx = vf->sink_ctx;
    inputs->pad_idx    = 0;
    inputs->next       = NULL;

    ret = avfilter_graph_parse_ptr(vf->graph, filters_descr,
                                    &inputs, &outputs, NULL);
    if (ret < 0) filters_err("Unable to parse video filters desc\n");

    ret = avfilter_graph_config(vf->graph, NULL);
    if (ret < 0) filters_err("Unable configure video filtergraph\n");

    vf->frame = av_frame_alloc();
    if (!vf->frame) filters_err("Unable to allocate video frame\n");

init_video_filters_cleanup:
    avfilter_inout_free(&inputs);
    avfilter_inout_free(&outputs);

    vf->active = !ret;
    return ret;
#undef filters_err
}


static int init_audio_filters(struct input_ctx *ictx, struct output_ctx *octx)
{
#define af_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto init_audio_filters_cleanup; \
}
  int ret = 0;
  char args[512];
  char filters_descr[256];
  const AVFilter *buffersrc  = avfilter_get_by_name("abuffer");
  const AVFilter *buffersink = avfilter_get_by_name("abuffersink");
  AVFilterInOut *outputs = avfilter_inout_alloc();
  AVFilterInOut *inputs  = avfilter_inout_alloc();
  struct filter_ctx *af = &octx->af;
  AVRational time_base = ictx->ic->streams[ictx->ai]->time_base;


  af->graph = avfilter_graph_alloc();

  if (!outputs || !inputs || !af->graph) {
    ret = AVERROR(ENOMEM);
    af_err("Unble to allocate audio filters\n");
  }

  /* buffer audio source: the decoded frames from the decoder will be inserted here. */
  snprintf(args, sizeof args,
      "sample_rate=%d:sample_fmt=%d:channel_layout=0x%"PRIx64":channels=%d:"
      "time_base=%d/%d",
      ictx->ac->sample_rate, ictx->ac->sample_fmt, ictx->ac->channel_layout,
      ictx->ac->channels, time_base.num, time_base.den);

  // TODO set sample format and rate based on encoder support,
  //      rather than hardcoding
  snprintf(filters_descr, sizeof filters_descr,
    "aformat=sample_fmts=fltp:channel_layouts=stereo:sample_rates=44100");

  ret = avfilter_graph_create_filter(&af->src_ctx, buffersrc,
                                     "in", args, NULL, af->graph);
  if (ret < 0) af_err("Cannot create audio buffer source\n");

  /* buffer audio sink: to terminate the filter chain. */
  ret = avfilter_graph_create_filter(&af->sink_ctx, buffersink,
                                     "out", NULL, NULL, af->graph);
  if (ret < 0) af_err("Cannot create audio buffer sink\n");

  /*
   * Set the endpoints for the filter graph. The filter_graph will
   * be linked to the graph described by filters_descr.
   */

  /*
   * The buffer source output must be connected to the input pad of
   * the first filter described by filters_descr; since the first
   * filter input label is not specified, it is set to "in" by
   * default.
   */
  outputs->name       = av_strdup("in");
  outputs->filter_ctx = af->src_ctx;
  outputs->pad_idx    = 0;
  outputs->next       = NULL;

  /*
   * The buffer sink input must be connected to the output pad of
   * the last filter described by filters_descr; since the last
   * filter output label is not specified, it is set to "out" by
   * default.
   */
  inputs->name       = av_strdup("out");
  inputs->filter_ctx = af->sink_ctx;
  inputs->pad_idx    = 0;
  inputs->next       = NULL;

  ret = avfilter_graph_parse_ptr(af->graph, filters_descr,
                                &inputs, &outputs, NULL);
  if (ret < 0) af_err("Unable to parse audio filters desc\n");

  ret = avfilter_graph_config(af->graph, NULL);
  if (ret < 0) af_err("Unable configure audio filtergraph\n");

  af->frame = av_frame_alloc();
  if (!af->frame) af_err("Unable to allocate audio frame\n");

init_audio_filters_cleanup:
  avfilter_inout_free(&inputs);
  avfilter_inout_free(&outputs);

  af->active = !ret;
  return ret;
#undef af_err
}


static int add_video_stream(struct output_ctx *octx, struct input_ctx *ictx)
{
#define vs_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, "Error adding video stream: " msg); \
  goto add_video_err; \
}

  // video stream to muxer
  int ret = 0;
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) vs_err("Unable to alloc video stream\n");
  octx->vi = st->index;
  st->avg_frame_rate = octx->fps;
  if (is_copy(octx->video->name)) {
    AVStream *ist = ictx->ic->streams[ictx->vi];
    if (ictx->vi < 0 || !ist) vs_err("Input video stream does not exist\n");
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0) vs_err("Error copying video params from input stream\n");
    // Sometimes the codec tag is wonky for some reason, so correct it
    ret = av_codec_get_tag2(octx->oc->oformat->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
    avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist, AVFMT_TBCF_DEMUXER);
  } else if (octx->vc) {
    st->time_base = octx->vc->time_base;
    ret = avcodec_parameters_from_context(st->codecpar, octx->vc);
    if (ret < 0) vs_err("Error setting video params from encoder\n");
  } else vs_err("No video encoder, not a copy; what is this?\n");
  return 0;

add_video_err:
  // XXX free anything here?
  return ret;
#undef vs_err
}

static int add_audio_stream(struct input_ctx *ictx, struct output_ctx *octx)
{
#define as_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, "Error adding audio stream: " msg); \
  goto add_audio_err; \
}

  if (ictx->ai < 0 || octx->da) {
    // Don't need to add an audio stream if no input audio exists,
    // or we're dropping the output audio stream
    return 0;
  }

  // audio stream to muxer
  int ret = 0;
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) as_err("Unable to alloc audio stream\n");
  if (is_copy(octx->audio->name)) {
    AVStream *ist = ictx->ic->streams[ictx->ai];
    if (ictx->ai < 0 || !ist) as_err("Input audio stream does not exist\n");
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0) as_err("Error copying audio params from input stream\n");
    // Sometimes the codec tag is wonky for some reason, so correct it
    ret = av_codec_get_tag2(octx->oc->oformat->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
    avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist, AVFMT_TBCF_DEMUXER);
  } else if (octx->ac) {
    st->time_base = octx->ac->time_base;
    ret = avcodec_parameters_from_context(st->codecpar, octx->ac);
    if (ret < 0) as_err("Error setting audio params from encoder\n");
  } else if (is_drop(octx->audio->name)) {
    // Supposed to exit this function early if there's a drop
    as_err("Shouldn't ever happen here\n");
  } else {
    as_err("No audio encoder; not a copy; what is this?\n");
  }
  octx->ai = st->index;

  // signal whether to drop preroll audio
  if (st->codecpar->initial_padding) octx->drop_ts = AV_NOPTS_VALUE;
  return 0;

add_audio_err:
  // XXX free anything here?
  return ret;
#undef as_err
}

static int open_audio_output(struct input_ctx *ictx, struct output_ctx *octx,
  AVOutputFormat *fmt)
{
#define ao_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg"\n"); \
  goto audio_output_err; \
}

  int ret = 0;
  AVCodec *codec = NULL;
  AVCodecContext *ac = NULL;

  // add audio encoder if a decoder exists and this output requires one
  if (ictx->ac && needs_decoder(octx->audio->name)) {

    // initialize audio filters
    ret = init_audio_filters(ictx, octx);
    if (ret < 0) ao_err("Unable to open audio filter")

    // open encoder
    codec = avcodec_find_encoder_by_name(octx->audio->name);
    if (!codec) ao_err("Unable to find audio encoder");
    // open audio encoder
    ac = avcodec_alloc_context3(codec);
    if (!ac) ao_err("Unable to alloc audio encoder");
    octx->ac = ac;
    ac->sample_fmt = av_buffersink_get_format(octx->af.sink_ctx);
    ac->channel_layout = av_buffersink_get_channel_layout(octx->af.sink_ctx);
    ac->channels = av_buffersink_get_channels(octx->af.sink_ctx);
    ac->sample_rate = av_buffersink_get_sample_rate(octx->af.sink_ctx);
    ac->time_base = av_buffersink_get_time_base(octx->af.sink_ctx);
    if (fmt->flags & AVFMT_GLOBALHEADER) ac->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(ac, codec, &octx->audio->opts);
    if (ret < 0) ao_err("Error opening audio encoder");
    av_buffersink_set_frame_size(octx->af.sink_ctx, ac->frame_size);
  }

  ret = add_audio_stream(ictx, octx);
  if (ret < 0) ao_err("Error adding audio stream")

audio_output_err:
  // TODO clean up anything here?
  return ret;

#undef ao_err
}


static int open_output(struct output_ctx *octx, struct input_ctx *ictx)
{
#define em_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_output_err; \
}
  int ret = 0, inp_has_stream;

  AVOutputFormat *fmt = NULL;
  AVFormatContext *oc = NULL;
  AVCodecContext *vc  = NULL;
  AVCodec *codec      = NULL;

  // open muxer
  fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt) em_err("Unable to guess output format\n");
  ret = avformat_alloc_output_context2(&oc, fmt, NULL, octx->fname);
  if (ret < 0) em_err("Unable to alloc output context\n");
  octx->oc = oc;

  // add video encoder if a decoder exists and this output requires one
  if (ictx->vc && needs_decoder(octx->video->name)) {
    ret = init_video_filters(ictx, octx);
    if (ret < 0) em_err("Unable to open video filter");

    codec = avcodec_find_encoder_by_name(octx->video->name);
    if (!codec) em_err("Unable to find encoder");

    // open video encoder
    // XXX use avoptions rather than manual enumeration
    vc = avcodec_alloc_context3(codec);
    if (!vc) em_err("Unable to alloc video encoder\n");
    octx->vc = vc;
    vc->width = av_buffersink_get_w(octx->vf.sink_ctx);
    vc->height = av_buffersink_get_h(octx->vf.sink_ctx);
    if (octx->fps.den) vc->framerate = av_buffersink_get_frame_rate(octx->vf.sink_ctx);
    else vc->framerate = ictx->vc->framerate;
    if (octx->fps.den) vc->time_base = av_buffersink_get_time_base(octx->vf.sink_ctx);
    else if (ictx->vc->time_base.num && ictx->vc->time_base.den) vc->time_base = ictx->vc->time_base;
    else vc->time_base = ictx->ic->streams[ictx->vi]->time_base;
    if (octx->bitrate) vc->rc_min_rate = vc->rc_max_rate = vc->rc_buffer_size = octx->bitrate;
    if (av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx)) {
      vc->hw_frames_ctx =
        av_buffer_ref(av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx));
    }
    vc->pix_fmt = av_buffersink_get_format(octx->vf.sink_ctx); // XXX select based on encoder + input support
    if (fmt->flags & AVFMT_GLOBALHEADER) vc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(vc, codec, &octx->video->opts);
    if (ret < 0) em_err("Error opening video encoder\n");
    octx->hw_type = ictx->hw_type;
  }

  // add video stream if input contains video
  inp_has_stream = ictx->vi >= 0;
  if (inp_has_stream && !octx->dv) {
    ret = add_video_stream(octx, ictx);
    if (ret < 0) em_err("Error adding video stream\n");
  }

  ret = open_audio_output(ictx, octx, fmt);
  if (ret < 0) em_err("Error opening audio output\n");

  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) em_err("Error opening output file\n");
  }

  ret = avformat_write_header(oc, &octx->muxer->opts);
  if (ret < 0) em_err("Error writing header\n");

  return 0;

open_output_err:
  free_output(octx);
  return ret;
}

static void free_input(struct input_ctx *inctx)
{
  if (inctx->ic) avformat_close_input(&inctx->ic);
  if (inctx->vc) {
    if (inctx->vc->hw_device_ctx) av_buffer_unref(&inctx->vc->hw_device_ctx);
    avcodec_free_context(&inctx->vc);
  }
  if (inctx->ac) avcodec_free_context(&inctx->ac);
  if (inctx->hw_device_ctx) av_buffer_unref(&inctx->hw_device_ctx);
}

static int open_video_decoder(input_params *params, struct input_ctx *ctx)
{
#define dd_err(msg,...) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg, ##__VA_ARGS__); \
  goto open_decoder_err; \
}
  int ret = 0;
  AVCodec *codec = NULL;
  AVFormatContext *ic = ctx->ic;

  // open video decoder
  ctx->vi = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (ctx->dv) ; // skip decoding video
  else if (ctx->vi < 0) {
    fprintf(stderr, "No video stream found in input\n");
  } else {
    if (AV_CODEC_ID_H264 == codec->id &&
        AV_HWDEVICE_TYPE_CUDA == params->hw_type) {
      AVCodec *c = avcodec_find_decoder_by_name("h264_cuvid");
      if (c) codec = c;
      else fprintf(stderr, "Cuvid decoder not found; defaulting to software\n");
    } else if (AV_CODEC_ID_H264 == codec->id &&
                       AV_HWDEVICE_TYPE_VAAPI == params->hw_type) {
      enum AVHWDeviceType type;
      type = av_hwdevice_find_type_by_name("vaapi");
      if (type == AV_HWDEVICE_TYPE_NONE) {
        fprintf(stderr, "Device type vaapi is not supported.\n");
        fprintf(stderr, "Available device types:");
        while((type = av_hwdevice_iterate_types(type)) != AV_HWDEVICE_TYPE_NONE)
          fprintf(stderr, " %s", av_hwdevice_get_type_name(type));
        fprintf(stderr, "\n");
        ret = -1;
        goto open_decoder_err;
      }

      int i;
      for (i = 0;; i++) {
        const AVCodecHWConfig *config = avcodec_get_hw_config(codec, i);
        if (!config) {
          dd_err("Decoder %s does not support device type %s.\n", codec->name, av_hwdevice_get_type_name(type));
        }
        if (config->methods & AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX &&
                config->device_type == type) {
          break;
        }
      }
    }
    AVCodecContext *vc = avcodec_alloc_context3(codec);
    if (!vc) dd_err("Unable to alloc video codec\n");
    ctx->vc = vc;
    ret = avcodec_parameters_to_context(vc, ic->streams[ctx->vi]->codecpar);
    if (ret < 0) dd_err("Unable to assign video params\n");
    vc->opaque = (void*)ctx;
    // XXX Could this break if the original device falls out of scope in golang?
    if (params->hw_type != AV_HWDEVICE_TYPE_NONE) {
      // First set the hw device then set the hw frame
      ret = av_hwdevice_ctx_create(&ctx->hw_device_ctx, params->hw_type, params->device, NULL, 0);
      if (ret < 0) dd_err("Unable to open hardware context for decoding\n")
      ctx->hw_type = params->hw_type;
      vc->hw_device_ctx = av_buffer_ref(ctx->hw_device_ctx);
      if (params->hw_type == AV_HWDEVICE_TYPE_VAAPI) {
        vc->pix_fmt = AV_PIX_FMT_VAAPI;
        int err;
        /* set hw_frames_ctx for encoder's AVCodecContext */
        if ((err = set_vaapi_hwframe_ctx(vc, vc->hw_device_ctx)) < 0) {
          dd_err("Failed to set hwframe context.\n");
        }
        vc->get_format = get_vaapi_format;
      } else {
        vc->get_format = get_hw_pixfmt;
      }
    }
    vc->pkt_timebase = ic->streams[ctx->vi]->time_base;
    ret = avcodec_open2(vc, codec, NULL);
    if (ret < 0) dd_err("Unable to open video decoder\n");
  }

  return 0;

open_decoder_err:
  free_input(ctx);
  return ret;
#undef dd_err
}

static int open_audio_decoder(input_params *params, struct input_ctx *ctx)
{
#define ad_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_audio_err; \
}
  int ret = 0;
  AVCodec *codec = NULL;
  AVFormatContext *ic = ctx->ic;

  // open audio decoder
  ctx->ai = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
  if (ctx->da) ; // skip decoding audio
  else if (ctx->ai < 0) {
    fprintf(stderr, "No audio stream found in input\n");
  } else {
    AVCodecContext * ac = avcodec_alloc_context3(codec);
    if (!ac) ad_err("Unable to alloc audio codec\n");
    if (ctx->ac) fprintf(stderr, "Audio context already open! %p\n", ctx->ac);
    ctx->ac = ac;
    ret = avcodec_parameters_to_context(ac, ic->streams[ctx->ai]->codecpar);
    if (ret < 0) ad_err("Unable to assign audio params\n");
    ret = avcodec_open2(ac, codec, NULL);
    if (ret < 0) ad_err("Unable to open audio decoder\n");
  }

  return 0;

open_audio_err:
  free_input(ctx);
  return ret;
#undef ad_err
}

static int open_input(input_params *params, struct input_ctx *ctx)
{
#define dd_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_input_err; \
}
  AVFormatContext *ic   = NULL;
  char *inp = params->fname;
  int ret = 0;

  // open demuxer
  ic = avformat_alloc_context();
  if (!ic) dd_err("demuxer: Unable to alloc context\n");
  ret = avio_open(&ic->pb, inp, AVIO_FLAG_READ);
  if (ret < 0) dd_err("demuxer: Unable to open file\n");
  ret = avformat_open_input(&ic, NULL, NULL, NULL);
  if (ret < 0) dd_err("demuxer: Unable to open input\n");
  ctx->ic = ic;
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) dd_err("Unable to find input info\n");
  ret = open_video_decoder(params, ctx);
  if (ret < 0) dd_err("Unable to open video decoder\n")
  ret = open_audio_decoder(params, ctx);
  if (ret < 0) dd_err("Unable to open audio decoder\n")

  return 0;

open_input_err:
fprintf(stderr, "Freeing input based on OPEN INPUT error\n");
  free_input(ctx);
  return ret;
#undef dd_err
}

int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt)
{
#define dec_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto dec_cleanup; \
}
  int ret = 0;

  // Read a packet and attempt to decode it.
  // If decoding was not possible, return the packet anyway for streamcopy
  av_init_packet(pkt);
  // TODO this while-loop isn't necessary anymore; clean up
  while (1) {
    AVStream *ist = NULL;
    AVCodecContext *decoder = NULL;
    ret = av_read_frame(ictx->ic, pkt);
    if (ret == AVERROR_EOF) goto dec_flush;
    else if (ret < 0) dec_err("Unable to read input\n");
    ist = ictx->ic->streams[pkt->stream_index];
    if (ist->index == ictx->vi && ictx->vc) decoder = ictx->vc;
    else if (ist->index == ictx->ai && ictx->ac) decoder = ictx->ac;
    else if (pkt->stream_index == ictx->vi || pkt->stream_index == ictx->ai) break;
    else dec_err("Could not find decoder or stream\n");

    if (!ictx->first_pkt && pkt->flags & AV_PKT_FLAG_KEY && decoder == ictx->vc) {
      ictx->first_pkt = av_packet_clone(pkt);
      ictx->first_pkt->pts = -1;
    }

    ret = avcodec_send_packet(decoder, pkt);
    if (ret < 0) dec_err("Error sending packet to decoder\n");
    ret = avcodec_receive_frame(decoder, frame);
    if (ret == AVERROR(EAGAIN)) {
      // Distinguish from EAGAIN that may occur with
      // av_read_frame or avcodec_send_packet
      ret = lpms_ERR_PACKET_ONLY;
      break;
    }
    else if (ret < 0) dec_err("Error receiving frame from decoder\n");
    break;
  }

dec_cleanup:
  return ret;

dec_flush:

  // Attempt to read all frames that are remaining within the decoder, starting
  // with video. If there's a nonzero response type, we know there are no more
  // video frames, so continue on to audio.

  // Flush video decoder.
  // To accommodate CUDA, we feed the decoder a a sentinel (flush) frame.
  // Once the flush frame has been decoded, the decoder is fully flushed.
  // TODO this is unnecessary for SW decoding! SW process should match audio
  if (ictx->vc) {
    send_first_pkt(ictx);

    ret = avcodec_receive_frame(ictx->vc, frame);
    pkt->stream_index = ictx->vi;
    if (!ret) {
      if (is_flush_frame(frame)) ictx->flushed = 1;
      return ret;
    }
  }
  // Flush audio decoder.
  if (ictx->ac) {
    avcodec_send_packet(ictx->ac, NULL);
    ret = avcodec_receive_frame(ictx->ac, frame);
    pkt->stream_index = ictx->ai;
    if (!ret) return ret;
  }
  return AVERROR_EOF;

#undef dec_err
}

static int mux(AVPacket *pkt, AVRational tb, struct output_ctx *octx, AVStream *ost)
{
  pkt->stream_index = ost->index;
  if (av_cmp_q(tb, ost->time_base)) {
    av_packet_rescale_ts(pkt, tb, ost->time_base);
  }

  // drop any preroll audio. may need to drop multiple packets for multichannel
  // XXX this breaks if preroll isn't exactly one AVPacket or drop_ts == 0
  //     hasn't been a problem in practice (so far)
  if (AVMEDIA_TYPE_AUDIO == ost->codecpar->codec_type) {
      if (octx->drop_ts == AV_NOPTS_VALUE) octx->drop_ts = pkt->pts;
      if (pkt->pts && pkt->pts == octx->drop_ts) return 0;
  }

  return av_interleaved_write_frame(octx->oc, pkt);
}

int encode(AVCodecContext* encoder, AVFrame *frame, struct output_ctx* octx, AVStream* ost) {
#define encode_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) { fprintf(stderr, "should not happen\n"); ret = AVERROR(ENOMEM); } \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s\n", msg, errstr); \
  goto encode_cleanup; \
}

  int ret = 0;
  AVPacket pkt = {0};

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type && frame) {
    if (!octx->res->frames) {
      frame->pict_type = AV_PICTURE_TYPE_I;
    }
    octx->res->frames++;
    octx->res->pixels += encoder->width * encoder->height;
  }


  // We don't want to send NULL frames for HW encoding
  // because that closes the encoder: not something we want
  if (AV_HWDEVICE_TYPE_NONE == octx->hw_type || frame) {
    ret = avcodec_send_frame(encoder, frame);
    if (AVERROR_EOF == ret) ; // continue ; drain encoder
    else if (ret < 0) encode_err("Error sending frame to encoder");
  }

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type &&
      (AV_HWDEVICE_TYPE_CUDA == octx->hw_type || AV_HWDEVICE_TYPE_VAAPI == octx->hw_type)
       && !frame) {
    avcodec_flush_buffers(encoder);
  }

  while (1) {
    av_init_packet(&pkt);
    ret = avcodec_receive_packet(encoder, &pkt);
    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) goto encode_cleanup;
    if (ret < 0) encode_err("Error receiving packet from encoder\n");
    ret = mux(&pkt, encoder->time_base, octx, ost);
    if (ret < 0) goto encode_cleanup;
    av_packet_unref(&pkt);
  }

encode_cleanup:
  av_packet_unref(&pkt);
  return ret;

#undef encode_err
}

int process_out(struct input_ctx *ictx, struct output_ctx *octx, AVCodecContext *encoder, AVStream *ost,
  struct filter_ctx *filter, AVFrame *inf)
{
#define proc_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) { fprintf(stderr, "u done messed up\n"); ret = AVERROR(ENOMEM); } \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s\n", msg, errstr); \
  goto proc_cleanup; \
}
  int ret = 0;

  if (!encoder) proc_err("Trying to transmux; not supported")

  if (!filter || !filter->active) {
    // No filter in between decoder and encoder, so use input frame directly
    return encode(encoder, inf, octx, ost);
  }

  // Sometimes we have to reset the filter if the HW context is updated
  // because we initially set the filter before the decoder is fully ready
  // and the decoder may change HW params
  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type &&
      inf && inf->hw_frames_ctx && filter->hwframes &&
      inf->hw_frames_ctx->data != filter->hwframes) {
    free_filter(&octx->vf); // XXX really should flush filter first
    ret = init_video_filters(ictx, octx);
    if (ret < 0) return lpms_ERR_FILTERS;
  }
  if (inf) {
    ret = av_buffersrc_write_frame(filter->src_ctx, inf);
    if (ret < 0) proc_err("Error feeding the filtergraph");
  } else {
    // We need to set the pts at EOF to the *end* of the last packet
    // in order to avoid discarding any queued packets
    int64_t next_pts = AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type ?
      ictx->next_pts_v : ictx->next_pts_a;
    av_buffersrc_close(filter->src_ctx, next_pts, AV_BUFFERSRC_FLAG_PUSH);
  }

  while (1) {
    // Drain the filter. Each input frame may have multiple output frames
    AVFrame *frame = filter->frame;
    av_frame_unref(frame);
    ret = av_buffersink_get_frame(filter->sink_ctx, frame);
    frame->pict_type = AV_PICTURE_TYPE_NONE;
    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) {
      // no frame returned from filtergraph
      // proceed only if the input frame is a flush (inf == null)
      if (inf) return ret;
      frame = NULL;
    } else if (ret < 0) proc_err("Error consuming the filtergraph\n");
    ret = encode(encoder, frame, octx, ost);
    av_frame_unref(frame);
    // For HW we keep the encoder open so will only get EAGAIN.
    // Return EOF in place of EAGAIN for to terminate the flush
    if (frame == NULL && AV_HWDEVICE_TYPE_NONE != octx->hw_type &&
        AVERROR(EAGAIN) == ret && !inf) return AVERROR_EOF;
    if (frame == NULL) return ret;
  }

proc_cleanup:
  return ret;
#undef proc_err
}

int flush_outputs(struct input_ctx *ictx, struct output_ctx *octx)
{
  // only issue w this flushing method is it's not necessarily sequential
  // wrt all the outputs; might want to iterate on each output per frame?
  int ret = 0;
  if (octx->vc) { // flush video
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->vc, octx->oc->streams[octx->vi], &octx->vf, NULL);
    }
  }
  ret = 0;
  if (octx->ac) { // flush audio
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->ac, octx->oc->streams[octx->ai], &octx->af, NULL);
    }
  }
  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  return av_write_trailer(octx->oc);
}


int transcode(struct transcode_thread *h,
  input_params *inp, output_params *params,
  output_results *results, output_results *decoded_results)
{
#define main_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) ret = AVERROR(EINVAL); \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s\n", msg, errstr); \
  goto transcode_cleanup; \
}
  int ret = 0, i = 0;
  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;
  AVPacket ipkt;
  AVFrame *dframe = NULL;

  if (!inp) main_err("transcoder: Missing input params\n")

  if (!ictx->ic->pb) {
    ret = avio_open(&ictx->ic->pb, inp->fname, AVIO_FLAG_READ);
    if (ret < 0) main_err("Unable to reopen file");
    // XXX check to see if we can also reuse decoder for sw decoding
    if (AV_HWDEVICE_TYPE_CUDA != ictx->hw_type && AV_HWDEVICE_TYPE_VAAPI != ictx->hw_type) {
      ret = open_video_decoder(inp, ictx);
      if (ret < 0) main_err("Unable to reopen video decoder");
    }
    ret = open_audio_decoder(inp, ictx);
    if (ret < 0) main_err("Unable to reopen audio decoder")
  }

  // populate output contexts
  for (i = 0; i <  nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      octx->fname = params[i].fname;
      octx->width = params[i].w;
      octx->height = params[i].h;
      octx->muxer = &params[i].muxer;
      octx->audio = &params[i].audio;
      octx->video = &params[i].video;
      octx->vfilters = params[i].vfilters;
      if (params[i].bitrate) octx->bitrate = params[i].bitrate;
      if (params[i].fps.den) octx->fps = params[i].fps;
      octx->dv = ictx->vi < 0 || is_drop(octx->video->name);
      octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
      octx->res = &results[i];

      // XXX valgrind this line up
      if (!h->initialized || AV_HWDEVICE_TYPE_NONE == octx->hw_type) {
        ret = open_output(octx, ictx);
        if (ret < 0) main_err("transcoder: Unable to open output");
        continue;
      }

      // reopen output for HW encoding

      AVOutputFormat *fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
      if (!fmt) main_err("Unable to guess format for reopen\n");
      ret = avformat_alloc_output_context2(&octx->oc, fmt, NULL, octx->fname);
      if (ret < 0) main_err("Unable to alloc reopened out context\n");

      // re-attach video encoder
      if (octx->vc) {
        ret = add_video_stream(octx, ictx);
        if (ret < 0) main_err("Unable to re-add video stream\n");
        ret = init_video_filters(ictx, octx);
        if (ret < 0) main_err("Unable to re-open video filter\n")
      } else fprintf(stderr, "no video stream\n");

      // re-attach audio encoder
      ret = open_audio_output(ictx, octx, fmt);
      if (ret < 0) main_err("Unable to re-add audio stream\n");

      if (!(fmt->flags & AVFMT_NOFILE)) {
        ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
        if (ret < 0) main_err("Error re-opening output file\n");
      }
      ret = avformat_write_header(octx->oc, NULL);
      if (ret < 0) main_err("Error re-writing header\n");
  }

  av_init_packet(&ipkt);
  dframe = av_frame_alloc();
  if (!dframe) main_err("transcoder: Unable to allocate frame\n");

  while (1) {
    int has_frame = 0;
    AVStream *ist = NULL;
    av_frame_unref(dframe);
    ret = process_in(ictx, dframe, &ipkt);
    if (ret == AVERROR_EOF) break;
                            // Bail out on streams that appear to be broken
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (ret < 0) main_err("transcoder: Could not decode; stopping\n");
    ist = ictx->ic->streams[ipkt.stream_index];
    has_frame = lpms_ERR_PACKET_ONLY != ret;

    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      if (is_flush_frame(dframe)) goto whileloop_end;
      // width / height will be zero for pure streamcopy (no decoding)
      decoded_results->frames += dframe->width && dframe->height;
      decoded_results->pixels += dframe->width * dframe->height;
      if (has_frame) {
        int64_t dur = 0;
        if (dframe->pkt_duration) dur = dframe->pkt_duration;
        else if (ist->avg_frame_rate.den) {
          dur = av_rescale_q(1, av_inv_q(ist->avg_frame_rate), ist->time_base);
        } else {
          // TODO use better heuristics for this; look at how ffmpeg does it
          //fprintf(stderr, "Could not determine next pts; filter might drop\n");
        }
        ictx->next_pts_v = dframe->pts + dur;
      }
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      if (has_frame) ictx->next_pts_a = dframe->pts + dframe->pkt_duration;
    }

    for (i = 0; i < nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      struct filter_ctx *filter = NULL;
      AVStream *ost = NULL;
      AVCodecContext *encoder = NULL;
      ret = 0; // reset to avoid any carry-through

      if (ist->index == ictx->vi) {
        if (octx->dv) continue; // drop video stream for this output
        ost = octx->oc->streams[0];
        if (ictx->vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        }
      } else if (ist->index == ictx->ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[!octx->dv]; // depends on whether video exists
        if (ictx->ac) {
          encoder = octx->ac;
          filter = &octx->af;
        }
      } else continue; // dropped or unrecognized stream

      if (!encoder && ost) {
        // stream copy
        AVPacket *pkt;

        // we hit this case when decoder is flushing; will be no input packet
        // (we don't need decoded frames since this stream is doing a copy)
        if (ipkt.pts == AV_NOPTS_VALUE) continue;

        pkt = av_packet_clone(&ipkt);
        if (!pkt) main_err("transcoder: Error allocating packet\n");
        ret = mux(pkt, ist->time_base, octx, ost);
        av_packet_free(&pkt);
      } else if (has_frame) {
        ret = process_out(ictx, octx, encoder, ost, filter, dframe);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) main_err("transcoder: Error encoding\n");
    }
whileloop_end:
    av_packet_unref(&ipkt);
  }

  // flush outputs
  for (i = 0; i < nb_outputs; i++) {
    ret = flush_outputs(ictx, &outputs[i]);
    if (ret < 0) main_err("transcoder: Unable to fully flush outputs")
  }

transcode_cleanup:
  avio_closep(&ictx->ic->pb);
  if (dframe) av_frame_free(&dframe);
  ictx->flushed = 0;
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) free_output(&outputs[i]);
  return ret == AVERROR_EOF ? 0 : ret;
#undef main_err
}

int lpms_transcode(input_params *inp, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results)
{
  int ret = 0;
  struct transcode_thread *h = inp->handle;

  if (!h->initialized) {
    int i = 0;
    int decode_a = 0, decode_v = 0;
    if (nb_outputs > MAX_OUTPUT_SIZE) {
      return lpms_ERR_OUTPUTS;
    }

    // Check to see if we can skip decoding
    for (i = 0; i < nb_outputs; i++) {
      if (!needs_decoder(params[i].video.name)) h->ictx.dv = ++decode_v == nb_outputs;
      if (!needs_decoder(params[i].audio.name)) h->ictx.da = ++decode_a == nb_outputs;
    }

    h->nb_outputs = nb_outputs;

    // populate input context
    ret = open_input(inp, &h->ictx);
    if (ret < 0) {
      return ret;
    }
  }

  if (h->nb_outputs != nb_outputs) {
    return lpms_ERR_OUTPUTS; // Not the most accurate error...
  }

  ret = transcode(h, inp, params, results, decoded_results);
  h->initialized = 1;

  return ret;
}

struct transcode_thread* lpms_transcode_new() {
  struct transcode_thread *h = malloc(sizeof (struct transcode_thread));
  if (!h) return NULL;
  memset(h, 0, sizeof *h);
  return h;
}

void lpms_transcode_stop(struct transcode_thread *handle) {
  // not threadsafe as-is; calling function must ensure exclusivity!

  int i;

  if (!handle) return;

  free_input(&handle->ictx);
  for (i = 0; i < MAX_OUTPUT_SIZE; i++) {
    free_output(&handle->outputs[i]);
    if (handle->outputs[i].vc) avcodec_free_context(&handle->outputs[i].vc);
  }

  free(handle);
}
