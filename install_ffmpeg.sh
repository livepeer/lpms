#!/usr/bin/env bash
echo 'WARNING: downloading and executing go-livepeer/install_ffmpeg.sh, use it directly in case of issues'
wget -O - https://raw.githubusercontent.com/livepeer/go-livepeer/eli/no-npp/install_ffmpeg.sh | bash -s $1