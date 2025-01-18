
#include <libavutil/fifo.h>
#include <libavcodec/avcodec.h>

AVFifo* queue_create();
void queue_free(AVFifo **fifo);
int queue_write(AVFifo *fifo, const AVPacket *pkt, const AVFrame *frame, int decoder_return);
int queue_read(AVFifo *fifo, AVFrame *out_frame, AVPacket *out_pkt, int *stream_index, int *decoder_return);
