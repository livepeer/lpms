#Test script to run all the tests for continuous integration

set -eux

EXTRA_BUILD_TAGS=""
DEVICE_FLAGS="sw"

if which clang > /dev/null; then
    EXTRA_BUILD_TAGS="--tags=nvidia"
    DEVICE_FLAGS="nv 0"
fi

go test $EXTRA_BUILD_TAGS -timeout 30m ./...

go run cmd/transcoding/transcoding.go transcoder/test.ts P144p30fps16x9,P240p30fps16x9 $DEVICE_FLAGS

printf "\n\nAll Tests Passed\n\n"
