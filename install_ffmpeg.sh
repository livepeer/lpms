#!/usr/bin/env bash

set -ex

export PATH="$HOME/compiled/bin":$PATH
export PKG_CONFIG_PATH=$HOME/compiled/lib/pkgconfig

# NVENC only works on Windows/Linux
if [ $(uname) != "Darwin" ]; then
  if [ ! -e "$HOME/nv-codec-headers" ]; then
    git clone https://git.videolan.org/git/ffmpeg/nv-codec-headers.git "$HOME/nv-codec-headers"
    cd $HOME/nv-codec-headers
    git checkout 250292dd20af60edc6e0d07f1d6e489a2f8e1c44
    make -e PREFIX="$HOME/compiled"
    make install -e PREFIX="$HOME/compiled"
  fi
fi

if [ ! -e "$HOME/nasm/nasm" ]; then
  # sudo apt-get -y install asciidoc xmlto # this fails :(
  git clone -b nasm-2.14.02 https://repo.or.cz/nasm.git "$HOME/nasm"
  cd "$HOME/nasm"
  ./autogen.sh
  ./configure --prefix="$HOME/compiled"
  make
  make install || echo "Installing docs fails but should be OK otherwise"
fi

if [ ! -e "$HOME/x264/x264" ]; then
  git clone http://git.videolan.org/git/x264.git "$HOME/x264"
  cd "$HOME/x264"
  # git master as of this writing
  git checkout 545de2ffec6ae9a80738de1b2c8cf820249a2530
  ./configure --prefix="$HOME/compiled" --enable-pic --enable-static
  make
  make install-lib-static
fi

EXTRA_FFMPEG_FLAGS=""
EXTRA_LDFLAGS=""

if [ $(uname) == "Darwin" ]; then
  EXTRA_LDFLAGS="-framework CoreFoundation -framework Security"
else
  # If we have clang, we can compile with CUDA support!
  if which clang > /dev/null; then
    echo "clang detected, building with GPU support"

    EXTRA_FFMPEG_FLAGS="--enable-cuda --enable-cuda-llvm --enable-cuvid --enable-nvenc --enable-decoder=h264_cuvid --enable-filter=scale_cuda,signature_cuda --enable-encoder=h264_nvenc"

    if [[ $BUILD_TAGS == *"experimental"* ]]; then
        echo "experimental tag detected, building with Tensorflow support"
        EXTRA_FFMPEG_FLAGS="$EXTRA_FFMPEG_FLAGS --enable-libtensorflow"
    fi
  fi
fi

if [ ! -e "$HOME/ffmpeg/libavcodec/libavcodec.a" ]; then
  #LIBTENSORFLOW_VERSION=2.3.0 \
  #&& curl -LO https://storage.googleapis.com/tensorflow/libtensorflow/libtensorflow-cpu-linux-x86_64-${LIBTENSORFLOW_VERSION}.tar.gz \
  #&& sudo tar -C /usr/local -xzf libtensorflow-cpu-linux-x86_64-${LIBTENSORFLOW_VERSION}.tar.gz \
  #&& sudo ldconfig  
  # --enable-libtensorflow

  git clone https://github.com/livepeer/FFmpeg.git "$HOME/ffmpeg" || echo "FFmpeg dir already exists"
  cd "$HOME/ffmpeg"

  git checkout e0eebeeeddf863f72da0232f9dddc05200340560 

  ./configure --prefix="$HOME/compiled" --enable-libx264 --enable-gpl --enable-static $EXTRA_FFMPEG_FLAGS
  make
  make install
fi
