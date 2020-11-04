#include "transcoder.h"
#include "decoder.h"
#include "filter.h"
#include "encoder.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>

#include <libavfilter/buffersrc.h>
#include <libavfilter/buffersink.h>

// Not great to appropriate internal API like this...
const int lpms_ERR_INPUT_PIXFMT = FFERRTAG('I','N','P','X');
const int lpms_ERR_INPUT_CODEC = FFERRTAG('I','N','P','C');
const int lpms_ERR_FILTERS = FFERRTAG('F','L','T','R');
const int lpms_ERR_PACKET_ONLY = FFERRTAG('P','K','O','N');
const int lpms_ERR_FILTER_FLUSHED = FFERRTAG('F','L','F','L');
const int lpms_ERR_OUTPUTS = FFERRTAG('O','U','T','P');
const int lpms_ERR_DTS = FFERRTAG('-','D','T','S');

//
//  Notes on transcoder internals:
//
//  Transcoding follows the typical process of the FFmpeg API:
//    read/demux/decode/filter/encode/mux/write
//
//  This is done over discrete segments. However, decode/filter/encoder are
//  expensive to re-initialize for every segment. We work around this by
//  persisting these components across segments.
//
//  The challenge with persistence is there is often internal data that is
//  buffered, and there isn't an explicit API to flush or drain that data
//  short of re-initializing the component. This is addressed for each component
//  as follows:
//
//  Demuxer: For resumable / header-less formats such as mpegts, the demuxer
//           is reused across segments. This gives a small speed boost. For
//           all other formats, the demuxer is closed and reopened at the next
//           segment.
//

// MOVED TO decoder.[ch]
//  Decoder: For audio, we pay the price of closing and re-opening the decoder.
//           For video, we cache the first packet we read (input_ctx.first_pkt).
//           The pts is set to a sentinel value and fed to the decoder. Once we
//           receive all frames from the decoder OR have sent too many sentinel
//           pkts without receiving anything, then we know the decoder has been
//           fully flushed.

// MOVED TO filter.[ch]
//  Filter:  The challenge here is around fps filter adding and dropping frames.
//           The fps filter expects a strictly monotonic input pts: frames with
//           earlier timestamps get dropped, and frames with too-late timestamps
//           will see a bunch of duplicated frames be generated to catch up with
//           the timestamp that was just inserted. So we cache the last seen
//           frame, rewrite the PTS based on the expected duration, and set a
//           sentinel field (AVFrame.opaque). Then do a lot of rewriting to
//           accommodate changes. See the notes in the filter_ctx struct and the
//           process_out function. This is done for both audio and video.
//
//           XXX No longer true update docs
//           One consequence of this behavior is that we currently cannot
//           process segments out of order, due to the monotonicity requirement.

// MOVED TO encoder.[ch]
// Encoder:  For software encoding, we close the encoder and re-open.
//           For Nvidia encoding, there is luckily an API available via
//           avcodec_flush_buffers to flush the encoder.
//

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
// Transcoder
//

static void close_output(struct output_ctx *octx)
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
  octx->af.flushed = octx->vf.flushed = 0;
  octx->af.flushing = octx->vf.flushing = 0;
  octx->vf.pts_diff = INT64_MIN;
}

static void free_output(struct output_ctx *octx) {
  close_output(octx);
  if (octx->vc) avcodec_free_context(&octx->vc);
  free_filter(&octx->vf);
  free_filter(&octx->af);
}

static int is_mpegts(AVFormatContext *ic) {
  return !strcmp("mpegts", ic->iformat->name);
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
    if (octx->gop_time) {
      // Rescale the gop time to the expected timebase after filtering.
      // The FPS filter outputs pts incrementing by 1 at a rate of 1/framerate
      // while non-fps will retain the input timebase.
      AVRational gop_tb = {1, 1000};
      AVRational dest_tb;
      if (octx->fps.den) dest_tb = av_inv_q(octx->fps);
      else dest_tb = ictx->ic->streams[ictx->vi]->time_base;
      octx->gop_pts_len = av_rescale_q(octx->gop_time, gop_tb, dest_tb);
      octx->next_kf_pts = 0; // force for first frame
    }
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
      if (!vc->hw_frames_ctx) em_err("Unable to alloc hardware context\n");
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
  int reopen_decoders = 1;
  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;
  AVPacket ipkt = {0};
  AVFrame *dframe = NULL;

  if (!inp) main_err("transcoder: Missing input params\n")

  // by default we re-use decoder between segments of same stream
  // unless we are using SW deocder and had to re-open IO or demuxer
  if (!ictx->ic) {
    // reopen demuxer for the input segment if needed
    // XXX could open_input() be re-used here?
    ret = avformat_open_input(&ictx->ic, inp->fname, NULL, NULL);
    if (ret < 0) main_err("Unable to reopen demuxer");
    ret = avformat_find_stream_info(ictx->ic, NULL);
    if (ret < 0) main_err("Unable to find info for reopened stream")
  } else if (!ictx->ic->pb) {
    // reopen input segment file IO context if needed
    ret = avio_open(&ictx->ic->pb, inp->fname, AVIO_FLAG_READ);
    if (ret < 0) main_err("Unable to reopen file");
  } else reopen_decoders = 0;
  if (reopen_decoders) {
    // XXX check to see if we can also reuse decoder for sw decoding
    if (AV_HWDEVICE_TYPE_CUDA != ictx->hw_type) {
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
      if (params[i].gop_time) octx->gop_time = params[i].gop_time;
      octx->dv = ictx->vi < 0 || is_drop(octx->video->name);
      octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
      octx->res = &results[i];

      // first segment of a stream, need to initalize output HW context
      // XXX valgrind this line up
      if (!h->initialized || AV_HWDEVICE_TYPE_NONE == octx->hw_type) {
        ret = open_output(octx, ictx);
        if (ret < 0) main_err("transcoder: Unable to open output");
        continue;
      }

      // non-first segment
      // re-open muxer for HW encoding
      AVOutputFormat *fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
      if (!fmt) main_err("Unable to guess format for reopen\n");
      ret = avformat_alloc_output_context2(&octx->oc, fmt, NULL, octx->fname);
      if (ret < 0) main_err("Unable to alloc reopened out context\n");

      // re-attach video encoder
      if (octx->vc) {
        ret = add_video_stream(octx, ictx);
        if (ret < 0) main_err("Unable to re-add video stream\n");
      } else fprintf(stderr, "no video stream\n");

      // re-attach audio encoder
      ret = open_audio_output(ictx, octx, fmt);
      if (ret < 0) main_err("Unable to re-add audio stream\n");

      if (!(fmt->flags & AVFMT_NOFILE)) {
        ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
        if (ret < 0) main_err("Error re-opening output file\n");
      }
      ret = avformat_write_header(octx->oc, &octx->muxer->opts);
      if (ret < 0) main_err("Error re-writing header\n");
  }

  av_init_packet(&ipkt);
  dframe = av_frame_alloc();
  if (!dframe) main_err("transcoder: Unable to allocate frame\n");

  while (1) {
    // DEMUXING & DECODING
    int has_frame = 0;
    AVStream *ist = NULL;
    AVFrame *last_frame = NULL;
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
      has_frame = has_frame && dframe->width && dframe->height;
      if (has_frame) last_frame = ictx->last_frame_v;
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      has_frame = has_frame && dframe->nb_samples;
      if (has_frame) last_frame = ictx->last_frame_a;
    }
    if (has_frame) {
      int64_t dur = 0;
      if (dframe->pkt_duration) dur = dframe->pkt_duration;
      else if (ist->r_frame_rate.den) {
        dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
      } else {
        // TODO use better heuristics for this; look at how ffmpeg does it
        fprintf(stderr, "Could not determine next pts; filter might drop\n");
      }
      dframe->pkt_duration = dur;
      av_frame_unref(last_frame);
      av_frame_ref(last_frame, dframe);
    }

    // ENCODING & MUXING OF ALL OUTPUT RENDITIONS
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
  if (ictx->ic) {
    // Only mpegts reuse the demuxer for subsequent segments.
    // Close the demuxer for everything else.
    // TODO might be reusable with fmp4 ; check!
    if (!is_mpegts(ictx->ic)) avformat_close_input(&ictx->ic);
    else if (ictx->ic->pb) {
      // Reset leftovers from demuxer internals to prepare for next segment
      avio_flush(ictx->ic->pb);
      avformat_flush(ictx->ic);
      avio_closep(&ictx->ic->pb);
    }
  }
  if (dframe) av_frame_free(&dframe);
  ictx->flushed = 0;
  ictx->flushing = 0;
  ictx->pkt_diff = 0;
  ictx->sentinel_count = 0;
  av_packet_unref(&ipkt);  // needed for early exits
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) close_output(&outputs[i]);
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
  }

  free(handle);
}
