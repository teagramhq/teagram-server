ARG PLAYWRIGHT_IMAGE
FROM mirror.gcr.io/library/node@sha256:5711a0d445a1af54af9589066c646df387d1831a608226f4cd694fc59e745059 AS node2418
FROM ${PLAYWRIGHT_IMAGE}

COPY --from=node2418 /usr/local/bin/node /usr/local/bin/node
ENV PATH="/usr/local/bin:/usr/bin:/bin"

WORKDIR /opt/real-server-fixture
RUN npm install --prefix /opt/real-server-fixture --save-exact --ignore-scripts --no-audit --no-fund playwright@1.61.1
COPY front.mjs probe.cjs /opt/real-server-fixture/
ENV NODE_PATH=/opt/real-server-fixture/node_modules \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
USER 1001:1001
