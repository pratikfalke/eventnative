package main

import (
	"bytes"
	"context"
	"flag"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/jitsucom/eventnative/appconfig"
	"github.com/jitsucom/eventnative/appstatus"
	"github.com/jitsucom/eventnative/caching"
	"github.com/jitsucom/eventnative/cluster"
	"github.com/jitsucom/eventnative/counters"
	"github.com/jitsucom/eventnative/destinations"
	"github.com/jitsucom/eventnative/enrichment"
	"github.com/jitsucom/eventnative/events"
	"github.com/jitsucom/eventnative/fallback"
	"github.com/jitsucom/eventnative/handlers"
	"github.com/jitsucom/eventnative/logfiles"
	"github.com/jitsucom/eventnative/logging"
	"github.com/jitsucom/eventnative/meta"
	"github.com/jitsucom/eventnative/metrics"
	"github.com/jitsucom/eventnative/middleware"
	"github.com/jitsucom/eventnative/notifications"
	"github.com/jitsucom/eventnative/safego"
	"github.com/jitsucom/eventnative/sources"
	"github.com/jitsucom/eventnative/storages"
	"github.com/jitsucom/eventnative/synchronization"
	"github.com/jitsucom/eventnative/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/viper"
)

//some inner parameters
const (
	//incoming.tok=$token-$timestamp.log
	uploaderFileMask   = "incoming.tok=*-20*.log"
	uploaderLoadEveryS = 60

	destinationsKey = "destinations"
	sourcesKey      = "sources"
)

var (
	configFilePath   = flag.String("cfg", "", "config file path")
	containerizedRun = flag.Bool("cr", false, "containerised run marker")

	//ldflags
	commit  string
	tag     string
	builtAt string
)

func readInViperConfig() error {
	flag.Parse()
	viper.AutomaticEnv()
	//support OS env variables as lower case and dot divided variables e.g. SERVER_PORT as server.port
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	//custom config
	viper.SetConfigFile(*configFilePath)
	if err := viper.ReadInConfig(); err != nil {
		//failfast for running service from source (not containerised) and with wrong config
		if viper.ConfigFileUsed() != "" && !*containerizedRun {
			return err
		} else {
			logging.ConfigWarn = "! Custom eventnative.yaml wasn't provided\n                            " +
				"! EventNative will start, however it will be mostly useless\n                            " +
				"! Please make a custom config file, you can generated a config with https://app.jitsu.com.\n                            " +
				"! Configuration documentation: https://docs.eventnative.org/configuration-1/configuration\n                            " +
				"! Add config with `-cfg eventnative.yaml` parameter or put eventnative.yaml to <config_dir> and add mapping\n                            " +
				"! -v <config_dir>/:/home/eventnative/app/res/ if you're using official Docker image"
		}
	}
	return nil
}

//go:generate easyjson -all useragent/resolver.go telemetry/models.go
func main() {
	//Setup seed for globalRand
	rand.Seed(time.Now().Unix())

	//Setup handlers binding for json parsing numbers into json.Number (not only in float64)
	binding.EnableDecoderUseNumber = true

	//Setup default timezone for time.Now() calls
	time.Local = time.UTC

	if err := readInViperConfig(); err != nil {
		logging.Fatal("Error while reading application config: ", err)
	}

	appconfig.Version = strings.Split(tag, "-")[0]

	if err := appconfig.Init(); err != nil {
		logging.Fatal(err)
	}

	enrichment.InitDefault()

	safego.GlobalRecoverHandler = func(value interface{}) {
		logging.Error("panic")
		logging.Error(value)
		logging.Error(string(debug.Stack()))
		notifications.SystemErrorf("Panic:\n%s\n%s", value, string(debug.Stack()))
	}

	telemetry.Init(commit, tag, builtAt, viper.GetBool("server.telemetry.disabled.usage"))
	metrics.Init(viper.GetBool("server.metrics.prometheus.enabled"))

	slackNotificationsWebHook := viper.GetString("notifications.slack.url")
	if slackNotificationsWebHook != "" {
		notifications.Init(notifications.ServiceName, slackNotificationsWebHook, appconfig.Instance.ServerName, logging.Errorf)
	}

	//listen to shutdown signal to free up all resources
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL, syscall.SIGHUP)
	go func() {
		<-c
		logging.Info("* Service is shutting down.. *")
		telemetry.ServerStop()
		appstatus.Instance.Idle = true
		cancel()
		appconfig.Instance.Close()
		telemetry.Flush()
		notifications.Close()
		time.Sleep(3 * time.Second)
		telemetry.Close()
		os.Exit(0)
	}()

	//Get logger configuration
	logEventPath := viper.GetString("log.path")
	//check if log.path is writable
	if !logging.IsDirWritable(logEventPath) {
		logging.Fatal("log.path:", logEventPath, "must be writable! Since EventNative docker user and owner of mounted dir are different: Please use 'chmod 777 your_mount_dir'")
	}
	logRotationMin := viper.GetInt64("log.rotation_min")

	loggerFactory := logging.NewFactory(logEventPath, logRotationMin, viper.GetBool("log.show_in_server"), appconfig.Instance.QueryLogsWriter)

	//synchronization service
	syncService, err := synchronization.NewService(
		ctx,
		appconfig.Instance.ServerName,
		viper.GetString("synchronization_service.type"),
		viper.GetString("synchronization_service.endpoint"),
		viper.GetUint("synchronization_service.connection_timeout_seconds"))
	if err != nil {
		logging.Fatal("Failed to initiate synchronization service", err)
	}

	// ** Destinations **

	//destinations config
	destinationsViper := viper.Sub(destinationsKey)
	destinationsStr := viper.GetString(destinationsKey)

	//override with config from os env
	destinationsJsonConfig := viper.GetString("destinations_json")
	if destinationsJsonConfig != "" && destinationsJsonConfig != "{}" {
		envJsonViper := viper.New()
		envJsonViper.SetConfigType("json")
		if err := envJsonViper.ReadConfig(bytes.NewBufferString(destinationsJsonConfig)); err != nil {
			logging.Error("Error reading/parsing json config from DESTINATIONS_JSON", err)
		} else {
			destinationsViper = envJsonViper.Sub(destinationsKey)
			destinationsStr = envJsonViper.GetString(destinationsKey)
		}
	}

	//meta storage config
	metaStorageViper := viper.Sub("meta.storage")

	//override with config from os env
	metaStorageJsonConfig := viper.GetString("meta_storage_json")
	if metaStorageJsonConfig != "" && metaStorageJsonConfig != "{}" {
		envJsonViper := viper.New()
		envJsonViper.SetConfigType("json")
		if err := envJsonViper.ReadConfig(bytes.NewBufferString(metaStorageJsonConfig)); err != nil {
			logging.Error("Error reading/parsing json config from META_STORAGE_JSON", err)
		} else {
			metaStorageViper = envJsonViper.Sub("meta_storage")
		}
	}

	//meta storage
	metaStorage, err := meta.NewStorage(metaStorageViper)
	if err != nil {
		logging.Fatalf("Error initializing meta storage: %v", err)
	}
	//close after all for saving last task statuses
	defer metaStorage.Close()

	//events counters
	counters.InitEvents(metaStorage)

	//events cache
	eventsCacheSize := viper.GetInt("server.cache.events.size")
	eventsCache := caching.NewEventsCache(metaStorage, eventsCacheSize)
	appconfig.Instance.ScheduleClosing(eventsCache)

	//Deprecated
	inMemoryEventsCache := events.NewCache(eventsCacheSize)
	appconfig.Instance.ScheduleClosing(inMemoryEventsCache)

	//Create event destinations
	destinationsService, err := destinations.NewService(ctx, destinationsViper, destinationsStr, logEventPath, syncService, eventsCache, loggerFactory, storages.Create)
	if err != nil {
		logging.Fatal(err)
	}
	appconfig.Instance.ScheduleClosing(destinationsService)

	// ** Sources **

	//sources config
	sourcesViper := viper.Sub(sourcesKey)
	sourcesSrc := viper.GetString(sourcesKey)

	//override with config from os env
	sourcesJsonConfig := viper.GetString("sources_json")
	if sourcesJsonConfig != "" && sourcesJsonConfig != "{}" {
		envJsonViper := viper.New()
		envJsonViper.SetConfigType("json")
		if err := envJsonViper.ReadConfig(bytes.NewBufferString(sourcesJsonConfig)); err != nil {
			logging.Error("Error reading/parsing json config from SOURCES_JSON", err)
		} else {
			sourcesViper = envJsonViper.Sub(sourcesKey)
			sourcesSrc = envJsonViper.GetString(sourcesKey)
		}
	}

	//sources sync tasks pool size
	poolSize := viper.GetInt("server.sync_tasks.pool.size")

	//Create sources
	sourceService, err := sources.NewService(ctx, sourcesViper, sourcesSrc, destinationsService, metaStorage, syncService, poolSize)
	if err != nil {
		logging.Fatal(err)
	}
	appconfig.Instance.ScheduleClosing(sourceService)

	//Uploader must read event logger directory
	uploader, err := logfiles.NewUploader(logEventPath, uploaderFileMask, uploaderLoadEveryS, destinationsService)
	if err != nil {
		logging.Fatal("Error while creating file uploader", err)
	}
	uploader.Start()

	adminToken := viper.GetString("server.admin_token")

	fallbackService, err := fallback.NewService(logEventPath, destinationsService)
	if err != nil {
		logging.Fatal("Error creating fallback service:", err)
	}

	//version reminder banner in logs
	if tag != "" && !viper.GetBool("server.disable_version_reminder") {
		vn := appconfig.NewVersionReminder(ctx)
		vn.Start()
		appconfig.Instance.ScheduleClosing(vn)
	}

	router := SetupRouter(destinationsService, adminToken, syncService, eventsCache, inMemoryEventsCache, sourceService, fallbackService)

	telemetry.ServerStart()
	notifications.ServerStart()
	logging.Info("Started server: " + appconfig.Instance.Authority)
	server := &http.Server{
		Addr:              appconfig.Instance.Authority,
		Handler:           middleware.Cors(router, appconfig.Instance.AuthorizationService.GetClientOrigins),
		ReadTimeout:       time.Second * 60,
		ReadHeaderTimeout: time.Second * 60,
		IdleTimeout:       time.Second * 65,
	}
	logging.Fatal(server.ListenAndServe())
}

func SetupRouter(destinations *destinations.Service, adminToken string, clusterManager cluster.Manager,
	eventsCache *caching.EventsCache, inMemoryEventsCache *events.Cache, sources *sources.Service, fallbackService *fallback.Service) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	router := gin.New() //gin.Default()
	router.Use(gin.Recovery())

	router.GET("/", handlers.NewRedirectHandler("/p/welcome.html").Handler)
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	publicUrl := viper.GetString("server.public_url")

	htmlHandler := handlers.NewPageHandler(viper.GetString("server.static_files_dir"), publicUrl, viper.GetBool("server.disable_welcome_page"))
	router.GET("/p/:filename", htmlHandler.Handler)

	staticHandler := handlers.NewStaticHandler(viper.GetString("server.static_files_dir"), publicUrl)
	router.GET("/s/:filename", staticHandler.Handler)
	router.GET("/t/:filename", staticHandler.Handler)

	jsEventHandler := handlers.NewEventHandler(destinations, events.NewJsPreprocessor(), eventsCache, inMemoryEventsCache)
	apiEventHandler := handlers.NewEventHandler(destinations, events.NewApiPreprocessor(), eventsCache, inMemoryEventsCache)

	sourcesHandler := handlers.NewSourcesHandler(sources)
	fallbackHandler := handlers.NewFallbackHandler(fallbackService)

	adminTokenMiddleware := middleware.AdminToken{Token: adminToken}
	apiV1 := router.Group("/api/v1")
	{
		apiV1.POST("/event", middleware.TokenFuncAuth(jsEventHandler.PostHandler, appconfig.Instance.AuthorizationService.GetClientOrigins, ""))
		apiV1.POST("/s2s/event", middleware.TokenTwoFuncAuth(apiEventHandler.PostHandler, appconfig.Instance.AuthorizationService.GetServerOrigins, appconfig.Instance.AuthorizationService.GetClientOrigins, "The token isn't a server token. Please use s2s integration token"))

		apiV1.POST("/destinations/test", adminTokenMiddleware.AdminAuth(handlers.DestinationsHandler, middleware.AdminTokenErr))
		apiV1.POST("/sources/:id/sync", adminTokenMiddleware.AdminAuth(sourcesHandler.SyncHandler, middleware.AdminTokenErr))
		apiV1.GET("/sources/:id/status", adminTokenMiddleware.AdminAuth(sourcesHandler.StatusHandler, middleware.AdminTokenErr))

		apiV1.GET("/cluster", adminTokenMiddleware.AdminAuth(handlers.NewClusterHandler(clusterManager).Handler, middleware.AdminTokenErr))
		apiV1.GET("/cache/events", adminTokenMiddleware.AdminAuth(jsEventHandler.OldGetHandler, middleware.AdminTokenErr))
		apiV1.GET("/events/cache", adminTokenMiddleware.AdminAuth(jsEventHandler.GetHandler, middleware.AdminTokenErr))

		apiV1.GET("/fallback", adminTokenMiddleware.AdminAuth(fallbackHandler.GetHandler, middleware.AdminTokenErr))
		apiV1.POST("/fallback/replay", adminTokenMiddleware.AdminAuth(fallbackHandler.ReplayHandler, middleware.AdminTokenErr))
	}

	router.POST("/api.:ignored", middleware.TokenFuncAuth(jsEventHandler.PostHandler, appconfig.Instance.AuthorizationService.GetClientOrigins, ""))

	if metrics.Enabled {
		router.GET("/prometheus", middleware.TokenAuth(gin.WrapH(promhttp.Handler()), adminToken))
	}

	return router
}
