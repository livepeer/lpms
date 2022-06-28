#include "encoder.h"
#include "logging.h"

#include <libavcodec/avcodec.h>
#include <libavfilter/buffersrc.h>
#include <libavfilter/buffersink.h>

static AVStream *add_stream_copy(struct output_ctx *octx, AVStream *ist)
{
  int ret = 0;
  if (!ist) LPMS_ERR(add_copy_err, "Input stream for copy not available");
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) LPMS_ERR(add_copy_err, "Unable to alloc copy stream");
  octx->vi = st->index;
  if (octx->fps.den) st->avg_frame_rate = octx->fps;
  else st->avg_frame_rate = ist->r_frame_rate;
  st->time_base = ist->time_base;
  ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
  if (ret < 0) LPMS_ERR(add_copy_err, "Error copying params from input stream");
  // Sometimes the codec tag is wonky for some reason, so correct it
  ret = av_codec_get_tag2(octx->oc->oformat->codec_tag, st->codecpar->codec_id, &st->codecpar->codec_tag);
  avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist, AVFMT_TBCF_DEMUXER);
  return st;

add_copy_err:
  return NULL;
}

static AVStream *add_stream_for_encoder(struct output_ctx *octx, AVCodecContext *encoder)
{
  int ret = 0;
  AVStream *st = avformat_new_stream(octx->oc, NULL);
  if (!st) LPMS_ERR(add_encoder_err, "Unable to alloc encoder stream");

  st->time_base = encoder->time_base;
  ret = avcodec_parameters_from_context(st->codecpar, encoder);
  if (ret < 0) LPMS_ERR(add_encoder_err, "Error setting stream params from encoder");
  return st;

add_encoder_err:
  return NULL;
}

static int add_video_stream(struct output_ctx *octx, struct input_ctx *ictx)
{
  // video stream to muxer
  int ret = 0;
  AVStream *st = NULL;
  if (is_copy(octx->video->name)) {
    // create stream as a copy of existing one
    if (ictx->vi < 0) LPMS_ERR(add_video_err, "Input video stream does not exist");
    st = add_stream_copy(octx, ictx->ic->streams[ictx->vi]);
    if (!st) LPMS_ERR(add_video_err, "Error adding video copy stream");
  } else if (octx->vc) {
    // create stream from encoder
    st = add_stream_for_encoder(octx, octx->vc);
    if (!st) LPMS_ERR(add_video_err, "Error adding video encoder stream");
    // Video has rescale here. Audio is slightly different
    // Rescale the gop/clip time to the expected timebase after filtering.
    // The FPS filter outputs pts incrementing by 1 at a rate of 1/framerate
    // while non-fps will retain the input timebase.
    AVRational ms_tb = {1, 1000};
    AVRational dest_tb;
    if (octx->fps.den) dest_tb = av_inv_q(octx->fps);
    else dest_tb = ictx->ic->streams[ictx->vi]->time_base;
    if (octx->gop_time) {
      octx->gop_pts_len = av_rescale_q(octx->gop_time, ms_tb, dest_tb);
      octx->next_kf_pts = 0; // force for first frame
    }
    if (octx->clip_from) {
      octx->clip_from_pts = av_rescale_q(octx->clip_from, ms_tb, dest_tb);
    }
    if (octx->clip_to) {
      octx->clip_to_pts = av_rescale_q(octx->clip_to, ms_tb, dest_tb);
    }
  } else if (is_drop(octx->video->name)) {
    LPMS_ERR(add_video_err, "add_video_stream called for dropped video!");
  } else LPMS_ERR(add_video_err, "No video encoder, not a copy; what is this?");
  return 0;

add_video_err:
  // XXX free anything here?
  return ret;
}

static int add_audio_stream(struct input_ctx *ictx, struct output_ctx *octx)
{
  // TODO: remove this? already handled in create_output
  if (ictx->ai < 0 || octx->da) {
    // Don't need to add an audio stream if no input audio exists,
    // or we're dropping the output audio stream
    return 0;
  }

  // audio stream to muxer
  int ret = 0;
  AVStream *st = NULL;
  if (is_copy(octx->audio->name)) {
    // create stream as a copy of existing one
    if (ictx->ai < 0) LPMS_ERR(add_audio_err, "Input audio stream does not exist");
    st = add_stream_copy(octx, ictx->ic->streams[ictx->ai]);
  } else if (octx->ac) {
    // create stream from encoder
    st = add_stream_for_encoder(octx, octx->ac);
    // Video has rescale here
  } else if (is_drop(octx->audio->name)) {
    // Supposed to exit this function early if there's a drop
    LPMS_ERR(add_audio_err, "add_audio_stream called for dropped audio!");
  } else {
    LPMS_ERR(add_audio_err, "No audio encoder; not a copy; what is this?");
  }

  if (!st) LPMS_ERR(add_audio_err, "Error adding video copy stream");
  octx->ai = st->index;

  // Audio has rescale here. Video version is slightly different
  AVRational ms_tb = {1, 1000};
  AVRational dest_tb = ictx->ic->streams[ictx->ai]->time_base;
  if (octx->clip_from) {
    octx->clip_audio_from_pts = av_rescale_q(octx->clip_from, ms_tb, dest_tb);
  }
  if (octx->clip_to) {
    octx->clip_audio_to_pts = av_rescale_q(octx->clip_to, ms_tb, dest_tb);
  }

  // signal whether to drop preroll audio
  if (st->codecpar->initial_padding) octx->drop_ts = AV_NOPTS_VALUE;

  octx->last_audio_dts = AV_NOPTS_VALUE;

  return 0;

add_audio_err:
  // XXX free anything here?
  return ret;
}

// Add all streams provided by the demuxer. This is used in transmuxing
// scenarios
int add_remux_streams(struct input_ctx *ictx, struct output_ctx *octx)
{
  octx->oc->flags |= AVFMT_FLAG_FLUSH_PACKETS;
  octx->oc->flush_packets = 1;
  for (int i = 0; i < ictx->ic->nb_streams; i++) {
    AVStream *st = add_stream_copy(octx, ictx->ic->streams[i]);
    if (!st) return -1; // error logged in add_stream_copy
  }
  return 0;
}


static int open_audio_encoder(struct input_ctx *ictx, struct output_ctx *octx,
  AVOutputFormat *fmt)
{
  int ret = 0;
  AVCodec *codec = NULL;
  AVCodecContext *ac = NULL;

  // initialize audio filters
  ret = init_audio_filters(ictx, octx);
  if (ret < 0) LPMS_ERR(audio_encoder_err, "Unable to open audio filter")

  // open encoder
  codec = avcodec_find_encoder_by_name(octx->audio->name);
  if (!codec) LPMS_ERR(audio_encoder_err, "Unable to find audio encoder");
  // open audio encoder
  ac = avcodec_alloc_context3(codec);
  if (!ac) LPMS_ERR(audio_encoder_err, "Unable to alloc audio encoder");
  octx->ac = ac;
  ac->sample_fmt = av_buffersink_get_format(octx->af.sink_ctx);
  ac->channel_layout = av_buffersink_get_channel_layout(octx->af.sink_ctx);
  ac->channels = av_buffersink_get_channels(octx->af.sink_ctx);
  ac->sample_rate = av_buffersink_get_sample_rate(octx->af.sink_ctx);
  ac->time_base = av_buffersink_get_time_base(octx->af.sink_ctx);
  if (fmt->flags & AVFMT_GLOBALHEADER) ac->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  ret = avcodec_open2(ac, codec, &octx->audio->opts);
  if (ret < 0) LPMS_ERR(audio_encoder_err, "Error opening audio encoder");
  av_buffersink_set_frame_size(octx->af.sink_ctx, ac->frame_size);

audio_encoder_err:
  // TODO clean up anything here?
  return ret;
}

static int open_video_encoder(struct input_ctx *ictx, struct output_ctx *octx,
                              AVOutputFormat *fmt)
{
  AVCodecContext *vc  = NULL;
  AVCodec *codec      = NULL;
  int ret = 0;
  // add video encoder if a decoder exists and this output requires one
  if (octx->dnn_filtergraph && !ictx->vc->hw_frames_ctx) {
    // swap filtergraph with the pre-initialized DNN filtergraph for SW
    // for HW we handle it later during filter re-init
    octx->vf.graph = *octx->dnn_filtergraph;
  }
  ret = init_video_filters(ictx, octx);
  if (ret < 0) LPMS_ERR(video_encoder_err, "Unable to open video filter");

  codec = avcodec_find_encoder_by_name(octx->video->name);
  if (!codec) LPMS_ERR(video_encoder_err, "Unable to find encoder");

  // open video encoder
  // XXX use avoptions rather than manual enumeration
  // (MA: dunno what the original author had in mind, avcodec_copy_context)
  vc = avcodec_alloc_context3(codec);
  if (!vc) LPMS_ERR(video_encoder_err, "Unable to alloc video encoder");
  octx->vc = vc;
  vc->width = av_buffersink_get_w(octx->vf.sink_ctx);
  vc->height = av_buffersink_get_h(octx->vf.sink_ctx);
  if (octx->fps.den) vc->framerate = av_buffersink_get_frame_rate(octx->vf.sink_ctx);
  else if (ictx->vc->framerate.num && ictx->vc->framerate.den) vc->framerate = ictx->vc->framerate;
  else vc->framerate = ictx->ic->streams[ictx->vi]->r_frame_rate;
  if (octx->fps.den) vc->time_base = av_buffersink_get_time_base(octx->vf.sink_ctx);
  else if (ictx->vc->time_base.num && ictx->vc->time_base.den) vc->time_base = ictx->vc->time_base;
  else vc->time_base = ictx->ic->streams[ictx->vi]->time_base;
  if (octx->bitrate) vc->rc_min_rate = vc->bit_rate = vc->rc_max_rate = vc->rc_buffer_size = octx->bitrate;
  if (av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx)) {
    vc->hw_frames_ctx =
      av_buffer_ref(av_buffersink_get_hw_frames_ctx(octx->vf.sink_ctx));
    if (!vc->hw_frames_ctx) LPMS_ERR(video_encoder_err, "Unable to alloc hardware context");
  }
  vc->pix_fmt = av_buffersink_get_format(octx->vf.sink_ctx); // XXX select based on encoder + input support
  if (fmt->flags & AVFMT_GLOBALHEADER) vc->flags |= AV_CODEC_FLAG_GLOBAL_HEADER;
  if(strcmp(octx->xcoderParams,"")!=0){
    av_opt_set(vc->priv_data, "xcoder-params", octx->xcoderParams, 0);
  }
  ret = avcodec_open2(vc, codec, &octx->video->opts);
  if (ret < 0) LPMS_ERR(video_encoder_err, "Error opening video encoder");
  octx->hw_type = ictx->hw_type;
video_encoder_err:
  return ret;
}

void close_output(struct output_ctx *octx)
{
  if (octx->oc) {
    if (!(octx->oc->oformat->flags & AVFMT_NOFILE) && octx->oc->pb) {
      avio_closep(&octx->oc->pb);
    }
    avformat_free_context(octx->oc);
    octx->oc = NULL;
  }
  if (octx->vc && octx->hw_type == AV_HWDEVICE_TYPE_NONE) avcodec_free_context(&octx->vc);
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
  free_filter(&octx->sf);
}

int open_output(struct output_ctx *octx, struct input_ctx *ictx)
{
  int ret = 0;

  AVOutputFormat *fmt = NULL;

  // First thing to do is to get muxer format. It is needed for encoder
  // initialization.
  fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt) LPMS_ERR(open_output_err, "Unable to guess output format");

  // Encoders block - video and audio
  // add video encoder if needed and don't have one yet
  if (ictx->vc && needs_decoder(octx->video->name) && !octx->vc) {
    ret = open_video_encoder(ictx, octx, fmt);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening video output");
  }

  // add audio encoder if needed
  if (ictx->ac && needs_decoder(octx->audio->name)) {
    ret = open_audio_encoder(ictx, octx, fmt);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening audio output");
  }

  // Now that the encoders are available, muxer can be created
  ret = avformat_alloc_output_context2(&octx->oc, fmt, NULL, octx->fname);
  if (ret < 0) LPMS_ERR(open_output_err, "Unable to alloc output context");

  // Add streams
  if (ictx->transmuxing) {
    // when transmuxing we add all the streams to the output muxer
    ret = add_remux_streams(ictx, octx);
    if (ret < 0) LPMS_ERR(open_output_err, "Error adding remux output");
  } else {
    // add video stream if needed
    if (!octx->dv) {
      ret = add_video_stream(octx, ictx);
      if (ret < 0) LPMS_ERR(open_output_err, "Error adding video stream");
    }

    // add audio stream if needed
    if (!octx->da) {
      ret = add_audio_stream(ictx, octx);
      if (ret < 0) LPMS_ERR(open_output_err, "Error adding audio stream")
    }
  }

  // Muxer headers can be written now once streams were added
  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) LPMS_ERR(open_output_err, "Error opening output file");
  }

  ret = avformat_write_header(octx->oc, &octx->muxer->opts);
  if (ret < 0) LPMS_ERR(open_output_err, "Error writing header");

  if(octx->sfilters != NULL && needs_decoder(octx->video->name) && octx->sf.active == 0) {
    ret = init_signature_filters(octx, NULL);
    if (ret < 0) LPMS_ERR(open_output_err, "Unable to open signature filter");
  }

  return 0;

open_output_err:
  free_output(octx);
  return ret;
}

static int encode(AVCodecContext* encoder, AVFrame *frame, struct output_ctx* octx, AVStream* ost)
{
  int ret = 0;
  AVPacket *pkt = NULL;

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type && frame) {
    if (!octx->res->frames) {
      frame->pict_type = AV_PICTURE_TYPE_I;
    }
    octx->res->frames++;
    octx->res->pixels += encoder->width * encoder->height;
  }

  // We don't want to send NULL frames for HW encoding
  // because that closes the encoder: not something we want
  if (AV_HWDEVICE_TYPE_NONE == octx->hw_type || AV_HWDEVICE_TYPE_MEDIACODEC == octx->hw_type ||
        AVMEDIA_TYPE_AUDIO == ost->codecpar->codec_type || frame) {
    ret = avcodec_send_frame(encoder, frame);
    if (AVERROR_EOF == ret) ; // continue ; drain encoder
    else if (ret < 0) LPMS_ERR(encode_cleanup, "Error sending frame to encoder");
  }

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type &&
      AV_HWDEVICE_TYPE_CUDA == octx->hw_type && !frame) {
    avcodec_flush_buffers(encoder);
  }

  pkt = av_packet_alloc();
  if (!pkt) {
      ret = AVERROR(ENOMEM);
      LPMS_ERR(encode_cleanup, "Error allocating packet for encode");
  }
  while (1) {
    av_packet_unref(pkt);
    ret = avcodec_receive_packet(encoder, pkt);
    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) goto encode_cleanup;
    if (ret < 0) LPMS_ERR(encode_cleanup, "Error receiving packet from encoder");
    ret = mux(pkt, encoder->time_base, octx, ost);
    if (ret < 0) goto encode_cleanup;
  }

encode_cleanup:
  if (pkt) av_packet_free(&pkt);
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

      if (pkt->dts != AV_NOPTS_VALUE && pkt->pts != AV_NOPTS_VALUE && pkt->dts > pkt->pts) {
        pkt->pts = pkt->dts = pkt->pts + pkt->dts + octx->last_audio_dts + 1
                     - FFMIN3(pkt->pts, pkt->dts, octx->last_audio_dts + 1)
                     - FFMAX3(pkt->pts, pkt->dts, octx->last_audio_dts + 1);
      }
      /*https://github.com/livepeer/FFmpeg/blob/682c4189d8364867bcc49f9749e04b27dc37cded/fftools/ffmpeg.c#L824*/
      if (pkt->dts != AV_NOPTS_VALUE && octx->last_audio_dts != AV_NOPTS_VALUE) {
        /*If the out video format does not require strictly increasing timestamps,
        but they must still be monotonic, then let set max timestamp as octx->last_audio_dts+1.*/
        int64_t max = octx->last_audio_dts + !(octx->oc->oformat->flags & AVFMT_TS_NONSTRICT);
        // check if dts is bigger than previous last dts or not, not then that's non-monotonic
        if (pkt->dts < max) {
          if (pkt->pts >= pkt->dts) pkt->pts = FFMAX(pkt->pts, max);
          pkt->dts = max;
        }
      }
      octx->last_audio_dts = pkt->dts;
  }

  return av_interleaved_write_frame(octx->oc, pkt);
}

static int getmetadatainf(AVFrame *inf, struct output_ctx *octx)
{
  if(inf == NULL) return -1;
  char classinfo[128] = {0,};
  AVDictionaryEntry *element = NULL;
  AVDictionary *metadata = inf->metadata;

  if(metadata != NULL) {
    element = av_dict_get(metadata, LVPDNN_FILTER_META, element, 0);
    if(element != NULL) {
      strcpy(classinfo, element->value);
      if(strlen(classinfo) > 0) {
        char * token = strtok(classinfo, ",");
        int cid = 0;
        while( token != NULL ) {
            octx->res->probs[cid] += atof(token);
            token = strtok(NULL, ",");
            cid++;
        }
        octx->res->frames++;
      }
    }
  }
  return 0;
}

static int calc_signature(AVFrame *inf, struct output_ctx *octx)
{
  int ret = 0;
  if (inf->hw_frames_ctx && octx->sf.hwframes && inf->hw_frames_ctx->data != octx->sf.hwframes) {
      free_filter(&octx->sf);
      ret = init_signature_filters(octx, inf);
      if (ret < 0) return lpms_ERR_FILTERS;
  }
  ret = av_buffersrc_write_frame(octx->sf.src_ctx, inf);
  if (ret < 0) return ret;
  AVFrame *signframe = octx->sf.frame;
  av_frame_unref(signframe);
  ret = av_buffersink_get_frame(octx->sf.sink_ctx, signframe);
  return ret;
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
  int is_audio = (AVMEDIA_TYPE_AUDIO == ost->codecpar->codec_type);
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

    if (is_video && !octx->clip_start_pts_found && frame) {
      octx->clip_start_pts = frame->pts;
      octx->clip_start_pts_found = 1;
    }
    if (is_audio && !octx->clip_audio_start_pts_found && frame) {
      octx->clip_audio_start_pts = frame->pts;
      octx->clip_audio_start_pts_found = 1;
    }


    if (is_video && octx->clip_to && octx->clip_start_pts_found && frame && frame->pts > octx->clip_to_pts + octx->clip_start_pts) goto skip;
    if (is_audio && octx->clip_to && octx->clip_audio_start_pts_found && frame && frame->pts > octx->clip_audio_to_pts + octx->clip_audio_start_pts) {
      goto skip;
    }

    if (is_video) {
      if (octx->clip_from && frame) {
        if (frame->pts < octx->clip_from_pts + octx->clip_start_pts)  goto skip;
        if (!octx->clip_started) {
          octx->clip_started = 1;
          frame->pict_type = AV_PICTURE_TYPE_I;
          if (octx->gop_pts_len) {
            octx->next_kf_pts = frame->pts + octx->gop_pts_len;
          }
        }
        if (octx->clip_from && frame) {
          frame->pts -= octx->clip_from_pts + octx->clip_start_pts;
        }
      }
    } else if (octx->clip_from_pts && !octx->clip_started) {
      // we want first frame to be video frame
      goto skip;
    }
    if (is_audio && octx->clip_from && frame && frame->pts < octx->clip_audio_from_pts + octx->clip_audio_start_pts) {
      goto skip;
    }
    if (is_audio && octx->clip_from && frame) {
      frame->pts -= octx->clip_audio_from_pts + octx->clip_audio_start_pts;
    }

    // Set GOP interval if necessary
    if (is_video && octx->gop_pts_len && frame && frame->pts >= octx->next_kf_pts) {
        frame->pict_type = AV_PICTURE_TYPE_I;
        octx->next_kf_pts = frame->pts + octx->gop_pts_len;
    }

    if(octx->is_dnn_profile) {
      ret = getmetadatainf(frame, octx);
      if(ret == -1 && frame == NULL) {
        // Return EOF in case of flushing procedure
        ret = AVERROR_EOF;
      }
    } else {
      if(is_video && frame != NULL && octx->sfilters != NULL) {
         ret = calc_signature(frame, octx);
         if(ret < 0) LPMS_WARN("Could not calculate signature value for frame");
      }
      ret = encode(encoder, frame, octx, ost);
    }
skip:
    av_frame_unref(frame);
    // For HW we keep the encoder open so will only get EAGAIN.
    // Return EOF in place of EAGAIN for to terminate the flush
    if (frame == NULL && octx->hw_type > AV_HWDEVICE_TYPE_NONE &&
            AV_HWDEVICE_TYPE_MEDIACODEC != octx->hw_type &&
            AVERROR(EAGAIN) == ret && !inf) return AVERROR_EOF;
    if (frame == NULL) return ret;
  }

proc_cleanup:
  return ret;
}

