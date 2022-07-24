#include "transcoder.h"
#include "decoder.h"
#include "filter.h"
#include "encoder.h"
#include "logging.h"
#include "stream_buffer.h"
#include "output_queue.h"

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
const int lpms_ERR_INPUTS = FFERRTAG('I', 'N', 'P', 'Z');
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
//  Demuxer: Used to be reused, but it was found very problematic, as reused
//           muxer retained information from previous segments. It caused all
//           kind of subtle problems and was removed
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
  struct input_ctx ictx;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];
  int nb_outputs;

  AVFilterGraph *dnn_filtergraph;

  // Input buffer - when I/O is done outside of transcoder, for example in
  // Low Latency scenarios
  StreamBuffer input_buffer;
  int use_buffer_for_input; // TODO: name it "use custom output" or some such
  // Output
  OutputQueue output_queue;
};

// TODO: this feels like it belongs elsewhere, not in the top-level transcoder
// code
static AVFilterGraph * create_dnn_filtergraph(lvpdnn_opts *dnn_opts)
{
  const AVFilter *filter = NULL;
  AVFilterContext *filter_ctx = NULL;
  AVFilterGraph *graph_ctx = NULL;
  int ret = 0;
  char errstr[1024];
  char *filter_name = "livepeer_dnn";
  char filter_args[512];
  snprintf(filter_args, sizeof filter_args, "model=%s:input=%s:output=%s:backend_configs=%s",
           dnn_opts->modelpath, dnn_opts->inputname, dnn_opts->outputname, dnn_opts->backend_configs);

  /* allocate graph */
  graph_ctx = avfilter_graph_alloc();
  if (!graph_ctx)
    LPMS_ERR(create_dnn_error, "Unable to open DNN filtergraph");

  /* get a corresponding filter and open it */
  if (!(filter = avfilter_get_by_name(filter_name))) {
    snprintf(errstr, sizeof errstr, "Unrecognized filter with name '%s'\n", filter_name);
    LPMS_ERR(create_dnn_error, errstr);
  }

  /* open filter and add it to the graph */
  if (!(filter_ctx = avfilter_graph_alloc_filter(graph_ctx, filter, filter_name))) {
    snprintf(errstr, sizeof errstr, "Impossible to open filter with name '%s'\n", filter_name);
    LPMS_ERR(create_dnn_error, errstr);
  }
  if (avfilter_init_str(filter_ctx, filter_args) < 0) {
    snprintf(errstr, sizeof errstr, "Impossible to init filter '%s' with arguments '%s'\n", filter_name, filter_args);
    LPMS_ERR(create_dnn_error, errstr);
  }

  return graph_ctx;

create_dnn_error:
  avfilter_graph_free(&graph_ctx);
  return NULL;
}

// TODO: I don't like this flush_output/flush_all_outputs stuff. To begin with
// we could use better terminology, because flushing the output means flushing
// the filters first, then the decoders. Also I don't like how
// av_interleaved_write_frame is sometimes called in one and sometimes in the
// other function. To be redone
static int flush_output(struct input_ctx *ictx, struct output_ctx *octx)
{
  // only issue w this flushing method is it's not necessarily sequential
  // wrt all the outputs; might want to iterate on each output per frame?
  int ret = 0;
  if (octx->vc) { // flush video
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->vc, octx->video_stream, &octx->vf, NULL);
    }
  }
  ret = 0;
  if (octx->ac) { // flush audio
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->ac, octx->audio_stream, &octx->af, NULL);
    }
  }
  // send EOF signal to signature filter
  if (octx->sfilters != NULL && octx->sf.src_ctx != NULL) {
    av_buffersrc_close(octx->sf.src_ctx, AV_NOPTS_VALUE, AV_BUFFERSRC_FLAG_PUSH);
  }

  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  return 0;
}

// See comment above
static int flush_all_outputs(struct transcode_thread *h)
{
  struct input_ctx *ictx = &h->ictx;
  int ret = 0;
  for (int i = 0; i < h->nb_outputs; i++) {
    // Again, global switch but could be output setting in the future
    if (ictx->transmuxing) {
      // just flush muxer, but do not write trailer and close
      av_interleaved_write_frame(h->outputs[i].oc, NULL);
    } else {
      if(h->outputs[i].is_dnn_profile == 0) {
        // this will flush video and audio streams, flush muxer
        // and close
        ret = flush_output(ictx, h->outputs + i);
        if (ret < 0) LPMS_ERR_RETURN("Unable to fully flush outputs")
      } else if(h->outputs[i].is_dnn_profile && h->outputs[i].res->frames > 0) {
        for (int j = 0; j < MAX_CLASSIFY_SIZE; j++) {
          h->outputs[i].res->probs[j] = h->outputs[i].res->probs[j] / h->outputs[i].res->frames;
        }
      }
    }
  }

  return 0;
}

// TODO: change name to something like "fix" perhaps? Handle is kinda reserved
// here for frames/packets
static void handle_discontinuity(struct input_ctx *ictx, AVPacket *pkt)
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

static int handle_audio_frame(struct transcode_thread *h, AVStream *ist,
                              output_results *decoded_results, AVFrame *dframe)
{
  struct input_ctx *ictx = &h->ictx;
  ++decoded_results->audio_frames;
  // frame duration update
  int64_t dur = 0;
  if (dframe->pkt_duration) {
    dur = dframe->pkt_duration;
  } else if (ist->r_frame_rate.den) {
    dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
  } else {
    // TODO use better heuristics for this; look at how ffmpeg does it
    LPMS_WARN("Could not determine next pts; filter might drop");
  }
  dframe->pkt_duration = dur;

  // keep as last frame
  av_frame_unref(ictx->last_frame_a);
  av_frame_ref(ictx->last_frame_a, dframe);

  for (int i = 0; i < h->nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;

    if (octx->ac) {
      int ret = process_out(ictx, octx, octx->ac,
                            octx->audio_stream, &octx->af, dframe);
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue; // this is ok
      if (ret < 0) LPMS_ERR_RETURN("Error encoding audio");
    }
  }

  return 0;
}

static int handle_video_frame(struct transcode_thread *h, AVStream *ist,
                              output_results *decoded_results, AVFrame *dframe)
{
  struct input_ctx *ictx = &h->ictx;

  // MA: "sentinel frames" stuff, will be removed
  if (is_flush_frame(dframe)) return 0;
  // if we are here, we know there is a frame
  ++decoded_results->frames;
  decoded_results->pixels += dframe->width * dframe->height;
  ++decoded_results->video_frames;

  // frame duration update
  int64_t dur = 0;
  if (dframe->pkt_duration) {
    dur = dframe->pkt_duration;
  } else if (ist->r_frame_rate.den) {
    dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
  } else {
    // TODO use better heuristics for this; look at how ffmpeg does it
    LPMS_WARN("Could not determine next pts; filter might drop");
  }
  dframe->pkt_duration = dur;

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

static int handle_audio_packet(struct transcode_thread *h, output_results *decoded_results,
                               AVPacket *pkt, AVFrame *frame)
{
  ++decoded_results->audio_packets;
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
      if (!octx->ac) ost = octx->audio_stream;
    }

    if (ost) {
      if (pkt->stream_index == ictx->ai) {
        // audio packet clipping
        if (!octx->clip_audio_start_pts_found) {
          octx->clip_audio_start_pts = pkt->pts;
          octx->clip_audio_start_pts_found = 1;
        }
        // similar to clipping in encoder.c
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
      ++octx->res->audio_packets;
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

static int handle_video_packet(struct transcode_thread *h, output_results *decoded_results,
                               AVPacket *pkt, AVFrame *frame)
{
  ++decoded_results->video_packets;
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
      // When transmuxing every input stream has its direct counterpart
      ost = octx->oc->streams[pkt->stream_index];
    } else if (pkt->stream_index == ictx->vi) {
      // This is video stream for this output, but do we need packet?
      if (octx->dv) continue; // drop video
      // If there is no encoder, then we are copying
      if (!octx->vc) ost = octx->video_stream;
    }

    if (ost) {
      // need to mux in the packet
      AVPacket *opkt = av_packet_clone(pkt);
      ret = mux(opkt, ist->time_base, octx, ost);
      av_packet_free(&opkt);
      if (ret < 0) LPMS_ERR_RETURN("Video packet muxing error");
      ++octx->res->video_packets;
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
    // parameter set? the decoder doesn't have to decode when receiving one
    if (!is_flush_frame(frame)) {
      ictx->pkt_diff--; // decrease buffer count for non-sentinel video frames
      if (ictx->flushing) ictx->sentinel_count = 0;
    }
    // Fine, we have frame, process it
    return handle_video_frame(h, ist, decoded_results, frame);
  }
}

static int handle_other_packet(struct transcode_thread *h,
                               output_results *decoded_results, AVPacket *pkt)
{
  ++decoded_results->other_packets;
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
      ++octx->res->other_packets;
    }
  }

  return 0;
}

static int transcode(struct transcode_thread *h, input_params *inp,
                     output_params *params, output_results *decoded_results)
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
    ret = av_read_frame(ictx->ic, ipkt);
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
      handle_other_packet(h, decoded_results, ipkt);
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
  // NOTE: this is "flush filters, flush encoders, flush muxers" all in one,
  // it will get broken down in future
  flush_all_outputs(h);

  // Processing finished
  if (ipkt) av_packet_free(&ipkt);
  if (iframe) av_frame_free(&iframe);

  return ret;
}

// lpms_* functions form externally visible Transcoder interface
void lpms_init(enum LPMSLogLevel max_level)
{
  av_log_set_level(max_level);
}

int lpms_transcode(input_params *inp, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results)
{
  int ret = 0;
  struct transcode_thread *h = inp->handle;
  int decode_a = 0, decode_v = 0;

  // Part I: Configuration checks. These are far too lax really
  if (nb_outputs > MAX_OUTPUT_SIZE) {
    return lpms_ERR_OUTPUTS;
  }

  if (!inp) return lpms_ERR_INPUTS;

  // MA: Note that here difference of configurations here is based upon number
  // of outputs alone. But what if the number of outputs is the same, but they
  // are of different types? What if the number and types of outputs are the
  // same but there is a different permutation?
  if (h->nb_outputs && (h->nb_outputs != nb_outputs)) {
#define MAX(x, y) (((x) > (y)) ? (x) : (y))
#define MIN(x, y) (((x) < (y)) ? (x) : (y))
    bool only_detector_diff = true;
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
    for (int i = MIN(nb_outputs, h->nb_outputs); i < MAX(nb_outputs, h->nb_outputs); i++) {
      if (!h->outputs[i].is_dnn_profile)
        only_detector_diff = false;
    }
    if (!only_detector_diff) {
      return lpms_ERR_OUTPUTS;
    }
#undef MAX
#undef MIN
  }

  // Part II: If we got here, it appears we can use new configuration
  h->nb_outputs = nb_outputs;
  // Check to see if we can skip decoding
  for (int i = 0; i < nb_outputs; i++) {
    if (!needs_decoder(params[i].video.name)) h->ictx.dv = ++decode_v == nb_outputs;
    if (!needs_decoder(params[i].audio.name)) h->ictx.da = ++decode_a == nb_outputs;
  }

  ret = open_input(inp, &h->ictx, h->use_buffer_for_input ? &h->input_buffer : NULL);
  if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open input");

  // populate output contexts
  // TODO: I don't like this manual copying. What about making the "context"
  // or "settings" or whatever structs and having them in both params as well
  // as in actual contexts? Then simple memcpy would do...or perhaps make
  // utility functions for this? Or make it open_output's job, more in line
  // with how open_input works (except that dv/da are initialized by hand)
  // In short: streamline this somehow
  for (int i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = h->outputs + i;
    octx->fname = params[i].fname;
    octx->width = params[i].w;
    octx->height = params[i].h;
    octx->muxer = &params[i].muxer;
    octx->audio = &params[i].audio;
    octx->video = &params[i].video;
    octx->vfilters = params[i].vfilters;
    octx->sfilters = params[i].sfilters;
    octx->xcoderParams = params[i].xcoderParams;
    if (params[i].is_dnn && h->dnn_filtergraph != NULL) {
      octx->is_dnn_profile = params[i].is_dnn;
      octx->dnn_filtergraph = &h->dnn_filtergraph;
    }
    if (params[i].bitrate) octx->bitrate = params[i].bitrate;
    if (params[i].fps.den) octx->fps = params[i].fps;
    if (params[i].gop_time) octx->gop_time = params[i].gop_time;
    if (params[i].from) octx->clip_from = params[i].from;
    if (params[i].to) octx->clip_to = params[i].to;
    octx->dv = h->ictx.vi < 0 || is_drop(octx->video->name);
    octx->da = h->ictx.ai < 0 || is_drop(octx->audio->name);
    octx->res = &results[i];
    octx->write_context.index = i;

    ret = open_output(octx, &h->ictx, h->use_buffer_for_input ? &h->output_queue : NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open output");
  }

  // Part III: transcoding operation itself since everything was set for up
  // properly (or at least it appears so)
  ret = transcode(h, inp, params, decoded_results);
  // we treat AVERROR_EOF here as success
  if (AVERROR_EOF == ret) {
    ret = 0;
  }

  // Part IV: shutdown
transcode_cleanup:
  if (h->use_buffer_for_input) {
    // terminate output queue
    queue_push_end(&h->output_queue);
  }

  // IMPORTANT: note that this is the only place when PRESERVE_HW_ENCODER and
  // PRESERVE_HW_DECODER are used. This is done to retain HW encoder and decoder
  // when segment transcoding was successful, because some HW encoders/decoders
  // are very costly to initialize from ffmpeg level.
  // In the future I want code like that:
  // - at the very least moved deep into respective video pipeline implementation
  // - ideally completely removed, because in most (if not all) cases it is the
  // memory buffers and not decoders/encoders that are so costly to initialize
  // and/or obtain. Keeping the pool of the buffers should allow for cheap
  // decoder/encoder initialization, and it is how it should be given that we
  // don't want to assume that the configuration stays constant between
  // segments being encoded (and we kinda do it now)
  free_input(&h->ictx, PRESERVE_HW_DECODER);
  for (int i = 0; i < nb_outputs; i++) {
    // TODO: one day this will be per-output setting and not a global one
    if (!h->ictx.transmuxing) {
      free_output(h->outputs + i, PRESERVE_HW_ENCODER);
    }
  }
  return ret;
}

// TODO: name - this is called _stop, but it is more like stop & destroy
void lpms_transcode_stop(struct transcode_thread *handle)
{
  // not threadsafe as-is; calling function must ensure exclusivity!

  int i;

  if (!handle) return;

  free_input(&handle->ictx, FORCE_CLOSE_HW_DECODER);
  for (i = 0; i < handle->nb_outputs; i++) {
    free_output(&handle->outputs[i], FORCE_CLOSE_HW_ENCODER);
  }

  if (handle->dnn_filtergraph) avfilter_graph_free(&handle->dnn_filtergraph);
  buffer_destroy(&handle->input_buffer);
  free(handle);
}

struct transcode_thread* lpms_transcode_new(lvpdnn_opts *dnn_opts)
{
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

  if (-1 == buffer_create(&h->input_buffer)) {
    free(h);
    return NULL;
  }
  queue_create(&h->output_queue);

  // handle dnn filter graph creation
  if (dnn_opts) {
    AVFilterGraph *filtergraph = create_dnn_filtergraph(dnn_opts);
    if (!filtergraph) {
      buffer_destroy(&h->input_buffer);
      queue_destroy(&h->output_queue);
      free(h);
      h = NULL;
    } else {
      h->dnn_filtergraph = filtergraph;
    }
  }
  return h;
}

void lpms_transcode_discontinuity(struct transcode_thread *handle) {
  if (!handle)
    return;
  for (int i = 0; i < MAX_OUTPUT_SIZE; i++) {
    handle->ictx.discontinuity[i] = 1;
  }
}

void lpms_transcode_push_reset(struct transcode_thread *handle, int on)
{
  if (!handle) return;
  buffer_reset(&handle->input_buffer);
  queue_reset(&handle->output_queue);
  handle->use_buffer_for_input = on;
}

void lpms_transcode_push_bytes(struct transcode_thread *handle, uint8_t *bytes, int size)
{
  if (!handle) return;
  buffer_put_bytes(&handle->input_buffer, bytes, size);
}

void lpms_transcode_push_eof(struct transcode_thread *handle)
{
  if (!handle) return;
  buffer_end_of_stream(&handle->input_buffer);
}

void lpms_transcode_push_error(struct transcode_thread *handle, int code)
{
  if (!handle) return;
  buffer_error(&handle->input_buffer, code);
}

const OutputPacket *lpms_transcode_peek_packet(struct transcode_thread *handle)
{
  if (!handle) return NULL;
  return queue_peek_front(&handle->output_queue);
}

void lpms_transcode_pop_packet(struct transcode_thread *handle)
{
  if (!handle) return;
  return queue_pop_front(&handle->output_queue);
}
