#include "lpms_ffmpeg.h"

#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersink.h>
#include <libavfilter/buffersrc.h>
#include <libavutil/opt.h>

// Not great to appropriate internal API like this...
const int lpms_ERR_INPUT_PIXFMT = FFERRTAG('I','N','P','X');
const int lpms_ERR_FILTERS = FFERRTAG('F','L','T','R');
const int lpms_ERR_PACKET_ONLY = FFERRTAG('P','K','O','N');

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

  int64_t next_pts_a, next_pts_v;
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

  // muxer and encoder information (name + options)
  component_opts *muxer;
  component_opts *video;
  component_opts *audio;

  int64_t drop_ts;     // preroll audio ts to drop

  output_results  *res; // data to return for this output
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
  if (octx->vc) avcodec_free_context(&octx->vc);
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

static enum AVPixelFormat get_hw_pixfmt(AVCodecContext *ctx, const enum AVPixelFormat *pix_fmts)
{
  // XXX see avcodec_get_hw_frames_parameters if fmt changes mid-stream
  return hw2pixfmt(ctx);
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
  AVCodecContext *ac  = NULL;
  AVCodec *codec      = NULL;
  AVStream *st        = NULL;

  // open muxer
  fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt) em_err("Unable to guess output format\n");
  ret = avformat_alloc_output_context2(&oc, fmt, NULL, octx->fname);
  if (ret < 0) em_err("Unable to alloc output context\n");
  octx->oc = oc;

  // add video encoder if a decoder exists and this output requires one
  if (ictx->vc && needs_decoder(octx->video->name)) {
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
    if (octx->fps.den) vc->time_base = av_buffersink_get_time_base(octx->vf.sink_ctx);
    if (octx->bitrate) vc->rc_min_rate = vc->rc_max_rate = vc->rc_buffer_size = octx->bitrate;
    if (av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx)) {
      vc->hw_frames_ctx =
        av_buffer_ref(av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx));
    }
    vc->pix_fmt = av_buffersink_get_format(octx->vf.sink_ctx); // XXX select based on encoder + input support
    if (fmt->flags & AVFMT_GLOBALHEADER) vc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(vc, codec, &octx->video->opts);
    av_dict_free(&octx->video->opts); // avcodec_open2 replaces this
    if (ret < 0) em_err("Error opening video encoder\n");
  }

  // add video stream if input contains video
  inp_has_stream = ictx->vi >= 0;
  if (inp_has_stream && !octx->dv) {
    // video stream in muxer
    st = avformat_new_stream(oc, NULL);
    if (!st) em_err("Unable to alloc video stream\n");
    octx->vi = st->index;
    st->avg_frame_rate = octx->fps;
    if (is_copy(octx->video->name)) {
      AVStream *ist = ictx->ic->streams[ictx->vi];
      st->time_base = ist->time_base;
      ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
      if (ret < 0) em_err("Error copying video params from input stream\n");
      // Sometimes the codec tag is wonky for some reason, so correct it
      ret = av_codec_get_tag2(fmt->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
      //if (!ret) fprintf(stderr, "Video codec tag not found. Continuing anyway\n");
      avformat_transfer_internal_stream_timing_info(fmt, st, ist, AVFMT_TBCF_DEMUXER);
    } else if (vc) {
      st->time_base = vc->time_base;
      ret = avcodec_parameters_from_context(st->codecpar, vc);
      if (ret < 0) em_err("Error setting video params from encoder\n");
    } else em_err("No video encoder, not a copy; what is this?\n");
  }

  // add audio encoder if a decoder exists and this output requires one
  if (ictx->ac && needs_decoder(octx->audio->name)) {
    codec = avcodec_find_encoder_by_name(octx->audio->name);
    if (!codec) em_err("Unable to find audio encoder\n");
    // open audio encoder
    ac = avcodec_alloc_context3(codec);
    if (!ac) em_err("Unable to alloc audio encoder\n");
    octx->ac = ac;
    ac->sample_fmt = av_buffersink_get_format(octx->af.sink_ctx);
    ac->channel_layout = av_buffersink_get_channel_layout(octx->af.sink_ctx);
    ac->channels = av_buffersink_get_channels(octx->af.sink_ctx);
    ac->sample_rate = av_buffersink_get_sample_rate(octx->af.sink_ctx);
    ac->time_base = av_buffersink_get_time_base(octx->af.sink_ctx);
    if (fmt->flags & AVFMT_GLOBALHEADER) ac->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(ac, codec, &octx->audio->opts);
    av_dict_free(&octx->audio->opts); // avcodec_open2 replaces this
    if (ret < 0) em_err("Error opening audio encoder\n");
    av_buffersink_set_frame_size(octx->af.sink_ctx, ac->frame_size);
  }

  // add audio stream if input contains audio
  inp_has_stream = ictx->ai >= 0;
  if (inp_has_stream && !octx->da) {
    // audio stream in muxer
    st = avformat_new_stream(oc, NULL);
    if (!st) em_err("Unable to alloc audio stream\n");
    if (is_copy(octx->audio->name)) {
      AVStream *ist = ictx->ic->streams[ictx->ai];
      st->time_base = ist->time_base;
      ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
      if (ret < 0) em_err("Error copying audio params from input stream\n");
      // Sometimes the codec tag is wonky for some reason, so correct it
      ret = av_codec_get_tag2(fmt->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
      //if (!ret) fprintf(stderr, "Audio codec tag not found. Continuing anyway\n");
      avformat_transfer_internal_stream_timing_info(fmt, st, ist, AVFMT_TBCF_DEMUXER);
    } else if (ac) {
      st->time_base = ac->time_base;
      ret = avcodec_parameters_from_context(st->codecpar, ac);
      if (ret < 0) em_err("Error setting audio params from encoder\n");
    } else em_err("No audio encoder; not a copy; what is this?\n");
    octx->ai = st->index;

    // signal whether to drop preroll audio
    if (st->codecpar->initial_padding) octx->drop_ts = AV_NOPTS_VALUE;
  }

  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) em_err("Error opening output file\n");
  }

  ret = avformat_write_header(oc, &octx->muxer->opts);
  av_dict_free(&octx->muxer->opts); // avformat_write_header replaces this
  if (ret < 0) em_err("Error writing header\n");

  return 0;

open_output_err:
  free_output(octx);
  return ret;
}

static void free_input(struct input_ctx *inctx)
{
  if (inctx->ic) avformat_close_input(&inctx->ic);
  if (inctx->vc) avcodec_free_context(&inctx->vc);
  if (inctx->ac) avcodec_free_context(&inctx->ac);
  if (inctx->hw_device_ctx) av_buffer_unref(&inctx->hw_device_ctx);
}

static int open_input(input_params *params, struct input_ctx *ctx)
{
#define dd_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_input_err; \
}
  AVCodec *codec = NULL;
  AVFormatContext *ic   = NULL;
  char *inp = params->fname;

  // open demuxer
  int ret = avformat_open_input(&ic, inp, NULL, NULL);
  if (ret < 0) dd_err("demuxer: Unable to open input\n");
  ctx->ic = ic;
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) dd_err("Unable to find input info\n");

  // open video decoder
  ctx->vi = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (ctx->dv) ; // skip decoding video
  else if (ctx->vi < 0) {
    fprintf(stderr, "No video stream found in input\n");
  } else {
    AVCodecContext *vc = avcodec_alloc_context3(codec);
    if (!vc) dd_err("Unable to alloc video codec\n");
    ctx->vc = vc;
    ret = avcodec_parameters_to_context(vc, ic->streams[ctx->vi]->codecpar);
    if (ret < 0) dd_err("Unable to assign video params\n");
    if (params->hw_type != AV_HWDEVICE_TYPE_NONE) {
      // First set the hw device then set the hw frame
      AVHWFramesContext *frames;
      ret = av_hwdevice_ctx_create(&ctx->hw_device_ctx, params->hw_type, params->device, NULL, 0);
      if (ret < 0) dd_err("Unable to open hardware context for decoding\n")
      ctx->hw_type = params->hw_type;
      vc->hw_device_ctx = av_buffer_ref(ctx->hw_device_ctx);
      vc->get_format = get_hw_pixfmt;
      vc->opaque = (void*)ctx;
      // XXX Ideally this would be auto initialized by the HW device ctx
      //     However the initialization doesn't occur in time to set up filters
      //     So we do it here. Also see avcodec_get_hw_frames_parameters
      vc->hw_frames_ctx = av_hwframe_ctx_alloc(vc->hw_device_ctx);
      if (!vc->hw_frames_ctx) dd_err("Unable to allocate hwframe context for decoding\n")
      frames = (AVHWFramesContext*)vc->hw_frames_ctx->data;
      frames->format = hw2pixfmt(vc);
      frames->sw_format = vc->pix_fmt;
      frames->width = vc->width;
      frames->height = vc->height;

      // May want to allocate extra HW frames if we encounter samples where
      // the defaults are insufficient. Raising this increases GPU memory usage
      // For now, the defaults seems OK.
      //vc->extra_hw_frames = 16 + 1; // H.264 max refs

      ret = av_hwframe_ctx_init(vc->hw_frames_ctx);
      if (AVERROR(ENOSYS) == ret) ret = lpms_ERR_INPUT_PIXFMT; // most likely
      if (ret < 0) dd_err("Unable to initialize a hardware frame pool\n")
    }
    ret = avcodec_open2(vc, codec, NULL);
    if (ret < 0) dd_err("Unable to open video decoder\n");
  }

  // open audio decoder
  ctx->ai = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
  if (ctx->da) ; // skip decoding audio
  else if (ctx->ai < 0) {
    fprintf(stderr, "No audio stream found in input\n");
  } else {
    AVCodecContext * ac = avcodec_alloc_context3(codec);
    if (!ac) dd_err("Unable to alloc audio codec\n");
    ctx->ac = ac;
    ret = avcodec_parameters_to_context(ac, ic->streams[ctx->ai]->codecpar);
    if (ret < 0) dd_err("Unable to assign audio params\n");
    ret = avcodec_open2(ac, codec, NULL);
    if (ret < 0) dd_err("Unable to open audio decoder\n");
  }

  return 0;

open_input_err:
  free_input(ctx);
  return ret;
#undef dd_err
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
    enum AVPixelFormat pix_fmts[] = { AV_PIX_FMT_YUV420P, AV_PIX_FMT_CUDA, AV_PIX_FMT_NONE }; // XXX ensure the encoder allows this
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


static int init_audio_filters(struct input_ctx *ictx, struct output_ctx *octx,
    char *filters_descr)
{
#define af_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto init_audio_filters_cleanup; \
}
  int ret = 0;
  char args[512];
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

int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt)
{
#define dec_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto dec_cleanup; \
}
  int ret = 0;

  av_init_packet(pkt);
  // loop until a new frame has been decoded, or EAGAIN
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
  if (ictx->vc) {
    avcodec_send_packet(ictx->vc, NULL);
    ret = avcodec_receive_frame(ictx->vc, frame);
    pkt->stream_index = ictx->vi; // XXX ugly?
    if (!ret) return ret;
  }
  if (ictx->ac) {
    avcodec_send_packet(ictx->ac, NULL);
    ret = avcodec_receive_frame(ictx->ac, frame);
    pkt->stream_index = ictx->ai; // XXX ugly?
  }
  return ret;

#undef dec_err
}

int mux(AVPacket *pkt, AVRational tb, struct output_ctx *octx, AVStream *ost)
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
  fprintf(stderr, "%s: %s", msg, errstr); \
  goto encode_cleanup; \
}

  AVPacket pkt = {0};

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type && frame) {
    octx->res->frames++;
    octx->res->pixels += encoder->width * encoder->height;
  }

  int ret = avcodec_send_frame(encoder, frame);
  if (AVERROR_EOF == ret) ; // continue ; drain encoder
  else if (ret < 0) encode_err("Error sending frame to encoder");

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
  fprintf(stderr, "%s: %s", msg, errstr); \
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
    if (ret < 0) proc_err("Error feeding the filtergraph\n");
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
    if (frame == NULL) return ret;
  }

proc_cleanup:
  return ret;
#undef proc_err
}

#define MAX_OUTPUT_SIZE 10

int lpms_transcode(input_params *inp, output_params *params,
    output_results *results, int nb_outputs, output_results *decoded_results)
{
#define main_err(msg) { \
  if (!ret) ret = AVERROR(EINVAL); \
  fprintf(stderr, msg); \
  goto transcode_cleanup; \
}
  int ret = 0, i = 0;
  int decode_a = 0, decode_v = 0;
  struct input_ctx ictx;
  AVPacket ipkt;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];
  AVFrame *dframe = NULL;

  memset(&ictx, 0, sizeof ictx);
  memset(outputs, 0, sizeof outputs);

  if (!inp) main_err("transcoder: Missing input params\n")
  if (nb_outputs > MAX_OUTPUT_SIZE) main_err("transcoder: Too many outputs\n");

  // Check to see if we can skip decoding
  for (i = 0; i < nb_outputs; i++) {
    if (!needs_decoder(params[i].video.name)) ictx.dv = ++decode_v == nb_outputs;
    if (!needs_decoder(params[i].audio.name)) ictx.da = ++decode_a == nb_outputs;
  }

  // populate input context
  ret = open_input(inp, &ictx);
  if (ret < 0) main_err("transcoder: Unable to open input\n");

  // populate output contexts
  for (i = 0; i < nb_outputs; i++) {
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
    octx->dv = ictx.vi < 0 || is_drop(octx->video->name);
    octx->da = ictx.ai < 0 || is_drop(octx->audio->name);
    octx->res = &results[i];
    if (ictx.vc) {
      ret = init_video_filters(&ictx, octx);
      if (ret < 0) main_err("Unable to open video filter");
    }
    if (ictx.ac) {
      char filter_str[256];
      //snprintf(filter_str, sizeof filter_str, "aformat=sample_fmts=s16:channel_layouts=stereo:sample_rates=44100,asetnsamples=n=1152,aresample");
      snprintf(filter_str, sizeof filter_str, "aformat=sample_fmts=fltp:channel_layouts=stereo:sample_rates=44100"); // set sample format and rate based on encoder support
      ret = init_audio_filters(&ictx, octx, filter_str);
      if (ret < 0) main_err("Unable to open audio filter");
    }
    ret = open_output(octx, &ictx);
    if (ret < 0) main_err("transcoder: Unable to open output\n");
  }

  av_init_packet(&ipkt);
  dframe = av_frame_alloc();
  if (!dframe) main_err("transcoder: Unable to allocate frame\n");

  while (1) {
    int has_frame = 0;
    AVStream *ist = NULL;
    av_frame_unref(dframe);
    ret = process_in(&ictx, dframe, &ipkt);
    if (ret == AVERROR_EOF) break;
                            // Bail out on streams that appear to be broken
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (ret < 0) main_err("transcoder: Could not decode; stopping\n");
    ist = ictx.ic->streams[ipkt.stream_index];
    has_frame = lpms_ERR_PACKET_ONLY != ret;

    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      // width / height will be zero for pure streamcopy (no decoding)
      decoded_results->frames += dframe->width && dframe->height;
      decoded_results->pixels += dframe->width * dframe->height;
      if (has_frame) ictx.next_pts_v = dframe->pts + dframe->pkt_duration;
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      if (has_frame) ictx.next_pts_a = dframe->pts + dframe->pkt_duration;
    }

    for (i = 0; i < nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      struct filter_ctx *filter = NULL;
      AVStream *ost = NULL;
      AVCodecContext *encoder = NULL;
      ret = 0; // reset to avoid any carry-through

      if (ist->index == ictx.vi) {
        if (octx->dv) continue; // drop video stream for this output
        ost = octx->oc->streams[0];
        if (ictx.vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        }
      } else if (ist->index == ictx.ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[!octx->dv]; // depends on whether video exists
        if (ictx.ac) {
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
        ret = process_out(&ictx, octx, encoder, ost, filter, dframe);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) main_err("transcoder: Error encoding\n");
    }

whileloop_end:
    av_packet_unref(&ipkt);
  }

  // flush outputs
  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    // only issue w this flushing method is it's not necessarily sequential
    // wrt all the outputs; might want to iterate on each output per frame?
    ret = 0;
    if (octx->vc) { // flush video
      while (!ret || ret == AVERROR(EAGAIN)) {
        ret = process_out(&ictx, octx, octx->vc, octx->oc->streams[octx->vi], &octx->vf, NULL);
      }
    }
    ret = 0;
    if (octx->ac) { // flush audio
      while (!ret || ret == AVERROR(EAGAIN)) {
        ret = process_out(&ictx, octx, octx->ac, octx->oc->streams[octx->ai], &octx->af, NULL);
      }
    }
    av_interleaved_write_frame(octx->oc, NULL); // flush muxer
    ret = av_write_trailer(octx->oc);
    if (ret < 0) main_err("transcoder: Unable to write trailer");
  }

transcode_cleanup:
  free_input(&ictx);
  for (i = 0; i < MAX_OUTPUT_SIZE; i++) free_output(&outputs[i]);
  if (dframe) av_frame_free(&dframe);
  return ret == AVERROR_EOF ? 0 : ret;
#undef main_err
}
