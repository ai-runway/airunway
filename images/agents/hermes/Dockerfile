# check=skip=InvalidDefaultArgInFrom
# Required from /versions.env; no fallback is allowed to drift from that pin.
ARG HERMES_BASE
FROM ${HERMES_BASE}

ENV HOME=/tmp/airunway-hermes \
    HERMES_HOME=/tmp/airunway-hermes \
    HERMES_WRITE_SAFE_ROOT=/tmp/airunway-hermes \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

COPY --chmod=0555 images/agents/hermes/entrypoint.py /opt/airunway/entrypoint.py

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/opt/hermes/.venv/bin/python", "/opt/airunway/entrypoint.py"]
CMD []
