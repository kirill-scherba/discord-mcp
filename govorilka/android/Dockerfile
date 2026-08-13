# Build Govorilka Android APK in a container (no SDK on host).
# Usage: docker build -t govorilka-android . && docker run --rm -v $PWD/app/build:/app/app/build govorilka-android

FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
ENV ANDROID_HOME=/opt/android-sdk
ENV ANDROID_SDK_ROOT=/opt/android-sdk
ENV PATH=$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools

# Base packages + Gradle (for wrapper generation)
RUN apt-get update && apt-get install -y --no-install-recommends \
    openjdk-17-jdk-headless \
    wget unzip git curl \
    && wget -q https://services.gradle.org/distributions/gradle-8.5-bin.zip -O /tmp/gradle.zip \
    && unzip -q /tmp/gradle.zip -d /opt \
    && rm /tmp/gradle.zip \
    && rm -rf /var/lib/apt/lists/*

ENV PATH=$PATH:/opt/gradle-8.5/bin

# Android command-line tools (sdkmanager)
RUN mkdir -p $ANDROID_HOME/cmdline-tools \
    && wget -q https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip -O /tmp/cmdtools.zip \
    && unzip -q /tmp/cmdtools.zip -d $ANDROID_HOME/cmdline-tools \
    && mv $ANDROID_HOME/cmdline-tools/cmdline-tools $ANDROID_HOME/cmdline-tools/latest \
    && rm /tmp/cmdtools.zip

# Accept licenses and install SDK components
RUN yes | sdkmanager --licenses > /dev/null 2>&1 \
    && sdkmanager --install "platforms;android-34" "build-tools;34.0.0" "platform-tools"

WORKDIR /app
COPY . .

# Generate the gradle wrapper once, then build the debug APK.
# (Файлы проекта скопированы прямо в /app, т.к. Dockerfile лежит в android/.)
CMD ["bash", "-c", "gradle wrapper --no-daemon 2>&1 | tail -5 && ./gradlew assembleDebug --no-daemon 2>&1 | tail -40"]
