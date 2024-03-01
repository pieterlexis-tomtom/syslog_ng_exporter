FROM quay.io/prometheus/busybox:latest
LABEL maintainer="Brad Davidson <brad@oatmail.org>"
COPY syslog_ng_exporter /bin/syslog_ng_exporter
ENTRYPOINT ["/bin/syslog_ng_exporter"]
EXPOSE     9577
