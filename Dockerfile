FROM gcr.io/distroless/static-debian11
COPY --chmod=555 bin/linux/app /
VOLUME /cfg
EXPOSE 80
ENTRYPOINT ["/app", "-addr", ":80", "-addressbooksfile", "/cfg/config.json"]
