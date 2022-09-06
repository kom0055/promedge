// Copyright 2015 The Prometheus Authors
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

// The main package for the Prometheus server executable.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net"
	"net/http"
	_ "net/http/pprof" // Comment this line to disable pprof endpoint.
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/regexp"
	"github.com/mwitkow/go-conntrack"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/promlog"
	promlogflag "github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"
	toolkit_web "github.com/prometheus/exporter-toolkit/web"
	toolkit_webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/legacymanager"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/model/exemplar"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/metadata"
	"github.com/prometheus/prometheus/model/relabel"
	"github.com/prometheus/prometheus/notifier"
	_ "github.com/prometheus/prometheus/plugins" // Register plugins.
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tracing"
	"github.com/prometheus/prometheus/util/logging"
	prom_runtime "github.com/prometheus/prometheus/util/runtime"
	"github.com/prometheus/prometheus/util/strutil"
	"go.uber.org/atomic"
	"go.uber.org/automaxprocs/maxprocs"
	"gopkg.in/alecthomas/kingpin.v2"
	"k8s.io/klog"
	klogv2 "k8s.io/klog/v2"

	"github.com/kom0055/promedge/scrape"
	"github.com/kom0055/promedge/tsdb"
	"github.com/kom0055/promedge/web"
)

var (
	appName = "prometheus"

	configSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "prometheus_config_last_reload_successful",
		Help: "Whether the last configuration reload attempt was successful.",
	})
	configSuccessTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "prometheus_config_last_reload_success_timestamp_seconds",
		Help: "Timestamp of the last successful configuration reload.",
	})

	serverOnlyFlags []string
)

func init() {
	prometheus.MustRegister(version.NewCollector(strings.ReplaceAll(appName, "-", "_")))

}

// serverOnlyFlag creates server-only kingpin flag.
func serverOnlyFlag(app *kingpin.Application, name, help string) *kingpin.FlagClause {
	return app.Flag(name, fmt.Sprintf("%s Use with server mode only.", help)).
		PreAction(func(parseContext *kingpin.ParseContext) error {
			// This will be invoked only if flag is actually provided by user.
			serverOnlyFlags = append(serverOnlyFlags, "--"+name)
			return nil
		})
}

type flagConfig struct {
	configFile string

	notifier            notifier.Options
	forGracePeriod      model.Duration
	outageTolerance     model.Duration
	resendDelay         model.Duration
	web                 web.Options
	scrape              scrape.Options
	tsdb                tsdbOptions
	lookbackDelta       model.Duration
	webTimeout          model.Duration
	queryTimeout        model.Duration
	queryConcurrency    int
	queryMaxSamples     int
	RemoteFlushDeadline model.Duration

	featureList []string
	// These options are extracted from featureList
	// for ease of use.
	enableAutoGOMAXPROCS bool

	prometheusURL   string
	corsRegexString string

	promlogConfig promlog.Config
}

// setFeatureListOptions sets the corresponding options from the featureList.
func (c *flagConfig) setFeatureListOptions(logger log.Logger) error {
	for _, f := range c.featureList {
		opts := strings.Split(f, ",")
		for _, o := range opts {
			switch o {
			case "remote-write-receiver":
				c.web.EnableRemoteWriteReceiver = true
				level.Warn(logger).Log("msg", "Remote write receiver enabled via feature flag remote-write-receiver. This is DEPRECATED. Use --web.enable-remote-write-receiver.")

			case "auto-gomaxprocs":
				c.enableAutoGOMAXPROCS = true
				level.Info(logger).Log("msg", "Automatically set GOMAXPROCS to match Linux container CPU quota")
			case "no-default-scrape-port":
				c.scrape.NoDefaultPort = true
				level.Info(logger).Log("msg", "No default port will be appended to scrape targets' addresses.")
			case "":
				continue
			case "promql-at-modifier", "promql-negative-offset":
				level.Warn(logger).Log("msg", "This option for --enable-feature is now permanently enabled and therefore a no-op.", "option", o)
			default:
				level.Warn(logger).Log("msg", "Unknown option for --enable-feature", "option", o)
			}
		}
	}
	return nil
}

func main() {
	if os.Getenv("DEBUG") != "" {
		runtime.SetBlockProfileRate(20)
		runtime.SetMutexProfileFraction(20)
	}

	cfg := flagConfig{
		notifier: notifier.Options{
			Registerer: prometheus.DefaultRegisterer,
		},
		web: web.Options{
			Registerer: prometheus.DefaultRegisterer,
			Gatherer:   prometheus.DefaultGatherer,
		},
		promlogConfig: promlog.Config{},
	}

	a := kingpin.New(filepath.Base(os.Args[0]), "The Prometheus monitoring server").UsageWriter(os.Stdout)

	a.Version(version.Print(appName))

	a.HelpFlag.Short('h')

	a.Flag("config.file", "Prometheus configuration file path.").
		Default("conf/prometheus.yml").StringVar(&cfg.configFile)

	a.Flag("web.listen-address", "Address to listen on for UI, API, and telemetry.").
		Default("0.0.0.0:19001").StringVar(&cfg.web.ListenAddress)

	webConfig := toolkit_webflag.AddFlags(a)

	a.Flag("web.read-timeout",
		"Maximum duration before timing out read of the request, and closing idle connections.").
		Default("5m").SetValue(&cfg.webTimeout)

	a.Flag("web.max-connections", "Maximum number of simultaneous connections.").
		Default("512").IntVar(&cfg.web.MaxConnections)

	a.Flag("web.external-url",
		"The URL under which Prometheus is externally reachable (for example, if Prometheus is served via a reverse proxy). Used for generating relative and absolute links back to Prometheus itself. If the URL has a path portion, it will be used to prefix all HTTP endpoints served by Prometheus. If omitted, relevant URL components will be derived automatically.").
		PlaceHolder("<URL>").StringVar(&cfg.prometheusURL)

	a.Flag("web.route-prefix",
		"Prefix for the internal routes of web endpoints. Defaults to path of --web.external-url.").
		PlaceHolder("<path>").StringVar(&cfg.web.RoutePrefix)

	a.Flag("web.user-assets", "Path to static asset directory, available at /user.").
		PlaceHolder("<path>").StringVar(&cfg.web.UserAssetsPath)

	a.Flag("web.enable-lifecycle", "Enable shutdown and reload via HTTP request.").
		Default("false").BoolVar(&cfg.web.EnableLifecycle)

	a.Flag("web.enable-admin-api", "Enable API endpoints for admin control actions.").
		Default("false").BoolVar(&cfg.web.EnableAdminAPI)

	a.Flag("web.enable-remote-write-receiver", "Enable API endpoint accepting remote write requests.").
		Default("false").BoolVar(&cfg.web.EnableRemoteWriteReceiver)

	a.Flag("web.console.templates", "Path to the console template directory, available at /consoles.").
		Default("consoles").StringVar(&cfg.web.ConsoleTemplatesPath)

	a.Flag("web.console.libraries", "Path to the console library directory.").
		Default("console_libraries").StringVar(&cfg.web.ConsoleLibrariesPath)

	a.Flag("web.page-title", "Document title of Prometheus instance.").
		Default("Prometheus Time Series Collection and Processing Server").StringVar(&cfg.web.PageTitle)

	a.Flag("web.cors.origin", `Regex for CORS origin. It is fully anchored. Example: 'https?://(domain1|domain2)\.com'`).
		Default(".*").StringVar(&cfg.corsRegexString)

	a.Flag("storage.remote.flush-deadline", "How long to wait flushing sample on shutdown or config reload.").
		Default("1m").PlaceHolder("<duration>").SetValue(&cfg.RemoteFlushDeadline)

	serverOnlyFlag(a, "storage.remote.read-sample-limit", "Maximum overall number of samples to return via the remote read interface, in a single query. 0 means no limit. This limit is ignored for streamed response types.").
		Default("5e7").IntVar(&cfg.web.RemoteReadSampleLimit)

	serverOnlyFlag(a, "storage.remote.read-concurrent-limit", "Maximum number of concurrent remote read calls. 0 means no limit.").
		Default("10").IntVar(&cfg.web.RemoteReadConcurrencyLimit)

	serverOnlyFlag(a, "storage.remote.read-max-bytes-in-frame", "Maximum number of bytes in a single frame for streaming remote read response types before marshalling. Note that client might have limit on frame size as well. 1MB as recommended by protobuf by default.").
		Default("1048576").IntVar(&cfg.web.RemoteReadBytesInFrame)

	serverOnlyFlag(a, "rules.alert.for-outage-tolerance", "Max time to tolerate prometheus outage for restoring \"for\" state of alert.").
		Default("1h").SetValue(&cfg.outageTolerance)

	serverOnlyFlag(a, "rules.alert.for-grace-period", "Minimum duration between alert and restored \"for\" state. This is maintained only for alerts with configured \"for\" time greater than grace period.").
		Default("10m").SetValue(&cfg.forGracePeriod)

	serverOnlyFlag(a, "rules.alert.resend-delay", "Minimum amount of time to wait before resending an alert to Alertmanager.").
		Default("1m").SetValue(&cfg.resendDelay)

	serverOnlyFlag(a, "alertmanager.notification-queue-capacity", "The capacity of the queue for pending Alertmanager notifications.").
		Default("10000").IntVar(&cfg.notifier.QueueCapacity)

	// TODO: Remove in Prometheus 3.0.
	alertmanagerTimeout := a.Flag("alertmanager.timeout", "[DEPRECATED] This flag has no effect.").Hidden().String()

	serverOnlyFlag(a, "query.lookback-delta", "The maximum lookback duration for retrieving metrics during expression evaluations and federation.").
		Default("5m").SetValue(&cfg.lookbackDelta)

	serverOnlyFlag(a, "query.timeout", "Maximum time a query may take before being aborted.").
		Default("2m").SetValue(&cfg.queryTimeout)

	serverOnlyFlag(a, "query.max-concurrency", "Maximum number of queries executed concurrently.").
		Default("20").IntVar(&cfg.queryConcurrency)

	serverOnlyFlag(a, "query.max-samples", "Maximum number of samples a single query can load into memory. Note that queries will fail if they try to load more samples than this into memory, so this also limits the number of samples a query can return.").
		Default("50000000").IntVar(&cfg.queryMaxSamples)

	a.Flag("scrape.discovery-reload-interval", "Interval used by scrape manager to throttle target groups updates.").
		Hidden().Default("5s").SetValue(&cfg.scrape.DiscoveryReloadInterval)

	a.Flag("enable-feature", "Comma separated feature names to enable. Valid options:  exemplar-storage, expand-external-labels, memory-snapshot-on-shutdown, promql-at-modifier, promql-negative-offset, promql-per-step-stats, remote-write-receiver (DEPRECATED), extra-scrape-metrics, new-service-discovery-manager, auto-gomaxprocs, no-default-scrape-port. See https://prometheus.io/docs/prometheus/latest/feature_flags/ for more details.").
		Default("").StringsVar(&cfg.featureList)

	promlogflag.AddFlags(a, &cfg.promlogConfig)

	_, err := a.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("Error parsing commandline arguments: %w", err))
		a.Usage(os.Args[1:])
		os.Exit(2)
	}

	logger := promlog.New(&cfg.promlogConfig)

	if err := cfg.setFeatureListOptions(logger); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("Error parsing feature list: %w", err))
		os.Exit(1)
	}

	cfg.web.ExternalURL, err = computeExternalURL(cfg.prometheusURL, cfg.web.ListenAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("parse external URL %q: %w", cfg.prometheusURL, err))
		os.Exit(2)
	}

	cfg.web.CORSOrigin, err = compileCORSRegexString(cfg.corsRegexString)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Errorf("could not compile CORS regex string %q: %w", cfg.corsRegexString, err))
		os.Exit(2)
	}

	if *alertmanagerTimeout != "" {
		level.Warn(logger).Log("msg", "The flag --alertmanager.timeout has no effect and will be removed in the future.")
	}

	// Now that the validity of the config is established, set the config
	// success metrics accordingly, although the config isn't really loaded
	// yet. This will happen later (including setting these metrics again),
	// but if we don't do it now, the metrics will stay at zero until the
	// startup procedure is complete, which might take long enough to
	// trigger alerts about an invalid config.
	configSuccess.Set(1)
	configSuccessTime.SetToCurrentTime()

	cfg.web.ReadTimeout = time.Duration(cfg.webTimeout)
	// Default -web.route-prefix to path of -web.external-url.
	if cfg.web.RoutePrefix == "" {
		cfg.web.RoutePrefix = cfg.web.ExternalURL.Path
	}
	// RoutePrefix must always be at least '/'.
	cfg.web.RoutePrefix = "/" + strings.Trim(cfg.web.RoutePrefix, "/")

	noStepSubqueryInterval := &safePromQLNoStepSubqueryInterval{}
	noStepSubqueryInterval.Set(config.DefaultGlobalConfig.EvaluationInterval)

	// Above level 6, the k8s client would log bearer tokens in clear-text.
	klog.ClampLevel(6)
	klog.SetLogger(log.With(logger, "component", "k8s_client_runtime"))
	klogv2.ClampLevel(6)
	klogv2.SetLogger(log.With(logger, "component", "k8s_client_runtime"))

	modeAppName := "Prometheus Server"
	mode := "server"

	level.Info(logger).Log("msg", "Starting "+modeAppName, "mode", mode, "version", version.Info())
	if bits.UintSize < 64 {
		level.Warn(logger).Log("msg", "This Prometheus binary has not been compiled for a 64-bit architecture. Due to virtual memory constraints of 32-bit systems, it is highly recommended to switch to a 64-bit binary of Prometheus.", "GOARCH", runtime.GOARCH)
	}

	level.Info(logger).Log("build_context", version.BuildContext())
	level.Info(logger).Log("host_details", prom_runtime.Uname())
	level.Info(logger).Log("fd_limits", prom_runtime.FdLimits())
	level.Info(logger).Log("vm_limits", prom_runtime.VMLimits())

	var (
		localStorage  = &readyStorage{}
		scraper       = &readyScrapeManager{}
		fanoutStorage = storage.NewFanout(logger, localStorage)
	)

	var (
		ctxWeb, cancelWeb = context.WithCancel(context.Background())
		ctxRule           = context.Background()

		notifierManager = notifier.NewManager(&cfg.notifier, log.With(logger, "component", "notifier"))

		ctxScrape, cancelScrape = context.WithCancel(context.Background())
		ctxNotify, cancelNotify = context.WithCancel(context.Background())
		discoveryManagerScrape  discoveryManager
		discoveryManagerNotify  discoveryManager
	)

	legacymanager.RegisterMetrics()
	discoveryManagerScrape = legacymanager.NewManager(ctxScrape, log.With(logger, "component", "discovery manager scrape"), legacymanager.Name("scrape"))
	discoveryManagerNotify = legacymanager.NewManager(ctxNotify, log.With(logger, "component", "discovery manager notify"), legacymanager.Name("notify"))

	var (
		scrapeManager  = scrape.NewManager(&cfg.scrape, log.With(logger, "component", "scrape manager"), fanoutStorage)
		tracingManager = tracing.NewManager(logger)

		queryEngine *promql.Engine
		ruleManager *rules.Manager
	)

	if cfg.enableAutoGOMAXPROCS {
		l := func(format string, a ...interface{}) {
			level.Info(logger).Log("component", "automaxprocs", "msg", fmt.Sprintf(strings.TrimPrefix(format, "maxprocs: "), a...))
		}
		if _, err := maxprocs.Set(maxprocs.Logger(l)); err != nil {
			level.Warn(logger).Log("component", "automaxprocs", "msg", "Failed to set GOMAXPROCS automatically", "err", err)
		}
	}

	{
		opts := promql.EngineOpts{
			Logger:                   log.With(logger, "component", "query engine"),
			Reg:                      prometheus.DefaultRegisterer,
			MaxSamples:               cfg.queryMaxSamples,
			Timeout:                  time.Duration(cfg.queryTimeout),
			LookbackDelta:            time.Duration(cfg.lookbackDelta),
			NoStepSubqueryIntervalFn: noStepSubqueryInterval.Get,
			// EnableAtModifier and EnableNegativeOffset have to be
			// always on for regular PromQL as of Prometheus v2.33.
			EnableAtModifier:     true,
			EnableNegativeOffset: true,
		}

		queryEngine = promql.NewEngine(opts)

		ruleManager = rules.NewManager(&rules.ManagerOptions{
			Appendable:      fanoutStorage,
			Queryable:       localStorage,
			QueryFunc:       rules.EngineQueryFunc(queryEngine, fanoutStorage),
			NotifyFunc:      sendAlerts(notifierManager, cfg.web.ExternalURL.String()),
			Context:         ctxRule,
			ExternalURL:     cfg.web.ExternalURL,
			Registerer:      prometheus.DefaultRegisterer,
			Logger:          log.With(logger, "component", "rule manager"),
			OutageTolerance: time.Duration(cfg.outageTolerance),
			ForGracePeriod:  time.Duration(cfg.forGracePeriod),
			ResendDelay:     time.Duration(cfg.resendDelay),
		})
	}

	scraper.Set(scrapeManager)

	cfg.web.Context = ctxWeb
	cfg.web.LocalStorage = localStorage
	cfg.web.Storage = fanoutStorage
	cfg.web.ExemplarStorage = localStorage
	cfg.web.QueryEngine = queryEngine
	cfg.web.ScrapeManager = scrapeManager
	cfg.web.RuleManager = ruleManager
	cfg.web.Notifier = notifierManager
	cfg.web.LookbackDelta = time.Duration(cfg.lookbackDelta)
	cfg.web.AppName = modeAppName

	cfg.web.Version = &web.PrometheusVersion{
		Version:   version.Version,
		Revision:  version.Revision,
		Branch:    version.Branch,
		BuildUser: version.BuildUser,
		BuildDate: version.BuildDate,
		GoVersion: version.GoVersion,
	}

	cfg.web.Flags = map[string]string{}

	// Exclude kingpin default flags to expose only Prometheus ones.
	boilerplateFlags := kingpin.New("", "").Version("")
	for _, f := range a.Model().Flags {
		if boilerplateFlags.GetFlag(f.Name) != nil {
			continue
		}

		cfg.web.Flags[f.Name] = f.Value.String()
	}

	// Depends on cfg.web.ScrapeManager so needs to be after cfg.web.ScrapeManager = scrapeManager.
	webHandler := web.New(log.With(logger, "component", "web"), &cfg.web)

	// Monitor outgoing connections on default transport with conntrack.
	http.DefaultTransport.(*http.Transport).DialContext = conntrack.NewDialContextFunc(
		conntrack.DialWithTracing(),
	)

	// This is passed to ruleManager.Update().
	externalURL := cfg.web.ExternalURL.String()

	reloaders := []reloader{
		{
			name:     "db_storage",
			reloader: localStorage.ApplyConfig,
		}, {
			name:     "web_handler",
			reloader: webHandler.ApplyConfig,
		}, {
			name: "query_engine",
			reloader: func(cfg *config.Config) error {

				if cfg.GlobalConfig.QueryLogFile == "" {
					queryEngine.SetQueryLogger(nil)
					return nil
				}

				l, err := logging.NewJSONFileLogger(cfg.GlobalConfig.QueryLogFile)
				if err != nil {
					return err
				}
				queryEngine.SetQueryLogger(l)
				return nil
			},
		}, {
			// The Scrape and notifier managers need to reload before the Discovery manager as
			// they need to read the most updated config when receiving the new targets list.
			name:     "scrape",
			reloader: scrapeManager.ApplyConfig,
		}, {
			name: "scrape_sd",
			reloader: func(cfg *config.Config) error {
				c := make(map[string]discovery.Configs)
				for _, v := range cfg.ScrapeConfigs {
					c[v.JobName] = v.ServiceDiscoveryConfigs
				}
				return discoveryManagerScrape.ApplyConfig(c)
			},
		}, {
			name:     "notify",
			reloader: notifierManager.ApplyConfig,
		}, {
			name: "notify_sd",
			reloader: func(cfg *config.Config) error {
				c := make(map[string]discovery.Configs)
				for k, v := range cfg.AlertingConfig.AlertmanagerConfigs.ToMap() {
					c[k] = v.ServiceDiscoveryConfigs
				}
				return discoveryManagerNotify.ApplyConfig(c)
			},
		}, {
			name: "rules",
			reloader: func(cfg *config.Config) error {

				// Get all rule files matching the configuration paths.
				var files []string
				for _, pat := range cfg.RuleFiles {
					fs, err := filepath.Glob(pat)
					if err != nil {
						// The only error can be a bad pattern.
						return fmt.Errorf("error retrieving rule files for %s: %w", pat, err)
					}
					files = append(files, fs...)
				}
				return ruleManager.Update(
					time.Duration(cfg.GlobalConfig.EvaluationInterval),
					files,
					cfg.GlobalConfig.ExternalLabels,
					externalURL,
					nil,
				)
			},
		}, {
			name:     "tracing",
			reloader: tracingManager.ApplyConfig,
		},
	}

	prometheus.MustRegister(configSuccess)
	prometheus.MustRegister(configSuccessTime)

	// Start all components while we wait for TSDB to open but only load
	// initial config and mark ourselves as ready after it completed.
	dbOpen := make(chan struct{})

	// sync.Once is used to make sure we can close the channel at different execution stages(SIGTERM or when the config is loaded).
	type closeOnce struct {
		C     chan struct{}
		once  sync.Once
		Close func()
	}
	// Wait until the server is ready to handle reloading.
	reloadReady := &closeOnce{
		C: make(chan struct{}),
	}
	reloadReady.Close = func() {
		reloadReady.once.Do(func() {
			close(reloadReady.C)
		})
	}

	listener, err := webHandler.Listener()
	if err != nil {
		level.Error(logger).Log("msg", "Unable to start web listener", "err", err)
		os.Exit(1)
	}

	err = toolkit_web.Validate(*webConfig)
	if err != nil {
		level.Error(logger).Log("msg", "Unable to validate web configuration file", "err", err)
		os.Exit(1)
	}

	var g run.Group
	{
		// Termination handler.
		term := make(chan os.Signal, 1)
		signal.Notify(term, os.Interrupt, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				// Don't forget to release the reloadReady channel so that waiting blocks can exit normally.
				select {
				case <-term:
					level.Warn(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
					reloadReady.Close()
				case <-webHandler.Quit():
					level.Warn(logger).Log("msg", "Received termination request via web service, exiting gracefully...")
				case <-cancel:
					reloadReady.Close()
				}
				return nil
			},
			func(err error) {
				close(cancel)
				webHandler.SetReady(false)
			},
		)
	}
	{
		// Scrape discovery manager.
		g.Add(
			func() error {
				err := discoveryManagerScrape.Run()
				level.Info(logger).Log("msg", "Scrape discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping scrape discovery manager...")
				cancelScrape()
			},
		)
	}
	{
		// Notify discovery manager.
		g.Add(
			func() error {
				err := discoveryManagerNotify.Run()
				level.Info(logger).Log("msg", "Notify discovery manager stopped")
				return err
			},
			func(err error) {
				level.Info(logger).Log("msg", "Stopping notify discovery manager...")
				cancelNotify()
			},
		)
	}
	{
		// Rule manager.
		g.Add(
			func() error {
				<-reloadReady.C
				ruleManager.Run()
				return nil
			},
			func(err error) {
				ruleManager.Stop()
			},
		)
	}
	{
		// Scrape manager.
		g.Add(
			func() error {
				// When the scrape manager receives a new targets list
				// it needs to read a valid config for each job.
				// It depends on the config being in sync with the discovery manager so
				// we wait until the config is fully loaded.
				<-reloadReady.C

				err := scrapeManager.Run(discoveryManagerScrape.SyncCh())
				level.Info(logger).Log("msg", "Scrape manager stopped")
				return err
			},
			func(err error) {
				// Scrape manager needs to be stopped before closing the local TSDB
				// so that it doesn't try to write samples to a closed storage.
				// We should also wait for rule manager to be fully stopped to ensure
				// we don't trigger any false positive alerts for rules using absent().
				level.Info(logger).Log("msg", "Stopping scrape manager...")
				scrapeManager.Stop()
			},
		)
	}
	{
		// Tracing manager.
		g.Add(
			func() error {
				<-reloadReady.C
				tracingManager.Run()
				return nil
			},
			func(err error) {
				tracingManager.Stop()
			},
		)
	}
	{
		// Reload handler.

		// Make sure that sighup handler is registered with a redirect to the channel before the potentially
		// long and synchronous tsdb init.
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		cancel := make(chan struct{})
		g.Add(
			func() error {
				<-reloadReady.C

				for {
					select {
					case <-hup:
						if err := reloadConfig(cfg.configFile, logger, noStepSubqueryInterval, reloaders...); err != nil {
							level.Error(logger).Log("msg", "Error reloading config", "err", err)
						}
					case rc := <-webHandler.Reload():
						if err := reloadConfig(cfg.configFile, logger, noStepSubqueryInterval, reloaders...); err != nil {
							level.Error(logger).Log("msg", "Error reloading config", "err", err)
							rc <- err
						} else {
							rc <- nil
						}
					case <-cancel:
						return nil
					}
				}
			},
			func(err error) {
				// Wait for any in-progress reloads to complete to avoid
				// reloading things after they have been shutdown.
				cancel <- struct{}{}
			},
		)
	}
	{
		// Initial configuration loading.
		cancel := make(chan struct{})
		g.Add(
			func() error {
				select {
				case <-dbOpen:
				// In case a shutdown is initiated before the dbOpen is released
				case <-cancel:
					reloadReady.Close()
					return nil
				}

				if err := reloadConfig(cfg.configFile, logger, noStepSubqueryInterval, reloaders...); err != nil {
					return fmt.Errorf("error loading config from %q: %w", cfg.configFile, err)
				}

				reloadReady.Close()

				webHandler.SetReady(true)
				level.Info(logger).Log("msg", "Server is ready to receive web requests.")
				<-cancel
				return nil
			},
			func(err error) {
				close(cancel)
			},
		)
	}
	{
		// TSDB.
		opts := cfg.tsdb.ToTSDBOptions()
		cancel := make(chan struct{})
		g.Add(
			func() error {
				level.Info(logger).Log("msg", "Starting TSDB ...")

				db, err := openDBWithMetrics(logger, prometheus.DefaultRegisterer, &opts)
				if err != nil {
					return fmt.Errorf("opening storage failed: %w", err)
				}

				level.Info(logger).Log("msg", "TSDB started")

				localStorage.Set(db, 0)
				close(dbOpen)
				<-cancel
				return nil
			},
			func(err error) {
				if err := fanoutStorage.Close(); err != nil {
					level.Error(logger).Log("msg", "Error stopping storage", "err", err)
				}
				close(cancel)
			},
		)
	}

	{
		// Web handler.
		g.Add(
			func() error {
				if err := webHandler.Run(ctxWeb, listener, *webConfig); err != nil {
					return fmt.Errorf("error starting web server: %w", err)
				}
				return nil
			},
			func(err error) {
				cancelWeb()
			},
		)
	}
	{
		// Notifier.

		// Calling notifier.Stop() before ruleManager.Stop() will cause a panic if the ruleManager isn't running,
		// so keep this interrupt after the ruleManager.Stop().
		g.Add(
			func() error {
				// When the notifier manager receives a new targets list
				// it needs to read a valid config for each job.
				// It depends on the config being in sync with the discovery manager
				// so we wait until the config is fully loaded.
				<-reloadReady.C

				notifierManager.Run(discoveryManagerNotify.SyncCh())
				level.Info(logger).Log("msg", "Notifier manager stopped")
				return nil
			},
			func(err error) {
				notifierManager.Stop()
			},
		)
	}
	if err := g.Run(); err != nil {
		level.Error(logger).Log("err", err)
		os.Exit(1)
	}
	level.Info(logger).Log("msg", "See you next time!")
}

func openDBWithMetrics(logger log.Logger, reg prometheus.Registerer, opts *tsdb.Options) (*tsdb.DB, error) {
	db, err := tsdb.Open(

		log.With(logger, "component", "tsdb"),
		reg,
		opts,
	)
	if err != nil {
		return nil, err
	}

	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "prometheus_tsdb_lowest_timestamp_seconds",
			Help: "Lowest timestamp value stored in the database.",
		}, func() float64 {
			return float64(db.Head().MinTime() / 1000)
		}), prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "prometheus_tsdb_head_min_time_seconds",
			Help: "Minimum time bound of the head block.",
		}, func() float64 { return float64(db.Head().MinTime() / 1000) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "prometheus_tsdb_head_max_time_seconds",
			Help: "Maximum timestamp of the head block.",
		}, func() float64 { return float64(db.Head().MaxTime() / 1000) }),
	)

	return db, nil
}

type safePromQLNoStepSubqueryInterval struct {
	value atomic.Int64
}

func durationToInt64Millis(d time.Duration) int64 {
	return int64(d / time.Millisecond)
}

func (i *safePromQLNoStepSubqueryInterval) Set(ev model.Duration) {
	i.value.Store(durationToInt64Millis(time.Duration(ev)))
}

func (i *safePromQLNoStepSubqueryInterval) Get(int64) int64 {
	return i.value.Load()
}

type reloader struct {
	name     string
	reloader func(*config.Config) error
}

func reloadConfig(filename string, logger log.Logger, noStepSuqueryInterval *safePromQLNoStepSubqueryInterval, rls ...reloader) (err error) {
	start := time.Now()
	timings := []interface{}{}
	level.Info(logger).Log("msg", "Loading configuration file", "filename", filename)

	defer func() {
		if err == nil {
			configSuccess.Set(1)
			configSuccessTime.SetToCurrentTime()
		} else {
			configSuccess.Set(0)
		}
	}()

	conf, err := config.LoadFile(filename, false, false, logger)
	if err != nil {
		return fmt.Errorf("couldn't load configuration (--config.file=%q): %w", filename, err)
	}

	failed := false
	for _, rl := range rls {
		rstart := time.Now()
		if err := rl.reloader(conf); err != nil {
			level.Error(logger).Log("msg", "Failed to apply configuration", "err", err)
			failed = true
		}
		timings = append(timings, rl.name, time.Since(rstart))
	}
	if failed {
		return fmt.Errorf("one or more errors occurred while applying the new configuration (--config.file=%q)", filename)
	}

	noStepSuqueryInterval.Set(conf.GlobalConfig.EvaluationInterval)
	l := []interface{}{"msg", "Completed loading of configuration file", "filename", filename, "totalDuration", time.Since(start)}
	level.Info(logger).Log(append(l, timings...)...)
	return nil
}

func startsOrEndsWithQuote(s string) bool {
	return strings.HasPrefix(s, "\"") || strings.HasPrefix(s, "'") ||
		strings.HasSuffix(s, "\"") || strings.HasSuffix(s, "'")
}

// compileCORSRegexString compiles given string and adds anchors
func compileCORSRegexString(s string) (*regexp.Regexp, error) {
	r, err := relabel.NewRegexp(s)
	if err != nil {
		return nil, err
	}
	return r.Regexp, nil
}

// computeExternalURL computes a sanitized external URL from a raw input. It infers unset
// URL parts from the OS and the given listen address.
func computeExternalURL(u, listenAddr string) (*url.URL, error) {
	if u == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return nil, err
		}
		_, port, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return nil, err
		}
		u = fmt.Sprintf("http://%s:%s/", hostname, port)
	}

	if startsOrEndsWithQuote(u) {
		return nil, errors.New("URL must not begin or end with quotes")
	}

	eu, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	ppref := strings.TrimRight(eu.Path, "/")
	if ppref != "" && !strings.HasPrefix(ppref, "/") {
		ppref = "/" + ppref
	}
	eu.Path = ppref

	return eu, nil
}

type sender interface {
	Send(alerts ...*notifier.Alert)
}

// sendAlerts implements the rules.NotifyFunc for a Notifier.
func sendAlerts(s sender, externalURL string) rules.NotifyFunc {
	return func(ctx context.Context, expr string, alerts ...*rules.Alert) {
		var res []*notifier.Alert

		for _, alert := range alerts {
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt,
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt
			} else {
				a.EndsAt = alert.ValidUntil
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			s.Send(res...)
		}
	}
}

// readyStorage implements the Storage interface while allowing to set the actual
// storage at a later point in time.
type readyStorage struct {
	mtx             sync.RWMutex
	db              storage.Storage
	startTimeMargin int64
}

func (s *readyStorage) ApplyConfig(conf *config.Config) error {
	db := s.get()
	if db, ok := db.(*tsdb.DB); ok {
		return db.ApplyConfig(conf)
	}
	return nil
}

// Set the storage.
func (s *readyStorage) Set(db storage.Storage, startTimeMargin int64) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	s.db = db
	s.startTimeMargin = startTimeMargin
}

func (s *readyStorage) get() storage.Storage {
	s.mtx.RLock()
	x := s.db
	s.mtx.RUnlock()
	return x
}

// StartTime implements the Storage interface.
func (s *readyStorage) StartTime() (int64, error) {
	if x := s.get(); x != nil {
		switch db := x.(type) {
		case *tsdb.DB:
			startTime := time.Now().Unix() * 1000
			// Add a safety margin as it may take a few minutes for everything to spin up.
			return startTime + s.startTimeMargin, nil
		default:
			panic(fmt.Sprintf("unknown storage type %T", db))
		}
	}

	return math.MaxInt64, tsdb.ErrNotReady
}

// Querier implements the Storage interface.
func (s *readyStorage) Querier(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
	if x := s.get(); x != nil {
		return x.Querier(ctx, mint, maxt)
	}
	return nil, tsdb.ErrNotReady
}

// ChunkQuerier implements the Storage interface.
func (s *readyStorage) ChunkQuerier(ctx context.Context, mint, maxt int64) (storage.ChunkQuerier, error) {
	if x := s.get(); x != nil {
		return x.ChunkQuerier(ctx, mint, maxt)
	}
	return nil, tsdb.ErrNotReady
}

func (s *readyStorage) ExemplarQuerier(ctx context.Context) (storage.ExemplarQuerier, error) {
	if x := s.get(); x != nil {
		switch db := x.(type) {
		case *tsdb.DB:
			return db.ExemplarQuerier(ctx)

		default:
			panic(fmt.Sprintf("unknown storage type %T", db))
		}
	}
	return nil, tsdb.ErrNotReady
}

// Appender implements the Storage interface.
func (s *readyStorage) Appender(ctx context.Context) storage.Appender {
	if x := s.get(); x != nil {
		return x.Appender(ctx)
	}
	return notReadyAppender{}
}

type notReadyAppender struct{}

func (n notReadyAppender) Append(ref storage.SeriesRef, l labels.Labels, t int64, v float64) (storage.SeriesRef, error) {
	return 0, tsdb.ErrNotReady
}

func (n notReadyAppender) AppendExemplar(ref storage.SeriesRef, l labels.Labels, e exemplar.Exemplar) (storage.SeriesRef, error) {
	return 0, tsdb.ErrNotReady
}

func (n notReadyAppender) UpdateMetadata(ref storage.SeriesRef, l labels.Labels, m metadata.Metadata) (storage.SeriesRef, error) {
	return 0, tsdb.ErrNotReady
}

func (n notReadyAppender) Commit() error { return tsdb.ErrNotReady }

func (n notReadyAppender) Rollback() error { return tsdb.ErrNotReady }

// Close implements the Storage interface.
func (s *readyStorage) Close() error {
	if x := s.get(); x != nil {
		return x.Close()
	}
	return nil
}

// CleanTombstones implements the api_v1.TSDBAdminStats and api_v2.TSDBAdmin interfaces.
func (s *readyStorage) CleanTombstones() error {
	if x := s.get(); x != nil {
		switch db := x.(type) {
		case *tsdb.DB:
			return db.CleanTombstones()
		default:
			panic(fmt.Sprintf("unknown storage type %T", db))
		}
	}
	return tsdb.ErrNotReady
}

// Delete implements the api_v1.TSDBAdminStats and api_v2.TSDBAdmin interfaces.
func (s *readyStorage) Delete(mint, maxt int64, ms ...*labels.Matcher) error {
	if x := s.get(); x != nil {
		switch db := x.(type) {
		case *tsdb.DB:
			return db.Delete(mint, maxt, ms...)

		default:
			panic(fmt.Sprintf("unknown storage type %T", db))
		}
	}
	return tsdb.ErrNotReady
}

// Stats implements the api_v1.TSDBAdminStats interface.
func (s *readyStorage) Stats(statsByLabelName string) (*tsdb.Stats, error) {
	if x := s.get(); x != nil {
		switch db := x.(type) {
		case *tsdb.DB:
			return db.Head().Stats(statsByLabelName), nil

		default:
			panic(fmt.Sprintf("unknown storage type %T", db))
		}
	}
	return nil, tsdb.ErrNotReady
}

// ErrNotReady is returned if the underlying scrape manager is not ready yet.
var ErrNotReady = errors.New("Scrape manager not ready")

// ReadyScrapeManager allows a scrape manager to be retrieved. Even if it's set at a later point in time.
type readyScrapeManager struct {
	mtx sync.RWMutex
	m   *scrape.Manager
}

// Set the scrape manager.
func (rm *readyScrapeManager) Set(m *scrape.Manager) {
	rm.mtx.Lock()
	defer rm.mtx.Unlock()

	rm.m = m
}

// Get the scrape manager. If is not ready, return an error.
func (rm *readyScrapeManager) Get() (*scrape.Manager, error) {
	rm.mtx.RLock()
	defer rm.mtx.RUnlock()

	if rm.m != nil {
		return rm.m, nil
	}

	return nil, ErrNotReady
}

// tsdbOptions is tsdb.Option version with defined units.
// This is required as tsdb.Option fields are unit agnostic (time).
type tsdbOptions struct {
	MaxExemplars int64
}

func (opts tsdbOptions) ToTSDBOptions() tsdb.Options {
	return tsdb.Options{
		MaxExemplars: opts.MaxExemplars,
	}
}

// discoveryManager interfaces the discovery manager. This is used to keep using
// the manager that restarts SD's on reload for a few releases until we feel
// the new manager can be enabled for all users.
type discoveryManager interface {
	ApplyConfig(cfg map[string]discovery.Configs) error
	Run() error
	SyncCh() <-chan map[string][]*targetgroup.Group
}
