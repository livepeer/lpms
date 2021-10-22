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

#define MAX_OUTPUT_SIZE 10

struct transcode_thread {
  int initialized;

  struct input_ctx ictx;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];

  AVFilterGraph *dnn_filtergraph;

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
  int ret = 0, i = 0;
  struct input_ctx *ictx = &h->ictx;
  int reopen_decoders = !ictx->transmuxing;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;
  AVPacket *ipkt = NULL;
  AVFrame *dframe = NULL;

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
  if (reopen_decoders) {
    // XXX check to see if we can also reuse decoder for sw decoding
    if (AV_HWDEVICE_TYPE_CUDA != ictx->hw_type) {
      ret = open_video_decoder(inp, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen video decoder");
    }
    ret = open_audio_decoder(inp, ictx);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen audio decoder")
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
      octx->sfilters = params[i].sfilters;
      if (params[i].is_dnn && h->dnn_filtergraph != NULL) {
          octx->is_dnn_profile = params[i].is_dnn;
          octx->dnn_filtergraph = &h->dnn_filtergraph;
      }
      if (params[i].bitrate) octx->bitrate = params[i].bitrate;
      if (params[i].fps.den) octx->fps = params[i].fps;
      if (params[i].gop_time) octx->gop_time = params[i].gop_time;
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

  ipkt = av_packet_alloc();
  if (!ipkt) LPMS_ERR(transcode_cleanup, "Unable to allocated packet");
  dframe = av_frame_alloc();
  if (!dframe) LPMS_ERR(transcode_cleanup, "Unable to allocate frame");

  while (1) {
    // DEMUXING & DECODING
    int has_frame = 0;
    AVStream *ist = NULL;
    AVFrame *last_frame = NULL;
    av_frame_unref(dframe);
    ret = process_in(ictx, dframe, ipkt);
    if (ret == AVERROR_EOF) break;
                            // Bail out on streams that appear to be broken
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (lpms_ERR_INPUT_NOKF == ret) {
      LPMS_ERR(transcode_cleanup, "Could not decode; No keyframes in input");
    } else if (ret < 0) LPMS_ERR(transcode_cleanup, "Could not decode; stopping");
    ist = ictx->ic->streams[ipkt->stream_index];
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
        LPMS_WARN("Could not determine next pts; filter might drop");
      }
      dframe->pkt_duration = dur;
      av_frame_unref(last_frame);
      av_frame_ref(last_frame, dframe);
    }
    if (ictx->transmuxing) {
      // decoded_results->frames++;
      ist = ictx->ic->streams[ipkt->stream_index];
      if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
        decoded_results->frames++;
      }
      if (ictx->discontinuity) {
        // calc dts diff
        ictx->dts_diff = ictx->last_dts + ictx->last_duration - ipkt->dts;
        ictx->discontinuity = 0;
      }

      ipkt->pts += ictx->dts_diff;
      ipkt->dts += ictx->dts_diff;

      if (ipkt->stream_index == 0) {
        ictx->last_dts = ipkt->dts;
        if (ipkt->duration) {
          ictx->last_duration = ipkt->duration;
        }
      }
    }

    // ENCODING & MUXING OF ALL OUTPUT RENDITIONS
    for (i = 0; i < nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      struct filter_ctx *filter = NULL;
      AVStream *ost = NULL;
      AVCodecContext *encoder = NULL;
      ret = 0; // reset to avoid any carry-through

      if (ictx->transmuxing)
        ost = octx->oc->streams[ipkt->stream_index];
      else if (ist->index == ictx->vi) {
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
        if (ipkt->pts == AV_NOPTS_VALUE) continue;

        pkt = av_packet_clone(ipkt);
        if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for copy");
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
    for (i = 0; i < nb_outputs; i++) {
      av_interleaved_write_frame(outputs[i].oc, NULL); // flush muxer
    }
    if (ictx->ic) {
        avformat_close_input(&ictx->ic);
        ictx->ic = NULL;
    }
    return 0;
  }

  // flush outputs
  for (i = 0; i < nb_outputs; i++) {
    if(outputs[i].is_dnn_profile == 0) {
      ret = flush_outputs(ictx, &outputs[i]);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to fully flush outputs")
    }
    else if(outputs[i].is_dnn_profile && outputs[i].res->frames > 0) {
       for (int j = 0; j < MAX_CLASSIFY_SIZE; j++) {
         outputs[i].res->probs[j] =  outputs[i].res->probs[j] / outputs[i].res->frames;         
       }
    }
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
  if (ipkt) av_packet_free(&ipkt);  // needed for early exits
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) {
    //send EOF signal to signature filter
    if(outputs[i].sfilters != NULL && outputs[i].sf.src_ctx != NULL) {
      av_buffersrc_close(outputs[i].sf.src_ctx, AV_NOPTS_VALUE, AV_BUFFERSRC_FLAG_PUSH);
      free_filter(&outputs[i].sf);
    }
    close_output(&outputs[i]);
  }
  return ret == AVERROR_EOF ? 0 : ret;
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
#define MAX(x, y) (((x) > (y)) ? (x) : (y))
#define MIN(x, y) (((x) < (y)) ? (x) : (y))
    bool only_detector_diff = true;
    // make sure only detection related outputs are changed
    for (int i = MIN(nb_outputs, h->nb_outputs); i < MAX(nb_outputs, h->nb_outputs); i++) {
      if (!h->outputs[i].is_dnn_profile)
        only_detector_diff = false;
    }
    if (only_detector_diff) {
      h->nb_outputs = nb_outputs;
    } else {
      return lpms_ERR_OUTPUTS;
    }
#undef MAX
#undef MIN
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
    if (handle->ictx.transmuxing && handle->outputs[i].oc) {
        av_write_trailer(handle->outputs[i].oc);
    }
    free_output(&handle->outputs[i]);
  }

  if (handle->dnn_filtergraph) avfilter_graph_free(&handle->dnn_filtergraph);

  free(handle);
}

static AVFilterGraph * create_dnn_filtergraph(lvpdnn_opts *dnn_opts)
{
  const AVFilter *filter = NULL;
  AVFilterContext *filter_ctx = NULL;
  AVFilterGraph *graph_ctx = NULL;
  int ret = 0;
  char errstr[512];
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

struct transcode_thread* lpms_transcode_new_with_dnn(lvpdnn_opts *dnn_opts)
{
  struct transcode_thread *h = malloc(sizeof (struct transcode_thread));
  if (!h) return NULL;
  memset(h, 0, sizeof *h);
  AVFilterGraph *filtergraph = create_dnn_filtergraph(dnn_opts);
  if (!filtergraph) {
      free(h);
      h = NULL;
  } else {
      h->dnn_filtergraph = filtergraph;
  }
  return h;
}

void lpms_transcode_discontinuity(struct transcode_thread *handle) {
  if (!handle)
    return;
  handle->ictx.discontinuity = 1;
}
