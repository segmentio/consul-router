package main

import (
	"context"
	"flag"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/apex/log"
	"github.com/apex/log/handlers/text"

	"github.com/segmentio/ecs-logs-go/apex"
	"github.com/segmentio/stats/datadog"
	"github.com/segmentio/stats/httpstats"
	"github.com/segmentio/stats/netstats"
)

func init() {
	if terminal.IsTerminal(1) {
		log.Log = &log.Logger{
			Handler: text.New(os.Stderr),
			Level:   log.DebugLevel,
		}
	} else {
		log.Log = &log.Logger{
			Handler: apex_ecslogs.NewHandler(os.Stderr),
			Level:   log.InfoLevel,
		}
	}
}

func main() {
	var config struct {
		http    string
		consul  string
		datadog string
		domain  string
		prefer  string
		health  string
		pprof   string

		cacheTimeout    time.Duration
		dialTimeout     time.Duration
		readTimeout     time.Duration
		writeTimeout    time.Duration
		idleTimeout     time.Duration
		shutdownTimeout time.Duration

		maxIdleConns        int
		maxIdleConnsPerHost int
		maxHeaderBytes      int
		enableCompression   bool
	}

	flag.StringVar(&config.http, "bind-http", ":4000", "The network address on which the router will listen for incoming connections")
	flag.StringVar(&config.pprof, "bind-pprof", "", "The network address on which router listens for profiling requests")
	flag.StringVar(&config.health, "bind-health-check", "", "The network address on which the router listens for health checks")
	flag.StringVar(&config.consul, "consul", "localhost:8500", "The address at which the router can access a consul agent")
	flag.StringVar(&config.datadog, "datadog", "localhost:8125", "The address at which the router will send datadog metrics")
	flag.StringVar(&config.domain, "domain", "localhost", "The domain for which the router will accept requests")
	flag.StringVar(&config.prefer, "prefer", "", "The services with a tag matching the preferred value will be favored by the router")
	flag.DurationVar(&config.cacheTimeout, "cache-timeout", 10*time.Second, "The timeout for cached hostnames")
	flag.DurationVar(&config.dialTimeout, "dial-timeout", 10*time.Second, "The timeout for dialing tcp connections")
	flag.DurationVar(&config.readTimeout, "read-timeout", 30*time.Second, "The timeout for reading http requests")
	flag.DurationVar(&config.writeTimeout, "write-timeout", 30*time.Second, "The timeout for writing http requests")
	flag.DurationVar(&config.idleTimeout, "idle-timeout", 90*time.Second, "The timeout for idle connections")
	flag.DurationVar(&config.shutdownTimeout, "shutdown-timeout", 10*time.Second, "The timeout for shutting down the router")
	flag.IntVar(&config.maxIdleConns, "max-idle-conns", 10000, "The maximum number of idle connections kept")
	flag.IntVar(&config.maxIdleConnsPerHost, "max-idle-conns-per-host", 100, "The maximum number of idle connections kept per host")
	flag.IntVar(&config.maxHeaderBytes, "max-header-bytes", 65536, "The maximum number of bytes allowed in http headers")
	flag.BoolVar(&config.enableCompression, "enable-compression", false, "When set the router will ask for compressed payloads")
	flag.Parse()

	// Atomic variable set to the http status returned by the http health check.
	healthStatus := uint32(http.StatusOK)

	// The datadog client that reports metrics generated by the router.
	dd := datadog.NewClient(datadog.ClientConfig{
		Address: config.datadog,
	})
	defer dd.Close()

	// The consul-based resolver used to lookup services.
	rslv := consulResolver{
		address: config.consul,
	}

	// The domain name served by the router, prefix with '.' so it doesn't have
	// to be done over and over in each http request.
	domain := config.domain
	if !strings.HasPrefix(domain, ".") {
		domain = "." + domain
	}

	// Start the health check server.
	if len(config.health) != 0 {
		go http.ListenAndServe(config.health, http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			res.WriteHeader(int(atomic.LoadUint32(&healthStatus)))
		}))
	}

	// Start hte profiler server.
	if len(config.pprof) != 0 {
		go http.ListenAndServe(config.pprof, nil)
	}

	// Configure the default http transport which is used for forwarding the requests.
	http.DefaultTransport = httpstats.NewTransport(nil, &http.Transport{
		DialContext:            dialer(config.dialTimeout),
		IdleConnTimeout:        config.idleTimeout,
		MaxIdleConns:           config.maxIdleConns,
		MaxIdleConnsPerHost:    config.maxIdleConnsPerHost,
		ResponseHeaderTimeout:  config.readTimeout,
		ExpectContinueTimeout:  config.readTimeout,
		MaxResponseHeaderBytes: int64(config.maxHeaderBytes),
		DisableCompression:     !config.enableCompression,
	})

	// Configure and run the http server.
	httpLstn, err := net.Listen("tcp", config.http)
	if err != nil {
		log.WithFields(log.Fields{
			"address": config.http,
			"error":   err,
		}).Fatal("failed to bind tcp address for http server")
	}

	httpStop := make(chan struct{})
	httpDone := make(chan struct{})
	go func() {
		switch err := (&http.Server{
			ReadTimeout:    config.readTimeout,
			WriteTimeout:   config.writeTimeout,
			MaxHeaderBytes: config.maxHeaderBytes,
			Handler: httpstats.NewHandler(nil, newServer(serverConfig{
				stop:         httpStop,
				done:         httpDone,
				rslv:         rslv,
				domain:       domain,
				prefer:       config.prefer,
				cacheTimeout: config.cacheTimeout,
			})),
		}).Serve(httpLstn); err {
		case nil, io.EOF:
		default:
			log.WithError(err).Fatal("failed to serve http requests")
		}
	}()

	// Gracefully shutdown when receiving a signal:
	// - set the health check status to 503
	// - close tcp connections
	// - wait for in-flight requests to complete
	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigchan
	log.WithField("signal", sig).Info("shutting down")

	atomic.StoreUint32(&healthStatus, http.StatusServiceUnavailable)
	httpLstn.Close()
	close(httpStop)

	for httpDone != nil {
		select {
		case <-time.After(config.shutdownTimeout):
			return
		case <-sigchan:
			return
		case <-httpDone:
			httpDone = nil
		}
	}
}

func dialer(timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: timeout,
	}
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, address)
		if conn != nil {
			conn = netstats.NewConn(nil, conn)
		}
		return conn, err
	}
}
