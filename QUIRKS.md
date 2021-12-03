# FFmpeg Quirks in LPMS
This document outlines how LPMS tweaks FFmpeg transcoding pipeline to address some specific problems.

## Handle zero frame (audio-only) segments at the start of a session

### Problem
Livepeer used to reject transcoding requests when it is sent audio-only segments. This could happen variety of reasons. In the case of MistServer, it happens when a transcode is enabled and there is more audio data in the buffer than video data. It could also happen in a variety of edge cases where bandwidth is severely constrained and end-user client software decides to only send audio to keep something online. 

### Desired Behavior
Instead of erroring on such segments, let's accept them and send back audio-only tracks. The audio output should be exactly the same as the audio input.

### Our Solution
We bypass the usual transcoding process for audio-only segments and just return them back. To do this, we check if video frame is present, and if not present just copy the frames to the output as it is. The bypassing is done by forcing the copy transcoder https://github.com/livepeer/lpms/commit/8bc28e3f702049a17c24ab2041857a47d8af51bf for such segments.
While the by-pass check code is still implemented in our LPMS, this function hasn't been used on the transcoder directly, instead used on the Broadcaster side in https://github.com/livepeer/go-livepeer/pull/1933.

## Very-few-video-frame segment handling by introducing sentinel frames

### Problem
Hardware transcoding fails when livepeer is sent segments with very few video frames. It works fine when running software trascoding but fails in hardware trascoding. This is caused because internal buffers of Nvidia buffers are bigger than in software mode. This could have been addressed by using ffmpeg flushing at the end of each segment however, when we do ffmpeg flushing we need to close and reopen transcoding session and this is quite expensive in Nvidia. Instead of flushing, closing and reopening transcoding session for each segment, LPMS chose to reuse the session for different segments of the same stream.

### Solution
To solve the flushing problem while still reusing the session, we introduced so called sentinel-packets. Sentinel packets are dummy frames with -1 timestamps. We insert these packets at the end of each segment to make sure that the packets that are sent to the buffer earlier always get popped out. We wait until we receive the sentinel packets pack and if we receive sentinel packet we know that this is the end of the segment. The filter gives up after sending SENTINEL_MAX packets, which is a pre-processor constant defined as 5 for now.

https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/decoder.h#L31
https://github.com/livepeer/lpms/blob/fe330766146dba62f3e1fccd07a4b96fa1abcf4d/ffmpeg/decoder.c#L82

## Handling out-of-order frames

### Problem

LPMS transcoder used to fail when segments or frames come in messed order. This issue is caused when one segment failed to get uploaded to Transcoder due to poor network and gets delivered later in its retry attempts or when some frames get dropped due to poor network. The FPS filter expects the timestamps to increase monotonically and uniformly. If this requirement is not met, transcoding pipeline fails.

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

FPS filter expects monotonic increase in input frame's PTS. We cannot rely on the input to be monotonic thus:
Set a dummy PTS before the frame is sent into filtergraph, that we manually increase monotonically.

If the input had missing frames (jumps in PTS) or if we had used dummy PTS before for the fps filter - we would need to set the encoded frame's PTS manually to ensure correct order and timescaling after change in FPS. 

https://github.com/livepeer/lpms/blob/e0a6002c849649d80a470c2d19130b279291051b/ffmpeg/filter.c#L308
https://github.com/livepeer/lpms/blob/e0a6002c849649d80a470c2d19130b279291051b/ffmpeg/filter.c#L356

## Reusing transcoding session with HW codecs

### Problem

Transcoder initialization is slow when using Nvidia hardware codecs, because of CUDA runtime startup. It makes transcoding impractical with small (few seconds) segments, because initialization time becomes comparable with time spent transcoding.

### Solution

The solution is to re-use transcoding session, in the form of keeping Ffmpeg objects alive between segments. To support this, the pipeline code needs to:
1. Use the same thread for each subsequent video segment
2. Properly flush decoder buffers after each segment using sentinel frames, as detailed in tiny segment handling section

When software (CPU) codecs are selected for transcoding, as well as for audio codecs, the logic above is not required, because initialization is fast and feasible per-segment.

## References

1. Zero-Frame segments: 
          Issue - https://github.com/livepeer/lpms/issues/203
          PR -https://github.com/livepeer/lpms/pull/204
2. Few-Frame segments:
          Issue - https://github.com/livepeer/lpms/issues/168
          PR - https://github.com/livepeer/lpms/pull/189
3. Handling out-of-order frames:
          Issue - https://github.com/livepeer/lpms/issues/199
          PR - https://github.com/livepeer/lpms/pull/201


