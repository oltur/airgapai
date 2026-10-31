# The Forgejo runner plus what make check needs. Jobs run inside this container, because the
# namespace's Pod Security level rules out nested containers.
ARG BASE
FROM ${BASE}
USER root
RUN apk add --no-cache bash git grep make
COPY gitleaks /usr/local/bin/gitleaks
USER 1000:1000
