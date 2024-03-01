package main

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	versionCollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "syslog_ng" // For Prometheus metrics.
)

var (
	app              = kingpin.New("syslog_ng_exporter", "A Syslog-NG exporter for Prometheus")
	slogLevel        = app.Flag("loglevel", "Log level (debug, info, warn, error)").Default("info").String()
	slogFormat       = app.Flag("logformat", "Log format (json, text)").Default("text").String()
	showVersion      = app.Flag("version", "Print version information.").Bool()
	listeningAddress = app.Flag("telemetry.address", "Address on which to expose metrics.").Default(":9577").String()
	metricsEndpoint  = app.Flag("telemetry.endpoint", "Path under which to expose metrics.").Default("/metrics").String()
	socketPath       = app.Flag("socket.path", "Path to syslog-ng control socket.").Default("/var/lib/syslog-ng/syslog-ng.ctl").String()
)

var (
	// Filled by goreleaser
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type Exporter struct {
	sockPath string
	mutex    sync.Mutex

	srcProcessed   *prometheus.Desc
	dstProcessed   *prometheus.Desc
	dstDropped     *prometheus.Desc
	dstStored      *prometheus.Desc
	dstWritten     *prometheus.Desc
	dstMemory      *prometheus.Desc
	up             *prometheus.Desc
	scrapeFailures prometheus.Counter
}

type Stat struct {
	objectType string
	id         string
	instance   string
	state      string
	metric     string
	value      float64
}

func NewExporter(path string) *Exporter {
	return &Exporter{
		sockPath: path,
		srcProcessed: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "source_messages_processed", "total"),
			"Number of messages processed by this source.",
			[]string{"type", "id", "source"},
			nil),
		dstProcessed: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "destination_messages_processed", "total"),
			"Number of messages processed by this destination.",
			[]string{"type", "id", "destination"},
			nil),
		dstDropped: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "destination_messages_dropped", "total"),
			"Number of messages dropped by this destination due to store overflow.",
			[]string{"type", "id", "destination"},
			nil),
		dstStored: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "destination_messages_stored", "total"),
			"Number of messages currently stored for this destination.",
			[]string{"type", "id", "destination"},
			nil),
		dstWritten: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "destination_messages_written", "total"),
			"Number of messages successfully written by this destination.",
			[]string{"type", "id", "destination"},
			nil),
		dstMemory: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "destination_bytes_stored", "total"),
			"Bytes of memory currently used to store messages for this destination.",
			[]string{"type", "id", "destination"},
			nil),
		up: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "up"),
			"Reads 1 if the syslog-ng server could be reached, else 0.",
			nil,
			nil),
		scrapeFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrape_failures_total",
			Help:      "Number of errors while scraping syslog-ng.",
		}),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.srcProcessed
	ch <- e.dstProcessed
	ch <- e.dstDropped
	ch <- e.dstStored
	ch <- e.dstWritten
	ch <- e.dstMemory
	ch <- e.up
	e.scrapeFailures.Describe(ch)
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	if err := e.collect(ch); err != nil {
		slog.Error("Error scraping syslog-ng", "error", err)
		e.scrapeFailures.Inc()
		e.scrapeFailures.Collect(ch)
	}
}

func (e *Exporter) collect(ch chan<- prometheus.Metric) error {
	conn, err := net.Dial("unix", e.sockPath)
	if err != nil {
		ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 0)
		return fmt.Errorf("Error connecting to syslog-ng: %v", err)
	}

	defer conn.Close()

	err = conn.SetDeadline(time.Now().Add(time.Second))
	if err != nil {
		return fmt.Errorf("Failed to set conn deadline: %v", err)
	}

	_, err = conn.Write([]byte("STATS\n"))
	if err != nil {
		ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 0)
		return fmt.Errorf("Error writing to control socket: %v", err)
	}

	buff := bufio.NewReader(conn)

	_, err = buff.ReadString('\n')
	if err != nil {
		return fmt.Errorf("Error reading header from control socket: %v", err)
	}

	ch <- prometheus.MustNewConstMetric(e.up, prometheus.GaugeValue, 1)

	for {
		line, err := buff.ReadString('\n')

		if err != nil || line[0] == '.' {
			slog.Debug("Reached end of STATS output")
			break
		}

		stat, err := parseStatLine(line)
		if err != nil {
			slog.Debug("Skipping STATS line", "line", line, "error", err)
			continue
		}

		slog.Debug("Successfully parsed STATS line", "stats", stat)

		switch stat.objectType[0:4] {
		case "src.", "sour":
			switch stat.metric {
			case "processed":
				ch <- prometheus.MustNewConstMetric(e.srcProcessed, prometheus.CounterValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			}

		case "dst.", "dest":
			switch stat.metric {
			case "dropped":
				ch <- prometheus.MustNewConstMetric(e.dstDropped, prometheus.CounterValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			case "processed":
				ch <- prometheus.MustNewConstMetric(e.dstProcessed, prometheus.CounterValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			case "written":
				ch <- prometheus.MustNewConstMetric(e.dstWritten, prometheus.CounterValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			case "stored", "queued":
				ch <- prometheus.MustNewConstMetric(e.dstStored, prometheus.GaugeValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			case "memory_usage":
				ch <- prometheus.MustNewConstMetric(e.dstMemory, prometheus.GaugeValue,
					stat.value, stat.objectType, stat.id, stat.instance)
			}
		}
	}

	return nil
}

func parseStatLine(line string) (Stat, error) {
	part := strings.SplitN(strings.TrimSpace(line), ";", 6)
	if len(part) < 6 {
		return Stat{}, fmt.Errorf("insufficient parts: %d < 6", len(part))
	}

	if len(part[0]) < 4 {
		return Stat{}, fmt.Errorf("invalid name: %s", part[0])
	}

	val, err := strconv.ParseFloat(part[5], 64)
	if err != nil {
		return Stat{}, err
	}

	stat := Stat{part[0], part[1], part[2], part[3], part[4], val}

	return stat, nil
}

func parseLogLevel(in string) (slog.Level, error) {
	switch in {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errors.New("Invalid log level")
	}
}

func main() {
	kingpin.MustParse(app.Parse(os.Args[1:]))

	if *showVersion {
		fmt.Fprintf(os.Stdout, "syslog_ng_exporter %s, build date %s, commit %s\n", version, date, commit)
		os.Exit(0)
	}

	logLevel, err := parseLogLevel(*slogLevel)
	if err != nil {
		slog.Error("Invalid log level", "error", err)
		os.Exit(1)
	}

	var logger *slog.Logger
	switch *slogFormat {
	case "json":
		logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	case "text":
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	default:
		slog.Error("Invalid log format", "format", *slogFormat)
		os.Exit(1)
	}
	slog.SetDefault(logger)

	exporter := NewExporter(*socketPath)
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(versionCollector.NewCollector("syslog_ng_exporter"))

	slog.Info("Starting syslog_ng_exporter", "version", version, "listeningAddress", *listeningAddress, "metricsEndpoint", *metricsEndpoint)

	http.Handle(*metricsEndpoint, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
			<head><title>Syslog-NG Exporter</title></head>
			<body>
			<h1>Syslog-NG Exporter</h1>
			<p><a href='` + *metricsEndpoint + `'>Metrics</a></p>
			</body>
			</html>`))
		if err != nil {
			slog.Warn("Failed sending response", "error", err)
		}
	})
	err = (http.ListenAndServe(*listeningAddress, nil))
	if err != nil {
		slog.Error("Failed to start HTTP server", "error", err)
		os.Exit(1)
	}
}
