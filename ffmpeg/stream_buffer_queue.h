#include <libavutil/thread.h>

typedef enum {
  END_OF_STREAM = 0x1,
  END_OF_ALL_STREAMS = 0x2
} StreamFlags;

struct StreamBuffer;

typedef struct _StreamBuffer {
  // data members
  char *data;         // stream data
  size_t size;        // number of bytes in the buffer
  int index;          // index of output, when used with output queue
  StreamFlags flags;  // end of stream flag, all end flag
  // list pointer - only queue should access it
  struct _StreamBuffer *previous;
} StreamBuffer;

typedef struct {
  // These are called "pthread", but FFmpeg also has Windows implementation,
  // so we should be safe on all reasonable platforms
  pthread_cond_t condition;
  pthread_mutex_t mutex;
  // Queue front and back
  StreamBuffer *front;
  StreamBuffer *back;
} StreamBufferQueue;

// NOT THREAD SAFE
void queue_create(StreamBufferQueue *queue);
void queue_destroy(StreamBufferQueue *queue);

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

