#include "transcoder.h"
#include "decoder.h"
#include "filter.h"
#include "encoder.h"
#include "logging.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <libavfilter/buffersrc.h>
#include <stdbool.h>

// Not great to appropriate internal API like this...
const int lpms_ERR_INPUT_PIXFMT = FFERRTAG('I','N','P','X');
const int lpms_ERR_INPUT_CODEC = FFERRTAG('I','N','P','C');
const int lpms_ERR_INPUT_NOKF = FFERRTAG('I','N','K','F');
const int lpms_ERR_FILTERS = FFERRTAG('F','L','T','R');
const int lpms_ERR_PACKET_ONLY = FFERRTAG('P','K','O','N');
const int lpms_ERR_FILTER_FLUSHED = FFERRTAG('F','L','F','L');
const int lpms_ERR_OUTPUTS = FFERRTAG('O','U','T','P');
const int lpms_ERR_UNRECOVERABLE = FFERRTAG('U', 'N', 'R', 'V');

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

struct transcode_thread {
  int initialized;

  struct input_ctx ictx;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];

  int nb_outputs;
};

void lpms_init(enum LPMSLogLevel max_level)
{
  av_log_set_level(max_level);
}

//
// Transcoder
//

static int is_mpegts(AVFormatContext *ic) {
  return !strcmp("mpegts", ic->iformat->name);
}

static int flush_outputs(struct input_ctx *ictx, struct output_ctx *octx)
{
  // only issue w this flushing method is it's not necessarily sequential
  // wrt all the outputs; might want to iterate on each output per frame?
  int ret = 0;
  if (octx->vc) { // flush video
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->vc, octx->oc->streams[0], &octx->vf, NULL);
    }
  }
  ret = 0;
  if (octx->ac) { // flush audio
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->ac, octx->oc->streams[octx->dv ? 0 : 1], &octx->af, NULL);
    }
  }
  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  return av_write_trailer(octx->oc);
}

int transcode_shutdown(struct transcode_thread *h, int ret)
{
  //av_log(NULL, AV_LOG_WARNING, "shutting down transcoder\n");

  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;
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
  ictx->flushed = 0;
  ictx->flushing = 0;
  ictx->pkt_diff = 0;
  ictx->sentinel_count = 0;
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && (AV_HWDEVICE_TYPE_NONE == ictx->hw_type)) {
      avcodec_free_context(&ictx->vc);
      //av_log(NULL, AV_LOG_WARNING, "released input codec context\n");
  }
  
  for (int i = 0; i < nb_outputs; i++) {
    //send EOF signal to signature filter
    if(outputs[i].sfilters != NULL && outputs[i].sf.src_ctx != NULL) {
      av_buffersrc_close(outputs[i].sf.src_ctx, AV_NOPTS_VALUE, AV_BUFFERSRC_FLAG_PUSH);
      free_filter(&outputs[i].sf);
    }

    close_output(&outputs[i]);
  }
  return ret == AVERROR_EOF ? 0 : ret;

}

int transcode_init(struct transcode_thread *h, input_params *inp,
                   output_params *params, output_results *results)
{
  int ret = 0;
  struct input_ctx *ictx = &h->ictx;
  ictx->xcoderParams = inp->xcoderParams;
  int reopen_decoders = !ictx->transmuxing;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;

  if (!inp) LPMS_ERR(transcode_cleanup, "Missing input params")

  // by default we re-use decoder between segments of same stream
  // unless we are using SW deocder and had to re-open IO or demuxer
  if (!ictx->ic) {
    // reopen demuxer for the input segment if needed
    // XXX could open_input() be re-used here?
    ret = avformat_open_input(&ictx->ic, inp->fname, NULL, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen demuxer");
    ret = avformat_find_stream_info(ictx->ic, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to find info for reopened stream")
  } else if (!ictx->ic->pb) {
    // reopen input segment file IO context if needed
    ret = avio_open(&ictx->ic->pb, inp->fname, AVIO_FLAG_READ);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen file");
  } else reopen_decoders = 0;

  if (AV_HWDEVICE_TYPE_CUDA == ictx->hw_type && ictx->vi >= 0) {
    if (ictx->last_format == AV_PIX_FMT_NONE) ictx->last_format = ictx->ic->streams[ictx->vi]->codecpar->format;
    else if (ictx->ic->streams[ictx->vi]->codecpar->format != ictx->last_format) {
      LPMS_WARN("Input pixel format has been changed in the middle.");
      ictx->last_format = ictx->ic->streams[ictx->vi]->codecpar->format;
      // if the decoder is not re-opened when the video pixel format is changed,
      // the decoder tries HW decoding with the video context initialized to a pixel format different from the input one.
      // to handle a change in the input pixel format,
      // we close the demuxer and re-open the decoder by calling open_input().
      free_input(&h->ictx);
      ret = open_input(inp, &h->ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen video demuxer for HW decoding");
      reopen_decoders = 0;
    }
  }

  if (reopen_decoders) {
    // XXX check to see if we can also reuse decoder for sw decoding
    if (ictx->hw_type == AV_HWDEVICE_TYPE_NONE) {
      ret = open_video_decoder(inp, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen video decoder");
    }
    ret = open_audio_decoder(inp, ictx);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen audio decoder")
  }

  // populate output contexts
  for (int i = 0; i <  nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    octx->fname = params[i].fname;
    octx->width = params[i].w;
    octx->height = params[i].h;
    octx->muxer = &params[i].muxer;
    octx->audio = &params[i].audio;
    octx->video = &params[i].video;
    octx->vfilters = params[i].vfilters;
    octx->sfilters = params[i].sfilters;
    octx->xcoderParams = params[i].xcoderParams;
    if (params[i].bitrate) octx->bitrate = params[i].bitrate;
    if (params[i].fps.den) octx->fps = params[i].fps;
    if (params[i].gop_time) octx->gop_time = params[i].gop_time;
    if (params[i].from) octx->clip_from = params[i].from;
    if (params[i].to) octx->clip_to = params[i].to;
    octx->dv = ictx->vi < 0 || is_drop(octx->video->name);
    octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
    octx->res = &results[i];

    // first segment of a stream, need to initalize output HW context
    // XXX valgrind this line up
    // when transmuxing we're opening output with first segment, but closing it
    // only when lpms_transcode_stop called, so we don't want to re-open it
    // on subsequent segments
    if (!h->initialized || (AV_HWDEVICE_TYPE_NONE == octx->hw_type && !ictx->transmuxing)) {
      ret = open_output(octx, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open output");
      if (ictx->transmuxing) {
        octx->oc->flags |= AVFMT_FLAG_FLUSH_PACKETS;
        octx->oc->flush_packets = 1;
      }
      continue;
    }

    if (!ictx->transmuxing) {
      // non-first segment of a HW session
      ret = reopen_output(octx, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to re-open output for HW session");
    }
  }

  return 0;   // all ok

transcode_cleanup:
  return transcode_shutdown(h, ret);
}

void handle_discontinuity(struct input_ctx *ictx, AVPacket *pkt)
{
  int stream_index = pkt->stream_index;
  if (stream_index >= MAX_OUTPUT_SIZE) {
    return;
  }

  if (ictx->discontinuity[stream_index]) {
    // calc dts diff
    ictx->dts_diff[stream_index] = ictx->last_dts[stream_index] + ictx->last_duration[stream_index] - pkt->dts;
    ictx->discontinuity[stream_index] = 0;
  }

  pkt->pts += ictx->dts_diff[stream_index];
  pkt->dts += ictx->dts_diff[stream_index];
  // TODO: old code was doing that. I don't think it makes sense for video - one
  // just can't throw away arbitrary packets, it may damage the whole stream
  // I think reasonable solution would be to readjust the discontinuity or
  // something like this? Or report that input stream has wrong dts? Or start
  // manually reassigning timestamps? Leaving it here not to forget
  //if (ictx->last_dts[stream_index] > -1 && ipkt->dts <= ictx->last_dts[stream_index])  {
  //      // skip packet if dts is equal or less than previous one
  //      goto whileloop_end;
  //    }
  ictx->last_dts[stream_index] = pkt->dts;
  if (pkt->duration) {
    ictx->last_duration[stream_index] = pkt->duration;
  }
}

int handle_audio_frame(struct transcode_thread *h, AVStream *ist, output_results *decoded_results, AVFrame *dframe)
{
  struct input_ctx *ictx = &h->ictx;

  // frame duration update
  int64_t dur = 0;
  if (dframe->duration) {
    dur = dframe->duration;
  } else if (ist->r_frame_rate.den) {
    dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
  } else {
    // TODO use better heuristics for this; look at how ffmpeg does it
    LPMS_WARN("Could not determine next pts; filter might drop");
  }
  dframe->duration = dur;

  // keep as last frame
  av_frame_unref(ictx->last_frame_a);
  av_frame_ref(ictx->last_frame_a, dframe);

  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;

    if (octx->ac) {
      int ret = process_out(ictx, octx, octx->ac,
                            octx->oc->streams[octx->dv ? 0 : 1], &octx->af, dframe);
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue; // this is ok
      if (ret < 0) LPMS_ERR_RETURN("Error encoding audio");
    }
  }

  return 0;
}

int handle_video_frame(struct transcode_thread *h, AVStream *ist, output_results *decoded_results, AVFrame *dframe)
{
  struct input_ctx *ictx = &h->ictx;

  // TODO: this was removed, but need to investigate if safe
  //    if (is_flush_frame(dframe)) goto whileloop_end;
  // if we are here, we know there is a frame
  ++decoded_results->frames;
  decoded_results->pixels += dframe->width * dframe->height;

  // frame duration update
  int64_t dur = 0;
  if (dframe->duration) {
    dur = dframe->duration;
  } else if (ist->r_frame_rate.den) {
    dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
  } else {
    // TODO use better heuristics for this; look at how ffmpeg does it
    LPMS_WARN("Could not determine next pts; filter might drop");
  }
  dframe->duration = dur;

  // keep as last frame
  av_frame_unref(ictx->last_frame_v);
  av_frame_ref(ictx->last_frame_v, dframe);

  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;
        
    if (octx->vc) {
      int ret = process_out(ictx, octx, octx->vc, octx->oc->streams[0], &octx->vf, dframe);
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue; // this is ok
      if (ret < 0) {
        LPMS_ERR_RETURN("Error encoding video");
      }
    }
  }

  return 0;
}

int handle_audio_packet(struct transcode_thread *h, output_results *decoded_results,
                        AVPacket *pkt, AVFrame *frame)
{
  // Packet processing part
  struct input_ctx *ictx = &h->ictx;
  AVStream *ist = ictx->ic->streams[pkt->stream_index];
  int ret = 0;
  // TODO: separate counter for the audio packets. Old code had none

  // TODO: this could probably be done always, because it is a no-op if
  // lpms_discontinuity() wasn't called
  if (ictx->transmuxing) {
    handle_discontinuity(ictx, pkt);
  }

  // Check if there are outputs to which packet can be muxed "as is"
  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;
    AVStream *ost = NULL;
    // TODO: this is now global, but could easily be particular output option
    // we could do for example one transmuxing output (more make no sense)
    // and other could be transcoding ones
    if (ictx->transmuxing) {
      // When transmuxing every input stream has its direct counterpart
      ost = octx->oc->streams[pkt->stream_index];
    } else if (pkt->stream_index == ictx->ai) {
      // This is audio stream for this output, but do we need packet?
      if (octx->da) continue; // drop audio
      // If there is no encoder, then we are copying. Also the index of
      // audio stream is 0 when we are dropping video and 1 otherwise
      if (!octx->ac) ost = octx->oc->streams[octx->dv ? 0 : 1];
    }

    if (ost) {
      if (pkt->stream_index == ictx->ai) {
        // audio packet clipping
        if (!octx->clip_audio_start_pts_found) {
          octx->clip_audio_start_pts = pkt->pts;
          octx->clip_audio_start_pts_found = 1;
        }
        if (octx->clip_to && octx->clip_audio_start_pts_found && pkt->pts > octx->clip_audio_to_pts + octx->clip_audio_start_pts) {
          continue;
        }
        if (octx->clip_from && !octx->clip_started) {
          // we want first frame to be video frame
          continue;
        }
        if (octx->clip_from && pkt->pts < octx->clip_audio_from_pts + octx->clip_audio_start_pts) {
          continue;
        }
      }

      AVPacket *opkt = av_packet_clone(pkt);
      if (octx->clip_from && ist->index == ictx->ai) {
        opkt->pts -= octx->clip_audio_from_pts + octx->clip_audio_start_pts;
      }
      ret = mux(opkt, ist->time_base, octx, ost);
      av_packet_free(&opkt);
      if (ret < 0) LPMS_ERR_RETURN("Audio packet muxing error");
    }
  }

  // Packet processing finished, check if we should decode a frame
  if (ictx->ai != pkt->stream_index) return 0;
  if (!ictx->ac) return 0;

  // Try to decode
  ret = avcodec_send_packet(ictx->ac, pkt);
  if (ret < 0) {
    LPMS_ERR_RETURN("Error sending audio packet to decoder");
  }
  ret = avcodec_receive_frame(ictx->ac, frame);
  if (ret == AVERROR(EAGAIN)) {
    // This is not really an error. It may be that packet just fed into
    // the decoder may be not enough to complete decoding. Upper level will
    // get next packet and retry
    return 0;
  } else if (ret < 0) {
    LPMS_ERR_RETURN("Error receiving audio frame from decoder");
  } else {
    // Fine, we have frame, process it
    return handle_audio_frame(h, ist, decoded_results, frame);
  }
}

int handle_video_packet(struct transcode_thread *h, output_results *decoded_results,
                        AVPacket *pkt, AVFrame *frame)
{
  // Packet processing part
  struct input_ctx *ictx = &h->ictx;
  AVStream *ist = ictx->ic->streams[pkt->stream_index];
  int ret = 0;

  // TODO: separate counter for the video packets. Old code was increasing
  // video frames counter on video packets when transmuxing, which was
  // misleading at best. Video packet counter can be updated on both
  // transmuxing as well as normal packet processing

  if (!ictx->first_pkt && (pkt->flags & AV_PKT_FLAG_KEY)) {
    // very first video packet, keep it
    // TODO: this should be called first_video_pkt
    ictx->first_pkt = av_packet_clone(pkt);
    ictx->first_pkt->pts = -1;
  }

  // TODO: this could probably be done always, because it is a no-op if
  // lpms_discontinuity() wasn't called
  if (ictx->transmuxing) {
    handle_discontinuity(ictx, pkt);
  }

  // Check if there are outputs to which packet can be muxed "as is"
  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;
    AVStream *ost = NULL;
    // TODO: this is now global, but could easily be particular output option
    // we could do for example one transmuxing output (more make no sense)
    // and other could be transcoding ones
    if (ictx->transmuxing) {
      //av_log(NULL, AV_LOG_WARNING, "transmuxing");
      // When transmuxing every input stream has its direct counterpart
      ost = octx->oc->streams[pkt->stream_index];
    } else if (pkt->stream_index == ictx->vi) {
      // This is video stream for this output, but do we need packet?
      if (octx->dv) continue; // drop video
      // If there is no encoder, then we are copying
      if (!octx->vc) ost = octx->oc->streams[0];
    }

    if (ost) {
      // need to mux in the packet
      AVPacket *opkt = av_packet_clone(pkt);
      ret = mux(opkt, ist->time_base, octx, ost);
      av_packet_free(&opkt);
      if (ret < 0) LPMS_ERR_RETURN("Video packet muxing error");
    }
  }

  // Packet processing finished, check if we should decode a frame
  if (ictx->vi != pkt->stream_index) return 0;
  if (!ictx->vc) return 0;

  // Try to decode
  ret = avcodec_send_packet(ictx->vc, pkt);
  if (ret < 0) {
    LPMS_ERR_RETURN("Error sending video packet to decoder");
  }
  ictx->pkt_diff++;
  ret = avcodec_receive_frame(ictx->vc, frame);
  if (ret == AVERROR(EAGAIN)) {
    // This is not really an error. It may be that packet just fed into
    // the decoder may be not enough to complete decoding. Upper level will
    // get next packet and retry
    return 0;
  } else if (ret < 0) {
    LPMS_ERR_RETURN("Error receiving video frame from decoder");
  } else {
    // TODO: this whole sentinel frame business and packet count is broken,
    // because it assumes 1-to-1 relationship between packets and frames, and
    // it won't be so in multislice streams. Also what if first packet is just
    // parameter set and encoder doesn't have to decode when receiving one
    if (!is_flush_frame(frame)) {
      ictx->pkt_diff--; // decrease buffer count for non-sentinel video frames
      if (ictx->flushing) ictx->sentinel_count = 0;
    }
    // Fine, we have frame, process it
    return handle_video_frame(h, ist, decoded_results, frame);
  }
}

int handle_other_packet(struct transcode_thread *h, AVPacket *pkt)
{
  struct input_ctx *ictx = &h->ictx;
  AVStream *ist = ictx->ic->streams[pkt->stream_index];
  int ret = 0;

  // TODO: this could probably be done always, because it is a no-op if
  // lpms_discontinuity() wasn't called
  if (ictx->transmuxing) {
    handle_discontinuity(ictx, pkt);
  }

  // Check if there are outputs to which packet can be muxed "as is"
  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;
    AVStream *ost = NULL;
    // TODO: this is now global, but could easily be particular output option
    // we could do for example one transmuxing output (more make no sense)
    // and other could be transcoding ones
    if (ictx->transmuxing) {
      // When transmuxing every input stream has its direct counterpart
      ost = octx->oc->streams[pkt->stream_index];
      // need to mux in the packet
      AVPacket *opkt = av_packet_clone(pkt);
      ret = mux(opkt, ist->time_base, octx, ost);
      av_packet_free(&opkt);
      if (ret < 0) LPMS_ERR_RETURN("Other packet muxing error");
    }
  }

  return 0;
}

// TODO: right now this flushes filter and the encoders, this will be separated
// in the future
int flush_all_outputs(struct transcode_thread *h)
{
  struct input_ctx *ictx = &h->ictx;
  int ret = 0;
  for (int i = 0; i < h->nb_outputs; i++) {
    // Again, global switch but could be output setting in the future
    if (ictx->transmuxing) {
      // just flush muxer, but do not write trailer and close
      av_interleaved_write_frame(h->outputs[i].oc, NULL);
    } else {
        // this will flush video and audio streams, flush muxer, write trailer
        // and close
        ret = flush_outputs(ictx, h->outputs + i);
        if (ret < 0) LPMS_ERR_RETURN("Unable to fully flush outputs")
    }
  }

  return 0;
}

int transcode2(struct transcode_thread *h,
  input_params *inp, output_params *params,
  output_results *decoded_results)
{
  struct input_ctx *ictx = &h->ictx;
  AVStream *ist = NULL;
  AVPacket *ipkt = NULL;
  AVFrame *iframe = NULL;
  int ret = 0;

  // TODO: allocation checks
  ipkt = av_packet_alloc();
  iframe = av_frame_alloc();

  // Main demuxing loop: process input packets till EOF in the input stream
  while (1) {
    ret = demux_in(ictx, ipkt);
    // See what we got
    if (ret == AVERROR_EOF) {
      // no more input packets
      break;
    } else if (ret < 0) {
      // demuxing error
      LPMS_ERR_BREAK("Unable to read input");
    }
    // all is fine, handle packet just received
    ist = ictx->ic->streams[ipkt->stream_index];
    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      // video packet
      ret = handle_video_packet(h, decoded_results, ipkt, iframe);
      if (ret < 0) break;
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      // audio packet
      ret = handle_audio_packet(h, decoded_results, ipkt, iframe);
      if (ret < 0) break;
    } else {
      // other types of packets (used only for transmuxing)
      handle_other_packet(h, ipkt);
      if (ret < 0) break;
    }
    av_packet_unref(ipkt);
  }

  // No more input packets. Demuxer finished work. But there may still
  // be frames buffered in the decoder(s), and we need to drain/flush

  // TODO: this will also get splitted into video and audio flushing
  // loops, but right now flush_in works for entire output, flushing
  // both audio and video
  while (1) {
    int stream_index;
    ret = flush_in(ictx, iframe, &stream_index);
    if (AVERROR_EOF == ret) {
      // No more frames, can break
      break;
    }
    if (AVERROR(EAGAIN) == ret) {
      // retry
      continue;
    }
    if (ret < 0) LPMS_ERR_BREAK("Flushing failed");
    ist = ictx->ic->streams[stream_index];
    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      handle_video_frame(h, ist, decoded_results, iframe);
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      handle_audio_frame(h, ist, decoded_results, iframe);
    }
  }

  // No more input frames. Decoder(s) finished work. But there may still
  // be frames buffered in the filters, and we need to flush them
  // IMPORTANT: no handle_*_frame calls here because there is no more input
  // frames.
  // NOTE: this is "flush filters, flush decoders, flush muxers" all in one,
  // it will get broken down in future
  flush_all_outputs(h);

  // Processing finished

transcode_cleanup:
  if (ipkt) av_packet_free(&ipkt);
  if (iframe) av_frame_free(&iframe);

  if (ictx->transmuxing) {
    // transcode_shutdown() is not to be called when transmuxing
    if (ictx->ic) {
        avformat_close_input(&ictx->ic);
        ictx->ic = NULL;
    }
    return 0;
  } else return transcode_shutdown(h, ret);
}

int transcode(struct transcode_thread *h,
  input_params *inp, output_params *params,
  output_results *decoded_results)
{
  int ret = 0;
  AVPacket *ipkt = NULL;
  AVFrame *dframe = NULL;
  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;

  ipkt = av_packet_alloc();
  if (!ipkt) LPMS_ERR(transcode_cleanup, "Unable to allocated packet");
  dframe = av_frame_alloc();
  if (!dframe) LPMS_ERR(transcode_cleanup, "Unable to allocate frame");

  while (1) {
    // DEMUXING & DECODING
    int has_frame = 0;
    AVStream *ist = NULL;
    AVFrame *last_frame = NULL;
    int stream_index = -1;

    av_frame_unref(dframe);
    ret = process_in(ictx, dframe, ipkt, &stream_index);
    if (ret == AVERROR_EOF) {
      // no more processing, go for flushes
      break;
    }
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (ret == AVERROR(EAGAIN)) ;  // this is a-ok
    else if (lpms_ERR_INPUT_NOKF == ret) {
      LPMS_ERR(transcode_cleanup, "Could not decode; No keyframes in input");
    } else if (ret < 0) LPMS_ERR(transcode_cleanup, "Could not decode; stopping");

    // So here we have several possibilities:
    // ipkt: usually it will be here, but if we are decoding, and if we reached
    // end of stream, it may be so that draining of the decoder produces frames
    // without packets
    // dframe: if there is no decoding (because of transmuxing, or because of
    // copying), it won't be set

    ist = ictx->ic->streams[stream_index];

    // This is for the case when we _are_ decoding but frame is not complete yet
    // So for example multislice h.264 picture without all slices fed in.
    // IMPORTANT: this should also be false if we are transmuxing, and it is not
    // so, at least not automatically, because then process_in returns 0 and not
    // lpms_ERR_PACKET_ONLY
    has_frame = lpms_ERR_PACKET_ONLY != ret;

    // Now apart from if (is_flush_frame(dframe)) goto whileloop_end; statement
    // this code just updates has_frame properly for video and audio, updates
    // statistics for video and ausio and sets last_frame
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
    } else {
      has_frame = 0;  // bugfix
    }
    // if there is frame, update duration and put this frame in place as last_frame
    if (has_frame) {
      int64_t dur = 0;
      if (dframe->duration) dur = dframe->duration;
      else if (ist->r_frame_rate.den) {
        dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
      } else {
        // TODO use better heuristics for this; look at how ffmpeg does it
        LPMS_WARN("Could not determine next pts; filter might drop");
      }
      dframe->duration = dur;
      av_frame_unref(last_frame);
      av_frame_ref(last_frame, dframe);
    }
    // similar but for transmuxing case - update number of frames
    if (ictx->transmuxing) {
      if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
        decoded_results->frames++;  // Michal: this is packets, not frames, may be wrong with multislice!
      }
      if (stream_index < MAX_OUTPUT_SIZE) {
        if (ictx->discontinuity[stream_index]) {
          // calc dts diff
          ictx->dts_diff[stream_index] = ictx->last_dts[stream_index] + ictx->last_duration[stream_index] - ipkt->dts;
          ictx->discontinuity[stream_index] = 0;
        }
        ipkt->pts += ictx->dts_diff[stream_index];
        ipkt->dts += ictx->dts_diff[stream_index];
        // Michal: this is dangerous, may damage the stream due to dropping critical
        // packet - one could do that to frames, but not packets. Some formats
        // allow dropping packets (like nonref picture slices in h.264 or frames
        // other than keyframes in vp8), but it needs to be done with care and
        // understanding of the packet role, not arbitrarily
        if (ictx->last_dts[stream_index] > -1 && ipkt->dts <= ictx->last_dts[stream_index])  {
          // skip packet if dts is equal or less than previous one
          goto whileloop_end;
        }
        ictx->last_dts[stream_index] = ipkt->dts;
        if (ipkt->duration) {
          ictx->last_duration[stream_index] = ipkt->duration;
        }
      }
    }

    // ENCODING & MUXING OF ALL OUTPUT RENDITIONS
    for (int i = 0; i < nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      struct filter_ctx *filter = NULL;
      AVStream *ost = NULL;
      AVCodecContext *encoder = NULL;
      ret = 0; // reset to avoid any carry-through

      if (ictx->transmuxing)
        ost = octx->oc->streams[stream_index];  // because all streams are copied 1:1
      else if (ist->index == ictx->vi) {
        if (octx->dv) continue; // drop video stream for this output
        ost = octx->oc->streams[0]; // because video stream is always stream 0
        if (ictx->vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        }
      } else if (ist->index == ictx->ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[octx->dv ? 0 : 1]; // audio index depends on whether video exists
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
        if (ipkt->pts == AV_NOPTS_VALUE) continue;

        if (ist->index == ictx->ai) {
          if (!octx->clip_audio_start_pts_found) {
            octx->clip_audio_start_pts = ipkt->pts;
            octx->clip_audio_start_pts_found = 1;
          }
          if (octx->clip_to && octx->clip_audio_start_pts_found && ipkt->pts > octx->clip_audio_to_pts + octx->clip_audio_start_pts) {
            continue;
          }
          if (octx->clip_from && !octx->clip_started) {
            // we want first frame to be video frame
            continue;
          }
          if (octx->clip_from && ipkt->pts < octx->clip_audio_from_pts + octx->clip_audio_start_pts) {
            continue;
          }
        }

        pkt = av_packet_clone(ipkt);
        if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for copy");
        if (octx->clip_from && ist->index == ictx->ai) {
          pkt->pts -= octx->clip_audio_from_pts + octx->clip_audio_start_pts;
        }
        ret = mux(pkt, ist->time_base, octx, ost);
        av_packet_free(&pkt);
      } else if (has_frame) {
        ret = process_out(ictx, octx, encoder, ost, filter, dframe);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) LPMS_ERR(transcode_cleanup, "Error encoding");
    }
whileloop_end:
    av_packet_unref(ipkt);
  }

  if (ictx->transmuxing) {
    for (int i = 0; i < nb_outputs; i++) {
      av_interleaved_write_frame(outputs[i].oc, NULL); // flush muxer
    }
    if (ictx->ic) {
        avformat_close_input(&ictx->ic);
        ictx->ic = NULL;
    }
    return 0;
  }

  // flush outputs
  for (int i = 0; i < nb_outputs; i++) {
      ret = flush_outputs(ictx, &outputs[i]);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to fully flush outputs")
  }

transcode_cleanup:
  if (dframe) av_frame_free(&dframe);
  if (ipkt) av_packet_free(&ipkt);  // needed for early exits
  return transcode_shutdown(h, ret);
}

// MA: this should probably be merged with transcode_init, as it basically is a
// part of initialization
int lpms_transcode(input_params *inp, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results, int use_new)
{
  int ret = 0;
  struct transcode_thread *h = inp->handle;

  if (!h->initialized) {
    //av_log(NULL, AV_LOG_WARNING, "starting new transcode thread\n");
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

  // MA: Note that here difference of configurations here is based upon number
  // of outputs alone. But what if the number of outputs is the same, but they
  // are of different types? What if the number and types of outputs are the
  // same but there is a different permutation?
  if (h->nb_outputs != nb_outputs) {
#define MAX(x, y) (((x) > (y)) ? (x) : (y))
#define MIN(x, y) (((x) < (y)) ? (x) : (y))
    // MA: we have a problem here. Consider first configuration with 1 output,
    // and second one with 2 outputs. When transcode_thread was created
    // (in lpms_transcode_new) all the outputs were cleared with zeros. Then,
    // only outputs described in first configuration were actually initialized.
    // Thus, for loop below will execute for i = 1 and so it will access
    // the output not initialized before. is_dnn_profile values will be both
    // zeros (so false), and so only_detector_diff will be set to false as well
    // So we will get lpms_ERR_OUTPUTS. But suppose that new output in second
    // configuration is the detector output. Shouldn't that be allowed?
    // To sum things up, this approach works if "new" configuration has less
    // outputs than old one, and the "removed" outputs were dnn outputs. This
    // approach doesn't work if the "new" configuration has more outputs than
    // old one, even if "added" outputs are actually dnn outputs.
    // make sure only detection related outputs are changed
      return lpms_ERR_OUTPUTS;
#undef MAX
#undef MIN
  }

  ret = transcode_init(h, inp, params, results);
  if (ret < 0) return ret;

  if (use_new) {
    ret = transcode2(h, inp, params, decoded_results);
  } else {
    ret = transcode(h, inp, params, decoded_results);
  }
  h->initialized = 1;
  return ret;
}

int lpms_transcode_reopen_demux(input_params *inp) {
  free_input(&inp->handle->ictx);
  return open_input(inp, &inp->handle->ictx);
}

struct transcode_thread* lpms_transcode_new() {
  struct transcode_thread *h = malloc(sizeof (struct transcode_thread));
  if (!h) return NULL;
  memset(h, 0, sizeof *h);
  // initialize video stream pixel format.
  h->ictx.last_format = AV_PIX_FMT_NONE;
  // keep track of last dts in each stream.
  // used while transmuxing, to skip packets with invalid dts.
  for (int i = 0; i < MAX_OUTPUT_SIZE; i++) {
    h->ictx.last_dts[i] = -1;
  }
  return h;
}

void lpms_transcode_stop(struct transcode_thread *handle) {
  // not threadsafe as-is; calling function must ensure exclusivity!
  int i;

  if (!handle) return;

  free_input(&handle->ictx);
  for (i = 0; i < MAX_OUTPUT_SIZE; i++) {
    if (handle->ictx.transmuxing && handle->outputs[i].oc) {
        av_write_trailer(handle->outputs[i].oc);
    }
    free_output(&handle->outputs[i]);
  }

  free(handle);
}

void lpms_transcode_discontinuity(struct transcode_thread *handle) {
  if (!handle)
    return;
  for (int i = 0; i < MAX_OUTPUT_SIZE; i++) {
    handle->ictx.discontinuity[i] = 1;
  }
}
