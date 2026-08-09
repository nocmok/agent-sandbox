FROM alpine:3.20

RUN apk add --no-cache nfs-utils

ENV SHARED_DIRECTORY=/exports
RUN mkdir -p /exports

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 2049 111/tcp 111/udp

ENTRYPOINT ["/entrypoint.sh"]
