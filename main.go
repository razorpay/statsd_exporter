// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/prometheus/statsd_exporter/pkg/address"
	"github.com/prometheus/statsd_exporter/pkg/event"
	"github.com/prometheus/statsd_exporter/pkg/exporter"
	"github.com/prometheus/statsd_exporter/pkg/line"
	"github.com/prometheus/statsd_exporter/pkg/listener"
	"github.com/prometheus/statsd_exporter/pkg/mapper"
)

const (
	defaultHelp = "Metric autogenerated by statsd_exporter."
	regErrF     = "Failed to update metric"
)

var (
	eventStats = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_events_total",
			Help: "The total number of StatsD events seen.",
		},
		[]string{"type"},
	)
	eventsFlushed = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_event_queue_flushed_total",
			Help: "Number of times events were flushed to exporter",
		},
	)
	eventsUnmapped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_events_unmapped_total",
			Help: "The total number of StatsD events no mapping was found for.",
		})
	udpPackets = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_udp_packets_total",
			Help: "The total number of StatsD packets received over UDP.",
		},
	)
	tcpConnections = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_tcp_connections_total",
			Help: "The total number of TCP connections handled.",
		},
	)
	tcpErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_tcp_connection_errors_total",
			Help: "The number of errors encountered reading from TCP.",
		},
	)
	tcpLineTooLong = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_tcp_too_long_lines_total",
			Help: "The number of lines discarded due to being too long.",
		},
	)
	unixgramPackets = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_unixgram_packets_total",
			Help: "The total number of StatsD packets received over Unixgram.",
		},
	)
	linesReceived = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_lines_total",
			Help: "The total number of StatsD lines received.",
		},
	)
	samplesReceived = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_samples_total",
			Help: "The total number of StatsD samples received.",
		},
	)
	sampleErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_sample_errors_total",
			Help: "The total number of errors parsing StatsD samples.",
		},
		[]string{"reason"},
	)
	tagsReceived = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_tags_total",
			Help: "The total number of DogStatsD tags processed.",
		},
	)
	tagErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "statsd_exporter_tag_errors_total",
			Help: "The number of errors parsing DogStatsD tags.",
		},
	)
	configLoads = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_config_reloads_total",
			Help: "The number of configuration reloads.",
		},
		[]string{"outcome"},
	)
	mappingsCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "statsd_exporter_loaded_mappings",
		Help: "The current number of configured metric mappings.",
	})
	conflictingEventStats = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_events_conflict_total",
			Help: "The total number of StatsD events with conflicting names.",
		},
		[]string{"type"},
	)
	errorEventStats = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_events_error_total",
			Help: "The total number of StatsD events discarded due to errors.",
		},
		[]string{"reason"},
	)
	eventsActions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "statsd_exporter_events_actions_total",
			Help: "The total number of StatsD events by action.",
		},
		[]string{"action"},
	)
	metricsCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "statsd_exporter_metrics_total",
			Help: "The total number of metrics.",
		},
		[]string{"type"},
	)
)

func init() {
	prometheus.MustRegister(version.NewCollector("statsd_exporter"))
	prometheus.MustRegister(eventStats)
	prometheus.MustRegister(eventsFlushed)
	prometheus.MustRegister(eventsUnmapped)
	prometheus.MustRegister(udpPackets)
	prometheus.MustRegister(tcpConnections)
	prometheus.MustRegister(tcpErrors)
	prometheus.MustRegister(tcpLineTooLong)
	prometheus.MustRegister(unixgramPackets)
	prometheus.MustRegister(linesReceived)
	prometheus.MustRegister(samplesReceived)
	prometheus.MustRegister(sampleErrors)
	prometheus.MustRegister(tagsReceived)
	prometheus.MustRegister(tagErrors)
	prometheus.MustRegister(configLoads)
	prometheus.MustRegister(mappingsCount)
	prometheus.MustRegister(conflictingEventStats)
	prometheus.MustRegister(errorEventStats)
	prometheus.MustRegister(eventsActions)
	prometheus.MustRegister(metricsCount)
}

// uncheckedCollector wraps a Collector but its Describe method yields no Desc.
// This allows incoming metrics to have inconsistent label sets
type uncheckedCollector struct {
	c prometheus.Collector
}

func (u uncheckedCollector) Describe(_ chan<- *prometheus.Desc) {}
func (u uncheckedCollector) Collect(c chan<- prometheus.Metric) {
	u.c.Collect(c)
}

func serveHTTP(mux http.Handler, listenAddress string, logger log.Logger) {
	level.Error(logger).Log("msg", http.ListenAndServe(listenAddress, mux))
	os.Exit(1)
}

func sighupConfigReloader(fileName string, mapper *mapper.MetricMapper, cacheSize int, logger log.Logger, option mapper.CacheOption) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)

	for s := range signals {
		if fileName == "" {
			level.Warn(logger).Log("msg", "Received signal but no mapping config to reload", "signal", s)
			continue
		}

		level.Info(logger).Log("msg", "Received signal, attempting reload", "signal", s)

		reloadConfig(fileName, mapper, cacheSize, logger, option)
	}
}

func reloadConfig(fileName string, mapper *mapper.MetricMapper, cacheSize int, logger log.Logger, option mapper.CacheOption) {
	err := mapper.InitFromFile(fileName, cacheSize, option)
	if err != nil {
		level.Info(logger).Log("msg", "Error reloading config", "error", err)
		configLoads.WithLabelValues("failure").Inc()
	} else {
		level.Info(logger).Log("msg", "Config reloaded successfully")
		configLoads.WithLabelValues("success").Inc()
	}
}

func dumpFSM(mapper *mapper.MetricMapper, dumpFilename string, logger log.Logger) error {
	f, err := os.Create(dumpFilename)
	if err != nil {
		return err
	}
	level.Info(logger).Log("msg", "Start dumping FSM", "file_name", dumpFilename)
	w := bufio.NewWriter(f)
	mapper.FSM.DumpFSM(w)
	w.Flush()
	f.Close()
	level.Info(logger).Log("msg", "Finish dumping FSM")
	return nil
}

func main() {
	var (
		listenAddress        = kingpin.Flag("web.listen-address", "The address on which to expose the web interface and generated Prometheus metrics.").Default(":9102").String()
		enableLifecycle      = kingpin.Flag("web.enable-lifecycle", "Enable shutdown and reload via HTTP request.").Default("false").Bool()
		metricsEndpoint      = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		statsdListenUDP      = kingpin.Flag("statsd.listen-udp", "The UDP address on which to receive statsd metric lines. \"\" disables it.").Default(":9125").String()
		statsdListenTCP      = kingpin.Flag("statsd.listen-tcp", "The TCP address on which to receive statsd metric lines. \"\" disables it.").Default(":9125").String()
		statsdListenUnixgram = kingpin.Flag("statsd.listen-unixgram", "The Unixgram socket path to receive statsd metric lines in datagram. \"\" disables it.").Default("").String()
		// not using Int here because flag displays default in decimal, 0755 will show as 493
		statsdUnixSocketMode = kingpin.Flag("statsd.unixsocket-mode", "The permission mode of the unix socket.").Default("755").String()
		mappingConfig        = kingpin.Flag("statsd.mapping-config", "Metric mapping configuration file name.").String()
		readBuffer           = kingpin.Flag("statsd.read-buffer", "Size (in bytes) of the operating system's transmit read buffer associated with the UDP or Unixgram connection. Please make sure the kernel parameters net.core.rmem_max is set to a value greater than the value specified.").Int()
		cacheSize            = kingpin.Flag("statsd.cache-size", "Maximum size of your metric mapping cache. Relies on least recently used replacement policy if max size is reached.").Default("1000").Int()
		cacheType            = kingpin.Flag("statsd.cache-type", "Metric mapping cache type. Valid options are \"lru\" and \"random\"").Default("lru").Enum("lru", "random")
		eventQueueSize       = kingpin.Flag("statsd.event-queue-size", "Size of internal queue for processing events.").Default("10000").Int()
		eventFlushThreshold  = kingpin.Flag("statsd.event-flush-threshold", "Number of events to hold in queue before flushing.").Default("1000").Int()
		eventFlushInterval   = kingpin.Flag("statsd.event-flush-interval", "Maximum time between event queue flushes.").Default("200ms").Duration()
		dumpFSMPath          = kingpin.Flag("debug.dump-fsm", "The path to dump internal FSM generated for glob matching as Dot file.").Default("").String()
		checkConfig          = kingpin.Flag("check-config", "Check configuration and exit.").Default("false").Bool()
		dogstatsdTagsEnabled = kingpin.Flag("statsd.parse-dogstatsd-tags", "Parse DogStatsd style tags. Enabled by default.").Default("true").Bool()
		influxdbTagsEnabled  = kingpin.Flag("statsd.parse-influxdb-tags", "Parse InfluxDB style tags. Enabled by default.").Default("true").Bool()
		libratoTagsEnabled   = kingpin.Flag("statsd.parse-librato-tags", "Parse Librato style tags. Enabled by default.").Default("true").Bool()
		signalFXTagsEnabled  = kingpin.Flag("statsd.parse-signalfx-tags", "Parse SignalFX style tags. Enabled by default.").Default("true").Bool()
	)

	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.Version(version.Print("statsd_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	parser := line.NewParser()
	if *dogstatsdTagsEnabled {
		parser.EnableDogstatsdParsing()
	}
	if *influxdbTagsEnabled {
		parser.EnableInfluxdbParsing()
	}
	if *libratoTagsEnabled {
		parser.EnableLibratoParsing()
	}
	if *signalFXTagsEnabled {
		parser.EnableSignalFXParsing()
	}

	cacheOption := mapper.WithCacheType(*cacheType)

	if *statsdListenUDP == "" && *statsdListenTCP == "" && *statsdListenUnixgram == "" {
		level.Error(logger).Log("At least one of UDP/TCP/Unixgram listeners must be specified.")
		os.Exit(1)
	}

	level.Info(logger).Log("msg", "Starting StatsD -> Prometheus Exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "context", version.BuildContext())
	level.Info(logger).Log("msg", "Accepting StatsD Traffic", "udp", *statsdListenUDP, "tcp", *statsdListenTCP, "unixgram", *statsdListenUnixgram)
	level.Info(logger).Log("msg", "Accepting Prometheus Requests", "addr", *listenAddress)

	events := make(chan event.Events, *eventQueueSize)
	defer close(events)
	eventQueue := event.NewEventQueue(events, *eventFlushThreshold, *eventFlushInterval, eventsFlushed)

	if *statsdListenUDP != "" {
		udpListenAddr, err := address.UDPAddrFromString(*statsdListenUDP)
		if err != nil {
			level.Error(logger).Log("msg", "invalid UDP listen address", "address", *statsdListenUDP, "error", err)
			os.Exit(1)
		}
		uconn, err := net.ListenUDP("udp", udpListenAddr)
		if err != nil {
			level.Error(logger).Log("msg", "failed to start UDP listener", "error", err)
			os.Exit(1)
		}

		if *readBuffer != 0 {
			err = uconn.SetReadBuffer(*readBuffer)
			if err != nil {
				level.Error(logger).Log("msg", "error setting UDP read buffer", "error", err)
				os.Exit(1)
			}
		}

		ul := &listener.StatsDUDPListener{
			Conn:            uconn,
			EventHandler:    eventQueue,
			Logger:          logger,
			LineParser:      parser,
			UDPPackets:      udpPackets,
			LinesReceived:   linesReceived,
			EventsFlushed:   eventsFlushed,
			SampleErrors:    *sampleErrors,
			SamplesReceived: samplesReceived,
			TagErrors:       tagErrors,
			TagsReceived:    tagsReceived,
		}

		go ul.Listen()
	}

	if *statsdListenTCP != "" {
		tcpListenAddr, err := address.TCPAddrFromString(*statsdListenTCP)
		if err != nil {
			level.Error(logger).Log("msg", "invalid TCP listen address", "address", *statsdListenUDP, "error", err)
			os.Exit(1)
		}
		tconn, err := net.ListenTCP("tcp", tcpListenAddr)
		if err != nil {
			level.Error(logger).Log("msg", err)
			os.Exit(1)
		}
		defer tconn.Close()

		tl := &listener.StatsDTCPListener{
			Conn:            tconn,
			EventHandler:    eventQueue,
			Logger:          logger,
			LineParser:      parser,
			LinesReceived:   linesReceived,
			EventsFlushed:   eventsFlushed,
			SampleErrors:    *sampleErrors,
			SamplesReceived: samplesReceived,
			TagErrors:       tagErrors,
			TagsReceived:    tagsReceived,
			TCPConnections:  tcpConnections,
			TCPErrors:       tcpErrors,
			TCPLineTooLong:  tcpLineTooLong,
		}

		go tl.Listen()
	}

	if *statsdListenUnixgram != "" {
		var err error
		if _, err = os.Stat(*statsdListenUnixgram); !os.IsNotExist(err) {
			level.Error(logger).Log("msg", "Unixgram socket already exists", "socket_name", *statsdListenUnixgram)
			os.Exit(1)
		}
		uxgconn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{
			Net:  "unixgram",
			Name: *statsdListenUnixgram,
		})
		if err != nil {
			level.Error(logger).Log("msg", "failed to listen on Unixgram socket", "error", err)
			os.Exit(1)
		}

		defer uxgconn.Close()

		if *readBuffer != 0 {
			err = uxgconn.SetReadBuffer(*readBuffer)
			if err != nil {
				level.Error(logger).Log("msg", "error setting Unixgram read buffer", "error", err)
				os.Exit(1)
			}
		}

		ul := &listener.StatsDUnixgramListener{
			Conn:            uxgconn,
			EventHandler:    eventQueue,
			Logger:          logger,
			LineParser:      parser,
			UnixgramPackets: unixgramPackets,
			LinesReceived:   linesReceived,
			EventsFlushed:   eventsFlushed,
			SampleErrors:    *sampleErrors,
			SamplesReceived: samplesReceived,
			TagErrors:       tagErrors,
			TagsReceived:    tagsReceived,
		}

		go ul.Listen()

		// if it's an abstract unix domain socket, it won't exist on fs
		// so we can't chmod it either
		if _, err := os.Stat(*statsdListenUnixgram); !os.IsNotExist(err) {
			defer os.Remove(*statsdListenUnixgram)

			// convert the string to octet
			perm, err := strconv.ParseInt("0"+string(*statsdUnixSocketMode), 8, 32)
			if err != nil {
				level.Warn(logger).Log("Bad permission %s: %v, ignoring\n", *statsdUnixSocketMode, err)
			} else {
				err = os.Chmod(*statsdListenUnixgram, os.FileMode(perm))
				if err != nil {
					level.Warn(logger).Log("Failed to change unixgram socket permission: %v", err)
				}
			}
		}

	}

	mapper := &mapper.MetricMapper{MappingsCount: mappingsCount}
	if *mappingConfig != "" {
		err := mapper.InitFromFile(*mappingConfig, *cacheSize, cacheOption)
		if err != nil {
			level.Error(logger).Log("msg", "error loading config", "error", err)
			os.Exit(1)
		}
		if *dumpFSMPath != "" {
			err := dumpFSM(mapper, *dumpFSMPath, logger)
			if err != nil {
				level.Error(logger).Log("msg", "error dumping FSM", "error", err)
				// Failure to dump the FSM is an error (the user asked for it and it
				// didn't happen) but not fatal (the exporter is fully functional
				// afterwards).
			}
		}
	} else {
		mapper.InitCache(*cacheSize, cacheOption)
	}

	exporter := exporter.NewExporter(mapper, logger, eventsActions, eventsUnmapped, errorEventStats, eventStats, conflictingEventStats, metricsCount)

	if *checkConfig {
		level.Info(logger).Log("msg", "Configuration check successful, exiting")
		return
	}

	mux := http.NewServeMux()
	mux.Handle(*metricsEndpoint, promhttp.Handler())
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>StatsD Exporter</title></head>
			<body>
			<h1>StatsD Exporter</h1>
			<p><a href="` + *metricsEndpoint + `">Metrics</a></p>
			</body>
			</html>`))
	})
	if *enableLifecycle {
		mux.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				if *mappingConfig == "" {
					level.Warn(logger).Log("msg", "Received lifecycle api reload but no mapping config to reload")
					return
				}
				level.Info(logger).Log("msg", "Received lifecycle api reload, attempting reload")
				reloadConfig(*mappingConfig, mapper, *cacheSize, logger, cacheOption)
			}
		})
		mux.HandleFunc("/-/quit", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut || r.Method == http.MethodPost {
				level.Info(logger).Log("msg", "Received lifecycle api quit, exiting")
				os.Exit(0)
			}
		})
	}

	go serveHTTP(mux, *listenAddress, logger)

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go sighupConfigReloader(*mappingConfig, mapper, *cacheSize, logger, cacheOption)
	go exporter.Listen(events)

	<-signals
}
