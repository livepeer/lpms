#include <libavformat/avformat.h>
#include <libavutil/thread.h>

typedef enum {
  END_OF_STREAM = 0x1, // end of current stream
  END_OF_QUEUE = 0x2,  // end of all streams on this side
  STREAM_ERROR = 0x4   // some kind of error occured while loading data into
                       // queue (this for example to make "invalid file" kind
                       // of tests happy)
} StreamFlags;

typedef enum {
  OTHER_ERROR = 0,     // error without dedicated handling
  NO_ENTRY = 1,        // ENOENT
} StreamErrors;

// This is a single portion of data from the stream. It may contain either
// input or output data, and it is suitable for use with StreamBufferQueue
struct StreamBuffer;

typedef struct _StreamBuffer {
  // data members
  char *data;         // stream data
  int size;           // number of bytes in the buffer
  int index;          // index of output, when used with output queue
  StreamFlags flags;  // end of stream flag, all end flag
  StreamErrors error; // actual error encountered by I/O code if STREAM_ERROR
                      // flag is set
  // list pointer - only queue should access it
  struct _StreamBuffer *next;
} StreamBuffer;

// Queue that allows for connecting demuxer as consumer, and muxer as a producer
// with other threads in safe way. Standard condition variable and mutex are
// used to accomplish this
typedef struct {
  // These are called "pthread", but FFmpeg also has Windows implementation,
  // so we should be safe on all reasonable platforms
  pthread_cond_t condition;
  pthread_mutex_t mutex;
  // Queue front and back
  StreamBuffer *front;
  StreamBuffer *back;
} StreamBufferQueue;

// Reading has to have a bit of context, due to the following reasons:
// - demuxer may wish to consume data in portions not alignet to the
// buffer size (which in turn comes from network or file I/O)
// - we want to support limited seek operation (as in "return to begin of
// stream)
typedef struct {
  StreamBufferQueue *queue;
  int offset;
  const StreamBuffer *current;
  int64_t position;
} ReadContext;

// NOT THREAD SAFE
void queue_create(StreamBufferQueue *queue);
void queue_destroy(StreamBufferQueue *queue);
// prepare read context for given queue
void queue_setup_read_context(StreamBufferQueue *queue, ReadContext *rctx);
// setup glue logic to allow ctx to use queue as input
int queue_setup_as_input(AVFormatContext *ctx, ReadContext *rctx);

// All functions below are thread-safe

// This adds buffer to the back of a queue. Will never block.
// Queue will take ownership of both buffer structure and its data member,
// these are expected to be allocated via standard malloc(), and will get
// released by free() on queue_pop_front() when the buffer will be front buffer
// of the queue
void queue_push_back(StreamBufferQueue *queue, StreamBuffer *buffer);
// This returns pointer to the front buffer of the queue, blocking if there
// isn't any. Once first element will get added, the function will return it
const StreamBuffer *queue_peek_front(StreamBufferQueue *queue);
// This releases front element of the queue, blocking if there isn't any. Once
// first element will get added, the function will release it and return
void queue_pop_front(StreamBufferQueue *queue);
// This removes all elements from the queue. Will never block.
void queue_remove_all(StreamBufferQueue *queue);
// This allows for iterating over queue's contents. current must not be NULL!
// If current has END_OF_QUEUE attribute, NULL will be returned. Otherwise
// next element is retuned, and if there is no next element yet, function will
// block
const StreamBuffer *queue_next(StreamBufferQueue *queue,
                               const StreamBuffer * current);


