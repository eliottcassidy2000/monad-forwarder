FROM alpine
COPY monad-forwarder /usr/bin/example
ENTRYPOINT ["/usr/bin/example"]