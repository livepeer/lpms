#include "filter.h"
#include "logging.h"

#include <libavfilter/buffersrc.h>
#include <libavfilter/buffersink.h>

#include <libavutil/opt.h>
#include <libavutil/pixdesc.h>

#include <assert.h>

int filtergraph_parser(struct filter_ctx *fctx, char* filters_descr, AVFilterInOut **inputs, AVFilterInOut **outputs)
{
  int ret = -1;
  if(fctx == NULL || filters_descr == NULL || inputs == NULL || outputs == NULL)
    return ret;
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
  (*outputs)->name       = av_strdup("in");
  (*outputs)->filter_ctx = fctx->src_ctx;
  (*outputs)->pad_idx    = 0;
  (*outputs)->next       = NULL;

  /*
   * The buffer sink input must be connected to the output pad of
   * the last filter described by filters_descr; since the last
   * filter output label is not specified, it is set to "out" by
   * default.
   */
  (*inputs)->name       = av_strdup("out");
  (*inputs)->filter_ctx = fctx->sink_ctx;
  (*inputs)->pad_idx    = 0;
  (*inputs)->next       = NULL;

  ret = avfilter_graph_parse_ptr(fctx->graph, filters_descr,
                                  inputs, outputs, NULL);
  return ret;
}

int init_video_filters(struct input_ctx *ictx, struct output_ctx *octx)
{
    char args[512];
    int ret = 0;
    const AVFilter *buffersrc  = avfilter_get_by_name("buffer");
    const AVFilter *buffersink = avfilter_get_by_name("buffersink");
    AVFilterInOut *outputs = NULL;
    AVFilterInOut *inputs  = NULL;
    AVRational time_base = ictx->ic->streams[ictx->vi]->time_base;
    enum AVPixelFormat pix_fmts[] = { AV_PIX_FMT_YUV420P, AV_PIX_FMT_CUDA, AV_PIX_FMT_NONE }; // XXX ensure the encoder allows this
    struct filter_ctx *vf = &octx->vf;
    char *filters_descr = octx->vfilters;
    enum AVPixelFormat in_pix_fmt = ictx->vc->pix_fmt;

    // no need for filters with the following conditions
    if (vf->active) goto vf_init_cleanup; // already initialized
    if (!needs_decoder(octx->video->name)) goto vf_init_cleanup;

    outputs = avfilter_inout_alloc();
    inputs = avfilter_inout_alloc();
    if (vf->graph == NULL) {
      vf->graph = avfilter_graph_alloc();
    }
    vf->pts_diff = INT64_MIN;
    if (!outputs || !inputs || !vf->graph) {
      ret = AVERROR(ENOMEM);
      LPMS_ERR(vf_init_cleanup, "Unable to allocate filters");
    }
    vf->time_base = time_base;
    if (ictx->vc->hw_device_ctx) in_pix_fmt = hw2pixfmt(ictx->vc);

    /* buffer video source: the decoded frames from the decoder will be inserted here. */
    snprintf(args, sizeof args,
            "video_size=%dx%d:pix_fmt=%d:time_base=%d/%d:pixel_aspect=%d/%d:colorspace=%s:range=%s",
            ictx->vc->width, ictx->vc->height, in_pix_fmt,
            time_base.num, time_base.den,
            ictx->vc->sample_aspect_ratio.num, ictx->vc->sample_aspect_ratio.den,
            av_color_space_name(ictx->vc->colorspace), av_color_range_name(ictx->vc->color_range));

    ret = avfilter_graph_create_filter(&vf->src_ctx, buffersrc,
                                       "in", args, NULL, vf->graph);
    if (ret < 0) LPMS_ERR(vf_init_cleanup, "Cannot create video buffer source");
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
    if (ret < 0) LPMS_ERR(vf_init_cleanup, "Cannot create video buffer sink");

    ret = av_opt_set_int_list(vf->sink_ctx, "pix_fmts", pix_fmts,
                              AV_PIX_FMT_NONE, AV_OPT_SEARCH_CHILDREN);
    if (ret < 0) LPMS_ERR(vf_init_cleanup, "Cannot set output pixel format");

    ret = filtergraph_parser(vf, filters_descr, &inputs, &outputs);
    if (ret < 0) LPMS_ERR(vf_init_cleanup, "Unable to parse video filters desc");

    ret = avfilter_graph_config(vf->graph, NULL);
    if (ret < 0) LPMS_ERR(vf_init_cleanup, "Unable configure video filtergraph");

    LPMS_DEBUG("Initialized filtergraph: ");
    LPMS_DEBUG(avfilter_graph_dump(vf->graph, NULL));

    vf->frame = av_frame_alloc();
    if (!vf->frame) LPMS_ERR(vf_init_cleanup, "Unable to allocate video frame");

    vf->active = 1;

vf_init_cleanup:
    avfilter_inout_free(&inputs);
    avfilter_inout_free(&outputs);

    return ret;
}


int init_audio_filters(struct input_ctx *ictx, struct output_ctx *octx)
{
  int ret = 0;
  char args[512];
  char filters_descr[256];
  char channel_layout[256];
  const AVFilter *buffersrc  = avfilter_get_by_name("abuffer");
  const AVFilter *buffersink = avfilter_get_by_name("abuffersink");
  AVFilterInOut *outputs = NULL;
  AVFilterInOut *inputs  = NULL;
  struct filter_ctx *af = &octx->af;
  AVRational time_base = ictx->ic->streams[ictx->ai]->time_base;

  // no need for filters with the following conditions
  if (af->active) goto af_init_cleanup; // already initialized
  if (!needs_decoder(octx->audio->name)) goto af_init_cleanup;

  outputs = avfilter_inout_alloc();
  inputs = avfilter_inout_alloc();
  af->graph = avfilter_graph_alloc();

  if (!outputs || !inputs || !af->graph) {
    ret = AVERROR(ENOMEM);
    LPMS_ERR(af_init_cleanup, "Unable to allocate audio filters");
  }

  /* buffer audio source: the decoded frames from the decoder will be inserted here. */
  ret = av_channel_layout_describe(&ictx->ac->ch_layout, channel_layout, sizeof(channel_layout));
  if (ret < 0) LPMS_ERR(af_init_cleanup, "Unable to describe audio channel layout");
  snprintf(args, sizeof args,
      "sample_rate=%d:sample_fmt=%d:channel_layout=%s:channels=%d:"
      "time_base=%d/%d",
      ictx->ac->sample_rate, ictx->ac->sample_fmt, channel_layout,
      ictx->ac->ch_layout.nb_channels, time_base.num, time_base.den);

  // TODO set sample format and rate based on encoder support,
  //      rather than hardcoding
  snprintf(filters_descr, sizeof filters_descr,
    "aformat=sample_fmts=fltp:channel_layouts=stereo:sample_rates=44100");

  ret = avfilter_graph_create_filter(&af->src_ctx, buffersrc,
                                     "in", args, NULL, af->graph);
  if (ret < 0) LPMS_ERR(af_init_cleanup, "Cannot create audio buffer source");

  /* buffer audio sink: to terminate the filter chain. */
  ret = avfilter_graph_create_filter(&af->sink_ctx, buffersink,
                                     "out", NULL, NULL, af->graph);
  if (ret < 0) LPMS_ERR(af_init_cleanup, "Cannot create audio buffer sink");

  ret = filtergraph_parser(af, filters_descr, &inputs, &outputs);
  if (ret < 0) LPMS_ERR(af_init_cleanup, "Unable to parse audio filters desc");

  ret = avfilter_graph_config(af->graph, NULL);
  if (ret < 0) LPMS_ERR(af_init_cleanup, "Unable configure audio filtergraph");

  af->frame = av_frame_alloc();
  if (!af->frame) LPMS_ERR(af_init_cleanup, "Unable to allocate audio frame");

  af->active = 1;

af_init_cleanup:
  avfilter_inout_free(&inputs);
  avfilter_inout_free(&outputs);

  return ret;
}

int init_signature_filters(struct output_ctx *octx, AVFrame *inf)
{
    char args[512];
    int ret = 0;
    const AVFilter *buffersrc  = avfilter_get_by_name("buffer");
    const AVFilter *buffersink = avfilter_get_by_name("buffersink");
    AVFilterInOut *outputs = NULL;
    AVFilterInOut *inputs  = NULL;
    AVRational time_base = octx->oc->streams[0]->time_base;
    enum AVPixelFormat pix_fmts[] = { AV_PIX_FMT_YUV420P, AV_PIX_FMT_CUDA, AV_PIX_FMT_NONE }; // XXX ensure the encoder allows this
    struct filter_ctx *sf = &octx->sf;
    char *filters_descr = octx->sfilters;
    enum AVPixelFormat in_pix_fmt = octx->vc->pix_fmt;
    if(octx->sfilters == NULL || strlen(octx->sfilters) <= 0) goto sf_init_cleanup;

    // no need for filters with the following conditions
    if (sf->active) goto sf_init_cleanup; // already initialized
    if (!needs_decoder(octx->video->name)) goto sf_init_cleanup;

    outputs = avfilter_inout_alloc();
    inputs = avfilter_inout_alloc();
    sf->graph = avfilter_graph_alloc();
    sf->pts_diff = INT64_MIN;
    if (!outputs || !inputs || !sf->graph) {
      ret = AVERROR(ENOMEM);
      LPMS_ERR(sf_init_cleanup, "Unable to allocate filters");
    }
    if (octx->vc->hw_device_ctx) in_pix_fmt = hw2pixfmt(octx->vc);

    /* buffer video source: the scaled frames from the decoder will be inserted here. */
    snprintf(args, sizeof args,
		  "video_size=%dx%d:pix_fmt=%d:time_base=%d/%d:pixel_aspect=%d/%d",
		  octx->vc->width, octx->vc->height, in_pix_fmt,
		  time_base.num, time_base.den,
		  octx->vc->sample_aspect_ratio.num, octx->vc->sample_aspect_ratio.den);

    ret = avfilter_graph_create_filter(&sf->src_ctx, buffersrc,
                                       "in", args, NULL, sf->graph);
    if (ret < 0) LPMS_ERR(sf_init_cleanup, "Cannot create video buffer source");

    if (octx->vc && inf && inf->hw_frames_ctx) {
      AVBufferSrcParameters *srcpar = av_buffersrc_parameters_alloc();      
      srcpar->hw_frames_ctx = inf->hw_frames_ctx;
      sf->hwframes = inf->hw_frames_ctx->data;
      av_buffersrc_parameters_set(sf->src_ctx, srcpar);
      av_freep(&srcpar);
    } else if (octx->vc && octx->vc->hw_frames_ctx) {
      AVBufferSrcParameters *srcpar = av_buffersrc_parameters_alloc();
      srcpar->hw_frames_ctx = octx->vc->hw_frames_ctx;
      sf->hwframes = octx->vc->hw_frames_ctx->data;
      av_buffersrc_parameters_set(sf->src_ctx, srcpar);
      av_freep(&srcpar);
    }

    /* buffer video sink: to terminate the filter chain. */
    ret = avfilter_graph_create_filter(&sf->sink_ctx, buffersink,
                                       "out", NULL, NULL, sf->graph);
    if (ret < 0) LPMS_ERR(sf_init_cleanup, "Cannot create video buffer sink");

    ret = av_opt_set_int_list(sf->sink_ctx, "pix_fmts", pix_fmts,
                              AV_PIX_FMT_NONE, AV_OPT_SEARCH_CHILDREN);
    if (ret < 0) LPMS_ERR(sf_init_cleanup, "Cannot set output pixel format");

    ret = filtergraph_parser(sf, filters_descr, &inputs, &outputs);
    if (ret < 0) LPMS_ERR(sf_init_cleanup, "Unable to parse signature filters desc");

    ret = avfilter_graph_config(sf->graph, NULL);
    if (ret < 0) LPMS_ERR(sf_init_cleanup, "Unable configure signature filtergraph");

    sf->frame = av_frame_alloc();
    if (!sf->frame) LPMS_ERR(sf_init_cleanup, "Unable to allocate video frame");

    sf->active = 1;

sf_init_cleanup:
    avfilter_inout_free(&inputs);
    avfilter_inout_free(&outputs);

    return ret;
}

int filtergraph_write(AVFrame *inf, struct input_ctx *ictx, struct output_ctx *octx, struct filter_ctx *filter, int is_video)
{
  int ret = 0;
  // We have to reset the filter because we initially set the filter
  // before the decoder is fully ready, and the decoder may change HW params
  // XXX: Unclear if this path is hit on all devices
  if (is_video && inf && inf->hw_frames_ctx && filter->hwframes &&
      inf->hw_frames_ctx->data != filter->hwframes) {
    free_filter(&octx->vf); // XXX really should flush filter first
    ret = init_video_filters(ictx, octx);
    if (ret < 0) return lpms_ERR_FILTERS;
  }

  // Timestamp handling code
  AVStream *vst = ictx->ic->streams[ictx->vi];
  if (inf) { // Non-Flush Frame
    inf->opaque = (void *) inf->pts; // Store original PTS for calc later
    if (is_video && octx->fps.den) {
      // Custom PTS set when FPS filter is used
      int64_t ts_step = inf->pts - filter->prev_frame_pts;
      if (filter->segments_complete && !filter->prev_frame_pts) {
        // We are on the first frame of the second (or later) segment
        // So in this case just increment the pts by 1/fps
        ts_step = av_rescale_q_rnd(1, av_inv_q(octx->fps), vst->time_base, AV_ROUND_NEAR_INF|AV_ROUND_PASS_MINMAX);
      }
      filter->custom_pts += ts_step;
      filter->prev_frame_pts = inf->pts;
    } else {
      // FPS Passthrough or Audio case
      filter->custom_pts = inf->pts;
    }
  } else if (!filter->flushed) { // Flush Frame
    int64_t ts_step;
    inf = (is_video) ? ictx->last_frame_v : ictx->last_frame_a;
    inf->opaque = (void *) (INT64_MIN); // Store INT64_MIN as pts for flush frames
    filter->flushing = 1;
    if (is_video && octx->fps.den) {
      // set ts_step to one frame (1/fps) in units of the output timebase
      ts_step = av_rescale_q_rnd(1, av_inv_q(octx->fps), vst->time_base, AV_ROUND_NEAR_INF|AV_ROUND_PASS_MINMAX);
    } else {
      // FPS Passthrough or Audio case - use packet duration instead of custom duration
      ts_step = inf->duration;
    }
    filter->custom_pts += ts_step;
  }

  if (inf) {
    // Apply the custom pts, then reset for the next output
    int old_pts = inf->pts;
    inf->pts = filter->custom_pts;
    ret = av_buffersrc_write_frame(filter->src_ctx, inf);
    inf->pts = old_pts;
    if (ret < 0) LPMS_ERR(fg_write_cleanup, "Error feeding the filtergraph");
  }
fg_write_cleanup:
  return ret;
}

int filtergraph_read(struct input_ctx *ictx, struct output_ctx *octx, struct filter_ctx *filter, int is_video)
{
    AVFrame *frame = filter->frame;
    av_frame_unref(frame);

    int ret = av_buffersink_get_frame(filter->sink_ctx, frame);
    frame->pict_type = AV_PICTURE_TYPE_NONE;

    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) return ret;
    else if (ret < 0) LPMS_ERR(fg_read_cleanup, "Error consuming the filtergraph");

    if (frame && ((int64_t) frame->opaque == INT64_MIN)) {
      // opaque being INT64_MIN means it's a flush packet
      // don't set flushed flag in case this is a flush from a previous segment
      if (filter->flushing) filter->flushed = 1;
      ret = lpms_ERR_FILTER_FLUSHED;
    } else if (frame && is_video && octx->fps.den) {
      // TODO why limit to fps filter? what about non-fps filtergraphs, eg scale?
      // We set custom PTS as an input of the filtergraph so we need to
      // re-calculate our output PTS before passing it on to the encoder
      if (filter->pts_diff == INT64_MIN) {
        int64_t pts = (int64_t)frame->opaque; // original input PTS
        pts = av_rescale_q_rnd(pts, ictx->ic->streams[ictx->vi]->time_base, av_buffersink_get_time_base(filter->sink_ctx), AV_ROUND_NEAR_INF|AV_ROUND_PASS_MINMAX);
        // difference between rescaled input PTS and the segment's first frame PTS of the filtergraph output
        filter->pts_diff = pts - frame->pts;
      }
      frame->pts += filter->pts_diff; // Re-calculate by adding back this segment's difference calculated at start
    }
fg_read_cleanup:
    return ret;
}

void free_filter(struct filter_ctx *filter)
{
  if (filter->frame) av_frame_free(&filter->frame);
  if (filter->graph) avfilter_graph_free(&filter->graph);
  memset(filter, 0, sizeof(struct filter_ctx));
}
