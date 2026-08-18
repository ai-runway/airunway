# check=skip=InvalidDefaultArgInFrom
# Required from /versions.env; no fallback is allowed to drift from that pin.
ARG OPENCLAW_BASE
FROM ${OPENCLAW_BASE}

ENV HOME=/tmp/airunway-openclaw \
    NODE_ENV=production

COPY --chmod=0555 images/agents/openclaw/entrypoint.mjs images/agents/openclaw/model_provider.mjs images/agents/openclaw/runtime_proxy.mjs /opt/airunway/

USER 65532:65532
EXPOSE 18789
ENTRYPOINT ["tini", "-s", "--", "node", "/opt/airunway/entrypoint.mjs"]
CMD []
