FROM nvidia/cuda:11.2.0-cudnn8-runtime-ubuntu20.04

# set the github runner version
ARG RUNNER_VERSION="2.287.1"

# update package list and create user
RUN apt-get update -y && useradd -m devops
RUN usermod -aG sudo devops

RUN apt-get update \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y tzdata \
  && apt-get install -y software-properties-common curl apt-transport-https jq \
  && curl https://dl.google.com/go/go1.15.5.linux-amd64.tar.gz | tar -C /usr/local -xz \
  && curl -fsSL https://download.docker.com/linux/ubuntu/gpg | apt-key add - \
  && add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu $(lsb_release -cs)  stable" \
  && apt-key adv --keyserver keyserver.ubuntu.com --recv 15CF4D18AF4F7421 \
  && add-apt-repository "deb [arch=amd64] http://apt.llvm.org/xenial/ llvm-toolchain-xenial-8 main" \
  && apt-get update \
  && apt-get -y install clang clang-tools build-essential pkg-config autoconf sudo git python docker-ce-cli xxd netcat-openbsd libnuma-dev cmake

# Set Env
ENV PKG_CONFIG_PATH /home/devops/compiled/lib/pkgconfig
ENV LD_LIBRARY_PATH /home/devops/compiled/lib:/usr/local/lib:/usr/local/cuda-11.2/lib64:/usr/lib/x86_64-linux-gnu
ENV PATH $PATH:/usr/local/go/bin:/home/devops/compiled/bin:/home/devops/ffmpeg
ENV BUILD_TAGS "debug-video experimental"
ENV NVIDIA_VISIBLE_DEVICES all
ENV NVIDIA_DRIVER_CAPABILITIES compute,video,utility

# Give sudo permission to user "devops"
RUN echo 'devops ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers

# install go
RUN curl -O -L https://dl.google.com/go/go1.17.6.linux-amd64.tar.gz && rm -rf /usr/local/go && tar -C /usr/local -xzf go1.17.6.linux-amd64.tar.gz

# download and install GH actions runner
RUN cd /home/devops && mkdir actions-runner && cd actions-runner \
    && curl -O -L https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz \
    && tar xzf ./actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz && chown -R devops ~devops

RUN LIBTENSORFLOW_VERSION=2.6.3 \
  && curl -LO https://storage.googleapis.com/tensorflow/libtensorflow/libtensorflow-gpu-linux-x86_64-${LIBTENSORFLOW_VERSION}.tar.gz \
  && sudo tar -C /usr/local -xzf libtensorflow-gpu-linux-x86_64-${LIBTENSORFLOW_VERSION}.tar.gz \
  && sudo ldconfig

# Add mime type for ts
RUN sudo echo '<?xml version="1.0" encoding="UTF-8"?><mime-info xmlns="http://www.freedesktop.org/standards/shared-mime-info"><mime-type type="video/mp2t"><comment>ts</comment><glob pattern="*.ts"/></mime-type></mime-info>'>>/usr/share/mime/packages/custom_mime_type.xml
RUN sudo update-mime-database /usr/share/mime

# install additional runner dependencies
RUN /home/devops/actions-runner/bin/installdependencies.sh

USER devops

WORKDIR /home/devops/actions-runner

# create script to handle graceful container shutdown
RUN echo -e '\n\
#!/bin/bash \n\
set -e \n\
cleanup() { \n\
    ./config.sh remove --unattended --token $(cat .reg-token) \n\
}\n\
trap "cleanup" INT \n\
trap "cleanup" TERM \n\
echo "get new runner token via GitHub API" \n\
curl -sX POST -H "Authorization: token ${ACCESS_TOKEN}" https://api.github.com/repos/${ORGANIZATION}/${REPOSITORY}/actions/runners/registration-token | jq .token --raw-output > .reg-token \n\
echo "configure runner" \n\
./config.sh --url https://github.com/${ORGANIZATION}/${REPOSITORY} --token $(cat .reg-token) --name lpms-linux-runner --labels lpms-linux-runner --unattended --replace \n\
echo "start runner" \n\
./bin/runsvc.sh & wait $! \n\
'>create_and_run.sh

RUN chmod +x create_and_run.sh

ENTRYPOINT ["sh", "./create_and_run.sh"]
