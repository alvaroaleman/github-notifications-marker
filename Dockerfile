FROM curlimages/curl:latest AS certs

FROM scratch

COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY github-notifications-marker /github-notifications-marker

ENTRYPOINT ["/github-notifications-marker"]
