# check=skip=InvalidDefaultArgInFrom
# Required from /versions.env; no fallback is allowed to drift from that pin.
ARG PYTHON_BASE
FROM ${PYTHON_BASE}

ENV HOME=/tmp/airunway-home \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

COPY images/agents/crewai/requirements.lock /tmp/requirements.lock
RUN pip install --no-cache-dir --require-hashes --requirement /tmp/requirements.lock \
    && rm /tmp/requirements.lock

WORKDIR /opt/airunway
COPY --chmod=0555 images/agents/python/airunway_runtime.py images/agents/crewai/adapter.py ./

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["python", "/opt/airunway/adapter.py"]
