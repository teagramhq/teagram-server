ARG PLAYWRIGHT_IMAGE
FROM ${PLAYWRIGHT_IMAGE} AS runtime

WORKDIR /opt/browser-acceptance

COPY deploy/browser-acceptance/package.json deploy/browser-acceptance/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund

COPY deploy/browser-acceptance/connect-observer.mjs ./connect-observer.mjs
COPY deploy/browser-acceptance/observer-service.mjs ./observer-service.mjs
COPY deploy/browser-acceptance/healthcheck.mjs ./healthcheck.mjs
COPY deploy/browser-acceptance/runtime.mjs ./runtime.mjs
COPY deploy/browser-acceptance/qa-runtime-adapter.mjs ./qa-runtime-adapter.mjs
COPY deploy/browser-acceptance/readiness-integration-smoke.mjs ./readiness-integration-smoke.mjs

RUN install -d -o 1000 -g 1000 -m 0700 /home/pwuser \
    && chmod 0555 /opt/browser-acceptance

USER 1000:1000
ENTRYPOINT ["node", "/opt/browser-acceptance/runtime.mjs"]
CMD ["readiness"]
