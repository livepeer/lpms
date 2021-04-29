#ifndef _LPMS_LOGGING_H_
#define _LPMS_LOGGING_H_

//MACRO FOR DEVELOP
#define USE_LVPDNN_
#ifdef USE_LVPDNN_
#define MAX_CLASSIFY_SIZE 10
#define LVPDNN_FILTER_NAME "lvpdnn"
#define LVPDNN_FILTER_META "lavfi.lvpdnn.text"
#endif
// LOGGING MACROS

#define LPMS_ERR(label, msg) {\
char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
if (!ret) ret = AVERROR(EINVAL); \
if (ret <-1) av_strerror(ret, errstr, sizeof errstr); \
av_log(NULL, AV_LOG_ERROR, "ERROR: %s:%d] %s : %s\n", __FILE__, __LINE__, msg, errstr); \
goto label; \
}

#define LPMS_WARN(msg) {\
av_log(NULL, AV_LOG_WARNING, "WARNING: %s:%d] %s\n", __FILE__, __LINE__, msg); \
}

#define LPMS_INFO(msg) {\
av_log(NULL, AV_LOG_INFO, "%s:%d] %s\n", __FILE__, __LINE__, msg); \
}

#endif // _LPMS_LOGGING_H_
