#ifndef _LPMS_STREAM_BUFFER_H_
#define _LPMS_STREAM_BUFFER_H_

#include <libavformat/avformat.h>
#include <libavutil/thread.h>

#define STREAM_BUFFER_BYTES (8 * 1024 * 1024)
#define PROTECTED_BYTES 1024

typedef enum {
  END_OF_STREAM = 0x1, // end of current stream
  STREAM_ERROR = 0x2   // some kind of error occured while loading data into
                       // queue (this for example to make "invalid file" kind
                       // of tests happy)
} StreamFlags;

typedef enum {
  OTHER_ERROR = 0,     // error without dedicated handling
  NO_ENTRY = 1,        // ENOENT
} StreamErrors;

typedef struct {
  // These are called "pthread", but FFmpeg also has Windows implementation,
  // so we should be safe on all reasonable platforms
  pthread_cond_t condition_put; // signalled when data gets added or flags change
  pthread_cond_t condition_get; // signalled when data gets read
  pthread_mutex_t mutex;
  // This is a circular buffer. It follows typical "index and the size" approach
  // with several caveats
  // 1) There is no "size", two values are used instead: "read_bytes" and
  // "unread_bytes". Read bytes are the ones that were already accessed and
  // _usually_ could be removed, except that we need to support (limited) seek()
  // functionality for FFmpeg demuxing. So unlike typical circular buffer, bytes
  // are not removed immediately on read() by moving input index and subtracting
  // from size. Instead the number of bytes just read is added to "read_bytes"
  // and subtracted from "unread_bytes", so by default no bytes are freed
  // 2) Adding new data into the buffer initially only affects "unread_bytes".
  // However, when there is not enough free data in the buffer, some of the
  // bytes already read have to be written over, and so both index and
  // "read_bytes" are affected. Care is taken to leave at least PROTECTED_BYTES
  // untouched, so seek back is always possible within PROTECTED_BYTES range
  int64_t index;
  int64_t read_bytes;
  int64_t unread_bytes;
  uint8_t *data;
  StreamFlags flags;
  StreamErrors error;
} StreamBuffer;

// NOT THREAD SAFE
int buffer_create(StreamBuffer *buffer);
void buffer_destroy(StreamBuffer *buffer);
// setup glue logic to allow ctx to use buffer as input
int buffer_setup_as_input(StreamBuffer *buffer, AVFormatContext *ctx);
void buffer_reset(StreamBuffer *buffer);

// THREAD SAFE
// This adds bytes to the unread data in the buffer. Usually the function will
// not block, unless there is not enough place in the buffer. Then it will block
// waiting for a chance to write data. Bytes are copied.
void buffer_put_bytes(StreamBuffer *buffer, uint8_t *bytes, int64_t size);
void buffer_end_of_stream(StreamBuffer *buffer);
void buffer_error(StreamBuffer *buffer, StreamErrors error);

#endif

