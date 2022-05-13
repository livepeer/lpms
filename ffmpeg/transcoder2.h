#ifndef _LPMS_TRANSCODER2_H_
#define _LPMS_TRANSCODER2_H_

#ifdef __cplusplus
#define EXTERNC extern "C"
#define EXTERNC_INCLUDE_BEGIN extern "C" {
#define EXTERNC_INCLUDE_END }

#else
#define EXTERNC
#define EXTERNC_INCLUDE_BEGIN
#define EXTERNC_INCLUDE_END
#endif

EXTERNC_INCLUDE_BEGIN
#include "transcoder.h"
EXTERNC_INCLUDE_END

EXTERNC int  lpms_transcode2(void *handle, input_params *inp, output_params *params, output_results *results, int nb_outputs, output_results *decoded_results);
EXTERNC void *lpms_transcode2_new();
EXTERNC void lpms_transcode2_stop(void *handle);

#endif

