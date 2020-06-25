#Test script to run all the tests for continuous integration

set -eux

go test ./...

go run cmd/transcoding/transcoding.go transcoder/test.ts P144p30fps16x9,P240p30fps16x9 sw

printf "\n\nAll Tests Passed\n\n"
