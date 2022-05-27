#include "transcoder2.h"

class Transcoder {
private:
    transcode_thread *implementation;
public:
    Transcoder()
    {
        implementation = lpms_transcode_new();
    }
    int transcode(
        input_params *inp,
        output_params *params,
        output_results *results,
        int nb_outputs,
        output_results *decoded_results)
    {
        inp->handle = implementation;
        return lpms_transcode(inp, params, results, nb_outputs, decoded_results, 1);
    }
    void stop()
    {
        lpms_transcode_stop(implementation);
    }
};

extern "C" int lpms_transcode2(
    void *handle,
    input_params *inp,
    output_params *params,
    output_results *results,
    int nb_outputs,
    output_results *decoded_results)
{
    Transcoder *transcoder = reinterpret_cast<Transcoder *>(handle);
    return transcoder->transcode(inp, params, results, nb_outputs, decoded_results);
}

extern "C" void *lpms_transcode2_new()
{
    return new Transcoder();
}

extern "C" void lpms_transcode2_stop(void *handle)
{
    Transcoder *transcoder = reinterpret_cast<Transcoder *>(handle);
    transcoder->stop();
}

