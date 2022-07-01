# FFmpeg Quirks in LPMS
This document outlines how LPMS tweaks FFmpeg transcoding pipeline to address some specific problems.

## Handle zero frame (audio-only) segments at the start of a session

### Problem
Livepeer rejected transcoding requests when sent audio-only segments, which could happen for a variety of reasons. In the case of MistServer, it could happen when there is more audio data in the buffer than video data. It could also happen in a variety of edge cases where bandwidth is severely constrained and end-user client software decides to only send audio to keep something online.

**Issue:** https://github.com/livepeer/lpms/issues/203
**Fix:** https://github.com/livepeer/lpms/issues/204

### Desired Behavior
Instead of erroring on such segments, let's accept them and send back audio-only tracks. The audio output should be exactly the same as the audio input.

### Our Solution
We bypass the usual transcoding process for audio-only segments and just return them back as-is. To do this, we can check if a valid video stream is present without any actual video frames, and if so we skip transcoding.

While [this check](https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/extras.c#L110-L117) is still implemented in LPMS, and was originally used by Transcoders, now this function is directly used by the Broadcaster since https://github.com/livepeer/go-livepeer/pull/1933.

## Very-few-video-frame segment handling by introducing sentinel frames

### Problem
Hardware transcoding fails when livepeer is sent segments with very few video frames. It works fine when running software trascoding but fails in hardware trascoding. This is caused because internal buffers of Nvidia's decoder are bigger compared to software decoder, and LPMS used a non-standard API for flushing the decoder buffers. Thus when the decoder had only received very few encoded frames for a very short segment, and didn't start emitting decoded frames before reaching EOF, we were unable to flush those few frames out.

**Issue:** https://github.com/livepeer/lpms/issues/168
**Fix:** https://github.com/livepeer/lpms/issues/189

### Solution
To solve the flushing problem while still reusing the session, we introduced so called sentinel-packets. Sentinel packets are created by copying the first (keyframe) packet of the segment, and replacing its timestamp with `-1`. We insert these packets at the end of each segment to make sure that the packets that are sent to the buffer earlier always get popped out. We wait until we receive the sentinel packet back and if we receive sentinel packet we know that we've flushed out all the actual frames of the segment. To handle edge-cases where the decoder is completely stuck, we only try sending SENTINEL_MAX packets (which is a [pre-processor constant](https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/decoder.h#L31) defined as 5 for now) and if we don't receive any decoded frames we give up on flushing.

## Handling out-of-order frames

### Problem

LPMS transcoding would fail when segments or frames were sent out-of-order. This might happen when a segment failed to get uploaded to the Transcoder due to poor network and gets delivered later in a retry attempts, or when some frames get dropped due to poor network. The FPS filter expects the timestamps to increase monotonically and uniformly. If this requirement is not met, the filter errors out and the transcoding fails.

**Issue:** https://github.com/livepeer/lpms/issues/199
**Fix:** https://github.com/livepeer/lpms/pull/201

### Solution

```
                                               FILTERGRAPH
                         +------------------------------------------------------+
                         |                                                      |
                         |      +------------+              +------------+      |
                         |      |            |              |            |      |
                         |      |            |              |            |      |
  +-----------+          |      |  SCALE     |              |  FPS       |      |         +-----------+
  |           |          |      |   filter   |              |   filter   |      |         |           |
  | decoded F +---------------->+            +------+------>+            +--------------->+ encoded F |
  |           |          |      |            |      ^       |            |      |         |           |
  +-----------+          |      |            |      |       |            |      |         +-----------+
                         |      |            |      |       |            |      |
     pts_in              |      |            |      |       |            |      |             pts_out
                         |      +------------+      |       +------------+      |    (2)
(non-monotonic &         |                          |                           |    (guess using pts_in & fps
 unreliable jumps)       +------------------------------------------------------+       to maintain same order)
                                                    |
                                                    |
                                                    |
                                                    +
                                           (1) dummy monotonic
                                                   pts
```

FPS filter expects monotonic increase in input frame's timestamps. As we cannot rely on the input to be monotonic, [we set dummy timestamps](https://github.com/livepeer/lpms/blob/e0a6002c849649d80a470c2d19130b279291051b/ffmpeg/filter.c#L308) that we manually increase monotonically, before frames are sent into the filtergraph. Later on, when the FPS filter has duplicated or dropped frames to match the target framerate, we [reconstruct the original timestamps](https://github.com/livepeer/lpms/blob/e0a6002c849649d80a470c2d19130b279291051b/ffmpeg/filter.c#L308) by taking a difference between the timestamps of the first frame of the segment before and after filtergraph, and applying this difference back to the ouptut in the timebase the encoder expects. This ensures the orignal playback order of the segments is restored in the transcoded output(s).

## Reusing transcoding session with HW codecs

### Problem

Transcoder initialization is slow when using Nvidia hardware codecs, because of CUDA runtime startup. It makes transcoding impractical with small (few seconds) segments, because initialization time becomes comparable with time spent transcoding.

### Solution

The solution is to re-use transcoding session, in the form of keeping Ffmpeg objects alive between segments. To support this, the pipeline code needs to:
1. Use the [same thread](https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/transcoder.c#L73-L82) for each subsequent video segment
2. Properly flush decoder buffers after each segment using sentinel frames, as detailed in tiny segment handling section
3. Use a [custom flushing API](https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/encoder.c#L342-L345) for the encoder, so that the same session can be re-used after flushing.

When software (CPU) codecs are selected for transcoding, as well as for audio codecs, the logic above is not required, because initialization is fast and feasible per-segment.

## Re-initializing transcoding session on audio changes

### Problem

Some MPEG-TS streams have segments [without audio packets](https://github.com/livepeer/lpms/issues/337). Such audioless segments may be encountered at any point of the stream. Because we re-use Ffmpeg context and demuxer between segments to save time on hardware codec initialization, renditions of such streams wouldn't ever have the audio, if first source segment didn't have it.

### Solution

The solution is to [keep](https://github.com/livepeer/lpms/blob/6ef0b4b0ed5bf34534298805492e0b3924cf9752/ffmpeg/ffmpeg.go#L91) track of segment audio stream information in the transcoding context, and react when there's a change.
There are two cases:
1. Video-only segment(s) is the first segment of the stream  
    In this case, when first segment with the audio is encountered, the Ffmpeg context is re-initialized by calling [open_input()](https://github.com/livepeer/lpms/blob/622b50738904a1c7d75a3b9650f1cf1341980670/ffmpeg/decoder.c#L298) function. After that, demuxer is aware of audio stream, and it will be copied to renditions.
2. Video-only segment(s) first encountered mid-stream  
No action is needed. Audio encoder simply won't get any packets from the demuxer, and rendition segment won't have audio packets either.
   
# Side effects
1. Important side effect of above solution is hardware context re-initialization. When using hardware encoders with 'slow' initialization, we will perform such initialization twice for 'no audio' > 'audio' stream, which may introduce additional latency mid-stream. At the time of writing, we don't know how often such streams are encountered in production environment. The consensus among developers is that even if such re-initialization happen, it still won't affect QoS, because hardware transcoding is, normally, many times faster than realtime.

