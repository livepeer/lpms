#ifndef _LPMS_OUTPUT_QUEUE_H_
#define _LPMS_OUTPUT_QUEUE_H_

#include <libavformat/avformat.h>
#include <libavutil/thread.h>

typedef enum {
  BEGIN_OF_OUTPUT = 0x1,    // before first packet is muxed - headers, etc
                            // (these packets will have timestamps of -1)
  PACKET_OUTPUT = 0x2,      // data packet - has valid timestamp
  END_OF_OUTPUT = 0x4,      // end of current stream (trailers, also ts == -1)
  END_OF_ALL_OUTPUTS = 0x8  // very last packet, no data beyond
} PacketFlags;

typedef struct _OutputPacket {
  struct _OutputPacket *next;
  uint8_t *data;
  int size;
  int index;
  PacketFlags flags;
  int64_t timestamp;
} OutputPacket;

typedef struct {
  // These are called "pthread", but FFmpeg also has Windows implementation,
  // so we should be safe on all reasonable platforms
  pthread_cond_t condition;
  pthread_mutex_t mutex;
  OutputPacket *front;
  OutputPacket *back;
} OutputQueue;

typedef struct {
  // Queue to use
  OutputQueue *queue;
  int index;
  // Staging area for packets
  OutputPacket *staging_front;
  OutputPacket *staging_back;
} WriteContext;

// NOT THREAD SAFE
void queue_create(OutputQueue *queue);
void queue_destroy(OutputQueue *queue);
// setup glue logic to allow ctx to use queue as output
int queue_setup_as_output(OutputQueue *queue, WriteContext *wctx, AVFormatContext *ctx);
void queue_reset(OutputQueue *queue);

// THREAD SAFE
const OutputPacket *queue_peek_front(OutputQueue *queue);
void queue_pop_front(OutputQueue *queue);
void queue_push_staging(WriteContext *wctx, PacketFlags flags, int64_t timestamp);
int queue_push_end(OutputQueue *queue);

#endif


