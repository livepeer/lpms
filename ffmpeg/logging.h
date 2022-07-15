#ifndef _LPMS_LOGGING_H_
#define _LPMS_LOGGING_H_

// LOGGING MACROS

#define LPMS_ERR(label, msg) {\
char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
if (!ret) ret = AVERROR(EINVAL); \
if (ret <-1) av_strerror(ret, errstr, sizeof errstr); \
av_log(NULL, AV_LOG_ERROR, "ERROR: %s:%d] %s : %s\n", __FILE__, __LINE__, msg, errstr); \
goto label; \
}

#define LPMS_ERR_RETURN(msg) {\
char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
if (!ret) ret = AVERROR(EINVAL); \
if (ret <-1) av_strerror(ret, errstr, sizeof errstr); \
av_log(NULL, AV_LOG_ERROR, "ERROR: %s:%d] %s : %s\n", __FILE__, __LINE__, msg, errstr); \
return ret; \
}

#define LPMS_ERR_BREAK(msg) {\
char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
if (!ret) ret = AVERROR(EINVAL); \
if (ret <-1) av_strerror(ret, errstr, sizeof errstr); \
av_log(NULL, AV_LOG_ERROR, "ERROR: %s:%d] %s : %s\n", __FILE__, __LINE__, msg, errstr); \
break; \
}

#define LPMS_WARN(msg) {\
av_log(NULL, AV_LOG_WARNING, "WARNING: %s:%d] %s\n", __FILE__, __LINE__, msg); \
}

#define LPMS_INFO(msg) {\
av_log(NULL, AV_LOG_INFO, "%s:%d] %s\n", __FILE__, __LINE__, msg); \
}

#define LPMS_DEBUG(msg) {\
av_log(NULL, AV_LOG_DEBUG, "%s:%d] %s\n", __FILE__, __LINE__, msg); \
}

#endif // _LPMS_LOGGING_H_
