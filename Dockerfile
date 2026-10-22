FROM eclipse-temurin:21-jre-noble
ARG MORPHE_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
      android-tools-adb python3 python3-venv ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
RUN test -n "$MORPHE_VERSION" && mkdir -p /opt/morphe && \
    curl -fsSL -o /opt/morphe/morphe.jar \
      "https://github.com/MorpheApp/morphe-cli/releases/download/${MORPHE_VERSION}/morphe-desktop-${MORPHE_VERSION#v}-all.jar"
WORKDIR /app
COPY requirements.txt .
RUN python3 -m venv /opt/venv && /opt/venv/bin/pip install --no-cache-dir -r requirements.txt
ENV PATH="/opt/venv/bin:$PATH" DATA_DIR=/data MORPHE_JAR=/opt/morphe/morphe.jar
COPY patcher ./patcher
VOLUME ["/data", "/config", "/root/.android"]
ENTRYPOINT ["python3", "-m", "patcher"]
CMD ["run"]
