FROM scratch

COPY github-notifications-marker /github-notifications-marker

ENTRYPOINT ["/github-notifications-marker"]
