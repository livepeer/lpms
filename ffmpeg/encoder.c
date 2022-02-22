#include "encoder.h"
#include "logging.h"

#include <libavcodec/avcodec.h>
#include <libavfilter/buffersrc.h>
#include <libavfilter/buffersink.h>

static int add_video_stream(struct output_ctx *octx, struct input_ctx *ictx)
{
  // video stream to muxer
  int ret = 0;
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) LPMS_ERR(add_video_err, "Unable to alloc video stream");
  octx->vi = st->index;
  if (octx->fps.den) st->avg_frame_rate = octx->fps;
  else st->avg_frame_rate = ictx->ic->streams[ictx->vi]->r_frame_rate;
  if (is_copy(octx->video->name)) {
    AVStream *ist = ictx->ic->streams[ictx->vi];
    if (ictx->vi < 0 || !ist) LPMS_ERR(add_video_err, "Input video stream does not exist");
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0) LPMS_ERR(add_video_err, "Error copying video params from input stream");
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
    if (ret < 0) LPMS_ERR(add_video_err, "Error setting video params from encoder");
  } else LPMS_ERR(add_video_err, "No video encoder, not a copy; what is this?");
  return 0;

add_video_err:
  // XXX free anything here?
  return ret;
}

static int add_audio_stream(struct input_ctx *ictx, struct output_ctx *octx)
{
  if (ictx->ai < 0 || octx->da) {
    // Don't need to add an audio stream if no input audio exists,
    // or we're dropping the output audio stream
    return 0;
  }

  // audio stream to muxer
  int ret = 0;
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) LPMS_ERR(add_audio_err, "Unable to alloc audio stream");
  if (is_copy(octx->audio->name)) {
    AVStream *ist = ictx->ic->streams[ictx->ai];
    if (ictx->ai < 0 || !ist) LPMS_ERR(add_audio_err, "Input audio stream does not exist");
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0) LPMS_ERR(add_audio_err, "Error copying audio params from input stream");
    // Sometimes the codec tag is wonky for some reason, so correct it
    ret = av_codec_get_tag2(octx->oc->oformat->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
    avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist, AVFMT_TBCF_DEMUXER);
  } else if (octx->ac) {
    st->time_base = octx->ac->time_base;
    ret = avcodec_parameters_from_context(st->codecpar, octx->ac);
    if (ret < 0) LPMS_ERR(add_audio_err, "Error setting audio params from encoder");
  } else if (is_drop(octx->audio->name)) {
    // Supposed to exit this function early if there's a drop
    LPMS_ERR(add_audio_err, "Shouldn't ever happen here");
  } else {
    LPMS_ERR(add_audio_err, "No audio encoder; not a copy; what is this?");
  }
  octx->ai = st->index;

  // signal whether to drop preroll audio
  if (st->codecpar->initial_padding) octx->drop_ts = AV_NOPTS_VALUE;
  return 0;

add_audio_err:
  // XXX free anything here?
  return ret;
}

static int open_audio_output(struct input_ctx *ictx, struct output_ctx *octx,
  AVOutputFormat *fmt)
{
  int ret = 0;
  AVCodec *codec = NULL;
  AVCodecContext *ac = NULL;

  // add audio encoder if a decoder exists and this output requires one
  if (ictx->ac && needs_decoder(octx->audio->name)) {

    // initialize audio filters
    ret = init_audio_filters(ictx, octx);
    if (ret < 0) LPMS_ERR(audio_output_err, "Unable to open audio filter")

    // open encoder
    codec = avcodec_find_encoder_by_name(octx->audio->name);
    if (!codec) LPMS_ERR(audio_output_err, "Unable to find audio encoder");
    // open audio encoder
    ac = avcodec_alloc_context3(codec);
    if (!ac) LPMS_ERR(audio_output_err, "Unable to alloc audio encoder");
    octx->ac = ac;
    ac->sample_fmt = av_buffersink_get_format(octx->af.sink_ctx);
    ac->channel_layout = av_buffersink_get_channel_layout(octx->af.sink_ctx);
    ac->channels = av_buffersink_get_channels(octx->af.sink_ctx);
    ac->sample_rate = av_buffersink_get_sample_rate(octx->af.sink_ctx);
    ac->time_base = av_buffersink_get_time_base(octx->af.sink_ctx);
    if (fmt->flags & AVFMT_GLOBALHEADER) ac->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
    ret = avcodec_open2(ac, codec, &octx->audio->opts);
    if (ret < 0) LPMS_ERR(audio_output_err, "Error opening audio encoder");
    av_buffersink_set_frame_size(octx->af.sink_ctx, ac->frame_size);
  }

  ret = add_audio_stream(ictx, octx);
  if (ret < 0) LPMS_ERR(audio_output_err, "Error adding audio stream")

audio_output_err:
  // TODO clean up anything here?
  return ret;
}

void close_output(struct output_ctx *octx)
{
   printf("close_output octx->fname %s\n", octx->fname);
  if (octx->oc) {
    if (!(octx->oc->oformat->flags & AVFMT_NOFILE) && octx->oc->pb) {
      avio_closep(&octx->oc->pb);
    }
    avformat_free_context(octx->oc);
    octx->oc = NULL;
  }
  if (octx->vc && AV_HWDEVICE_TYPE_MEDIACODEC == octx->hw_type) avcodec_free_context(&octx->vc);
  if (octx->ac) avcodec_free_context(&octx->ac);
  octx->af.flushed = octx->vf.flushed = 0;
  octx->af.flushing = octx->vf.flushing = 0;
  octx->vf.pts_diff = INT64_MIN;
}

void free_output(struct output_ctx *octx)
{
  close_output(octx);
  if (octx->vc) avcodec_free_context(&octx->vc);
  free_filter(&octx->vf);
  free_filter(&octx->af);
}

int open_remux_output(struct input_ctx *ictx, struct output_ctx *octx)
{
  int ret = 0;
  octx->oc->flags |= AVFMT_FLAG_FLUSH_PACKETS;
  octx->oc->flush_packets = 1;
  for (int i = 0; i < ictx->ic->nb_streams; i++) {
    ret = 0;
    AVStream *st = avformat_new_stream(octx->oc, NULL);
    if (!st) LPMS_ERR(open_output_err, "Unable to alloc stream");
    if (octx->fps.den)
      st->avg_frame_rate = octx->fps;
    else
      st->avg_frame_rate = ictx->ic->streams[i]->r_frame_rate;

    AVStream *ist = ictx->ic->streams[i];
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0)
      LPMS_ERR(open_output_err, "Error copying params from input stream");
    // Sometimes the codec tag is wonky for some reason, so correct it
    ret = av_codec_get_tag2(octx->oc->oformat->codec_tag,
                            st->codecpar->codec_id, &st->codecpar->codec_tag);
    avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist,
                                                  AVFMT_TBCF_DEMUXER);

  }
  return 0;
open_output_err:
  return ret;
}

int open_output(struct output_ctx *octx, struct input_ctx *ictx)
{
  int ret = 0, inp_has_stream;

  AVOutputFormat *fmt = NULL;
  AVFormatContext *oc = NULL;
  AVCodecContext *vc  = NULL;
  AVCodec *codec      = NULL;

  printf("open_output octx->fname %s\n", octx->fname);

  // open muxer
  fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt) LPMS_ERR(open_output_err, "Unable to guess output format");
  ret = avformat_alloc_output_context2(&oc, fmt, NULL, octx->fname);
  if (ret < 0) LPMS_ERR(open_output_err, "Unable to alloc output context");
  octx->oc = oc;

  // add video encoder if a decoder exists and this output requires one
  if (ictx->vc && needs_decoder(octx->video->name)) {
    ret = init_video_filters(ictx, octx);
    if (ret < 0) LPMS_ERR(open_output_err, "Unable to open video filter");

    codec = avcodec_find_encoder_by_name(octx->video->name);
    if (!codec) LPMS_ERR(open_output_err, "Unable to find encoder");

    // open video encoder
    // XXX use avoptions rather than manual enumeration
    vc = avcodec_alloc_context3(codec);
    if (!vc) LPMS_ERR(open_output_err, "Unable to alloc video encoder");
    octx->vc = vc;
    vc->width = av_buffersink_get_w(octx->vf.sink_ctx);
    vc->height = av_buffersink_get_h(octx->vf.sink_ctx);
    if (octx->fps.den) vc->framerate = av_buffersink_get_frame_rate(octx->vf.sink_ctx);
    else if (ictx->vc->framerate.num && ictx->vc->framerate.den) vc->framerate = ictx->vc->framerate;
    else vc->framerate = ictx->ic->streams[ictx->vi]->r_frame_rate;
    if (octx->fps.den) vc->time_base = av_buffersink_get_time_base(octx->vf.sink_ctx);
    else if (ictx->vc->time_base.num && ictx->vc->time_base.den) vc->time_base = ictx->vc->time_base;
    else vc->time_base = ictx->ic->streams[ictx->vi]->time_base;
    if (octx->bitrate) vc->rc_min_rate = vc->rc_max_rate = vc->rc_buffer_size = octx->bitrate;
    if (av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx)) {
      vc->hw_frames_ctx =
        av_buffer_ref(av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx));
      if (!vc->hw_frames_ctx) LPMS_ERR(open_output_err, "Unable to alloc hardware context");
    }
    vc->pix_fmt = av_buffersink_get_format(octx->vf.sink_ctx); // XXX select based on encoder + input support
    if (fmt->flags & AVFMT_GLOBALHEADER) vc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
	if(strcmp(ictx->xcoderParams,"")!=0){
	    av_opt_set(vc->priv_data, "xcoder-params", ictx->xcoderParams, 0);
		//printf("xcoder-params %s\n", ictx->xcoderParams);
	}

    ret = avcodec_open2(vc, codec, &octx->video->opts);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening video encoder");
    octx->hw_type = ictx->hw_type;
  }

  if (!ictx->transmuxing) {
    // add video stream if input contains video
    inp_has_stream = ictx->vi >= 0;
    if (inp_has_stream && !octx->dv) {
      ret = add_video_stream(octx, ictx);
      if (ret < 0) LPMS_ERR(open_output_err, "Error adding video stream");
    }

    ret = open_audio_output(ictx, octx, fmt);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening audio output");
  } else {
    ret = open_remux_output(ictx, octx);
    if (ret < 0) {
      goto open_output_err;
    }
  }

  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening output file");
  }

  ret = avformat_write_header(oc, &octx->muxer->opts);
  if (ret < 0) LPMS_ERR(open_output_err, "Error writing header");

  return 0;

open_output_err:
  free_output(octx);
  return ret;
}

int reopen_output(struct output_ctx *octx, struct input_ctx *ictx)
{
  int ret = 0;
  // re-open muxer for HW encoding
  AVOutputFormat *fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt) LPMS_ERR(reopen_out_err, "Unable to guess format for reopen");
  ret = avformat_alloc_output_context2(&octx->oc, fmt, NULL, octx->fname);
  if (ret < 0) LPMS_ERR(reopen_out_err, "Unable to alloc reopened out context");

  // re-attach video encoder
  if (octx->vc) {
    ret = add_video_stream(octx, ictx);
    if (ret < 0) LPMS_ERR(reopen_out_err, "Unable to re-add video stream");
  } else LPMS_INFO("No video stream!?");

  // re-attach audio encoder
  ret = open_audio_output(ictx, octx, fmt);
  if (ret < 0) LPMS_ERR(reopen_out_err, "Unable to re-add audio stream");

  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) LPMS_ERR(reopen_out_err, "Error re-opening output file");
  }
  ret = avformat_write_header(octx->oc, &octx->muxer->opts);
  if (ret < 0) LPMS_ERR(reopen_out_err, "Error re-writing header");

reopen_out_err:
  return ret;
}

static int encode(AVCodecContext* encoder, AVFrame *frame, struct output_ctx* octx, AVStream* ost)
{
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
  if (AV_HWDEVICE_TYPE_MEDIACODEC == octx->hw_type || frame) {
    ret = avcodec_send_frame(encoder, frame);
    if (AVERROR_EOF == ret) ; // continue ; drain encoder
    else if (ret < 0) LPMS_ERR(encode_cleanup, "Error sending frame to encoder");
  }

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type &&
      AV_HWDEVICE_TYPE_MEDIACODEC == octx->hw_type && !frame) {
    avcodec_flush_buffers(encoder);
  }

  while (1) {
    av_init_packet(&pkt);
    ret = avcodec_receive_packet(encoder, &pkt);
    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) goto encode_cleanup;
    if (ret < 0) LPMS_ERR(encode_cleanup, "Error receiving packet from encoder");
    ret = mux(&pkt, encoder->time_base, octx, ost);
    if (ret < 0) goto encode_cleanup;
    av_packet_unref(&pkt);
  }

encode_cleanup:
  av_packet_unref(&pkt);
  return ret;
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

int process_out(struct input_ctx *ictx, struct output_ctx *octx, AVCodecContext *encoder, AVStream *ost,
  struct filter_ctx *filter, AVFrame *inf)
{
  int ret = 0;

  if (!encoder) LPMS_ERR(proc_cleanup, "Trying to transmux; not supported")

  if (!filter || !filter->active) {
    // No filter in between decoder and encoder, so use input frame directly
    return encode(encoder, inf, octx, ost);
  }

  int is_video = (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type);
  ret = filtergraph_write(inf, ictx, octx, filter, is_video);
  if (ret < 0) goto proc_cleanup;

  while (1) {
    // Drain the filter. Each input frame may have multiple output frames
    AVFrame *frame = filter->frame;
    ret = filtergraph_read(ictx, octx, filter, is_video);
    if (ret == lpms_ERR_FILTER_FLUSHED) continue;
    else if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) {
      // no frame returned from filtergraph
      // proceed only if the input frame is a flush (inf == null)
      if (inf) return ret;
      frame = NULL;
    } else if (ret < 0) goto proc_cleanup;

    // Set GOP interval if necessary
    if (is_video && octx->gop_pts_len && frame && frame->pts >= octx->next_kf_pts) {
        frame->pict_type = AV_PICTURE_TYPE_I;
        octx->next_kf_pts = frame->pts + octx->gop_pts_len;
    }
    ret = encode(encoder, frame, octx, ost);
    av_frame_unref(frame);
    // For HW we keep the encoder open so will only get EAGAIN.
    // Return EOF in place of EAGAIN for to terminate the flush
    if (frame == NULL && AV_HWDEVICE_TYPE_MEDIACODEC != octx->hw_type &&
        AVERROR(EAGAIN) == ret && !inf) return AVERROR_EOF;
    if (frame == NULL) return ret;
  }

proc_cleanup:
  return ret;
}

