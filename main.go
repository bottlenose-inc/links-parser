package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/publicsuffix"
	"gopkg.in/redis.v3"

	irukaConfig "github.com/bottlenose-inc/go-common-tools/config" // go-common-tools config loader
	irukaLogger "github.com/bottlenose-inc/go-common-tools/logger" // go-common-tools bunyan-style logger package
	"github.com/bottlenose-inc/go-common-tools/metrics"            // go-common-tools Prometheus metrics package
	rj "github.com/bottlenose-inc/rapidjson"                       // faster json handling
	"github.com/gorilla/mux"                                       // URL router and dispatcher
	"github.com/prometheus/client_golang/prometheus"               // Prometheus client library
)

const (
	SERVICE_NAME     = "links"
	BODY_LIMIT_BYTES = 1024 * 1024 // trunc incoming request to 1 MB
	OBJECTS_PER_LOG  = 1000

	USAGE_STRING = `{
  "result": {
    "name": "links",
    "description": "Fetches resources identified by URLs",
    "in": {
      "url": {"type": "string"}
    },
    "out": {
      "link": {
        "type": "object",
        "fields": {
          "cacheHit": {
            "type": "boolean"
          },
          "description": {
            "type": "string"
          },
          "error": {
            "type": "string"
          },
          "fetchDuration": {
            "type": "number"
          },
          "favicon": {
            "type" : "string"
          },
          "id": {
            "type": "string"
          },
          "imageUrl": {
            "type": "string"
          },
          "originalUrl": {
            "type": "string"
          },
          "providerKeywords": {
            "type": "string"
          },
          "parseDuration": {
            "type": "number"
          },
          "providerName": {
            "type": "string"
          },
          "providerUrl": {
            "type": "string"
          },
          "title": {
            "type": "string"
          },
          "type": {
            "type": "string"
          },
          "url": {
            "type": "string"
          },
          "rootUrl": {
            "type": "string"
          }
        }
      }
    }
  }
}`
)

var (
	numProcessed = 0
	startTime    = time.Now()

	redisClient *redis.Client

	httpClient        http.Client
	RedirectAttempted = errors.New("redirect")

	totalRequestsCounter       prometheus.Counter
	invalidRequestsCounter     prometheus.Counter
	objsProcessedCounterVector *prometheus.CounterVec
	requestDurationCounter     prometheus.Counter
	requestDuration            prometheus.Histogram
	errorsCounter              prometheus.Counter
	cacheHitCounterVector      *prometheus.CounterVec

	notFound []byte
	usage    []byte
	logger   *irukaLogger.Logger

	ProviderNames = make(map[string]string)

	cfg = &Config{}
)

func main() {
	// Initialize logger
	var err error
	logger, err = irukaLogger.NewLogger(SERVICE_NAME)
	if err != nil {
		log.Fatal("Unable to initialize go-iruka logger, exiting: " + err.Error())
	}

	// load config
	if err = InitCfg(); err != nil {
		logger.Fatal("Error loading config: " + err.Error())
		os.Exit(1)
	}

	// load provider names file
	pnFile, err := ioutil.ReadFile(cfg.ProviderNamesFile)
	if err != nil {
		logger.Fatal("File error: " + err.Error())
		os.Exit(1)
	}
	err = json.Unmarshal(pnFile, &ProviderNames)
	if err != nil {
		logger.Fatal("Error loading provider names data: " + err.Error())
		os.Exit(1)
	}

	// Start Prometheus metrics server
	go metrics.StartPrometheusMetricsServer(SERVICE_NAME, logger, cfg.PrometheusPort)

	// Initialize Prometheus Metrics
	InitMetrics()

	// init redis
	logFile, err := os.OpenFile("log/redis-client.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		logger.Error("Error creating Redis client logfile: " + err.Error())
	} else {
		redis.Logger = log.New(logFile, "", log.LstdFlags)
	}
	redisClient = redis.NewClient(&redis.Options{
		Addr:     cfg.RedisHost,
		Password: "",
		DB:       int64(cfg.RedisDB),
	})

	// init http client
	options := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&options)
	if err != nil {
		logger.Error("Error init cookie jar: " + err.Error())
	}
	httpClient = http.Client{
		Timeout: cfg.HTTPGetTimeout,
		Transport: &http.Transport{
			Dial: (&net.Dialer{
				Timeout: cfg.HTTPGetTimeout,
			}).Dial,
			TLSHandshakeTimeout: cfg.HTTPGetTimeout,
			DisableCompression:  true,
			DisableKeepAlives:   true,
		},
		Jar: jar,
	}
	httpClient.CheckRedirect = func(req *http.Request, iva []*http.Request) error {
		return RedirectAttempted
	}

	// Prepare responses
	GenerateResponses()

	// Start HTTP server
	err = http.ListenAndServe(":"+strconv.Itoa(cfg.ListenPort), getRouter())
	if err != nil {
		logger.Fatal("Error starting HTTP server: " + err.Error())
		os.Exit(1)
	}
}

func InitCfg() error {
	if err := irukaConfig.GetConfig(cfg, "config.yml"); err != nil {
		return err
	}
	cfg.RedisTTL = time.Duration(cfg.RedisTTLDays*24) * time.Hour
	cfg.RedisErrorTTL = time.Duration(cfg.RedisErrorTTLMins) * time.Minute
	cfg.HTTPGetTimeout = time.Duration(cfg.HTTPGetTimeoutSec) * time.Second

	cfg.MultiTagsMap = make(map[string]bool)
	for _, tag := range cfg.MultiTags {
		cfg.MultiTagsMap[tag] = true
	}

	return nil
}

func InitMetrics() {
	var emptyMap map[string]string
	totalRequestsCounter, _ = metrics.CreateCounter("augmentation_requests_total", "", "", "The total number of requests received.", emptyMap)
	invalidRequestsCounter, _ = metrics.CreateCounter("augmentation_invalid_requests_total", "", "", "The total number of invalid requests received.", emptyMap)
	requestDurationCounter, _ = metrics.CreateCounter("augmentation_request_duration_milliseconds", "", "", "The total amount of time spent processing requests.", emptyMap)
	requestDuration, _ = metrics.CreateHistogram("augmentation_request_duration_hist", "", "", "Histogram of total amount of time spent processing requests.", emptyMap)
	errorsCounter, _ = metrics.CreateCounter("augmentation_errors_logged_total", "", "", "The total number of errors logged.", emptyMap)
	objsProcessedCounterVector, _ = metrics.CreateCounterVector("augmentation_objects_processed_total", "", "", "The total number of objects processed.", emptyMap, []string{"status"})
	cacheHitCounterVector, _ = metrics.CreateCounterVector("augmentation_objects_cache_hits", "", "", "Number of requests hitting redis cache.", emptyMap, []string{"cache"})
}

// Clear cookies regularly
func ClearCookies() {
	options := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	jar, err := cookiejar.New(&options)
	if err != nil {
		logger.Error("Error init cookie jar: " + err.Error())
	} else {
		httpClient.Jar = jar
	}
}

// GenerateResponses prepares the usage and 404 responses. They can then just be returned,
// rather than generated for each individual request.
func GenerateResponses() {
	// Generate usage response
	usageJson, err := rj.NewParsedStringJson(USAGE_STRING)
	if err != nil {
		logger.Fatal("Error generating usage JSON: " + err.Error())
		os.Exit(1)
	}
	usage = []byte(usageJson.Pretty())
	usageJson.Free()

	// Generate 404 response
	notFoundJson := rj.NewDoc()
	notFoundCt := notFoundJson.GetContainerNewObj()
	notFoundCt.AddValue("error", "Not found")
	notFound = notFoundJson.Bytes()
	notFoundJson.Free()
}

// Initialize router and define routes
func getRouter() *mux.Router {
	router := mux.NewRouter().StrictSlash(true)
	router.NotFoundHandler = HandlerWrapper(NotFound)
	router.Methods("GET").Path("/").Handler(HandlerWrapper(Usage))
	router.Methods("POST").Path("/").Handler(HandlerWrapper(Links))
	return router
}

// incSuccessfulCounter increments objsProcessedCounterVector's successful count.
func incSuccessfulCounter() {
	counter, err := objsProcessedCounterVector.GetMetricWithLabelValues("successful")
	if err != nil {
		logger.Error("Incrementing successful objects prometheus counter vector failed: " + err.Error())
	} else {
		counter.Inc()
	}
}

// incSuccessfulCounter increments objsProcessedCounterVector's unsuccessful count.
func incUnsuccessfulCounter() {
	counter, err := objsProcessedCounterVector.GetMetricWithLabelValues("unsuccessful")
	if err != nil {
		logger.Error("Incrementing unsuccessful objects prometheus counter vector failed: " + err.Error())
	} else {
		counter.Inc()
	}
}

// incCacheHitCounter increments cacheHitCounterVector's hit count
func incCacheHitCounter() {
	counter, err := cacheHitCounterVector.GetMetricWithLabelValues("hit")
	if err != nil {
		logger.Error("Inc cache hit prometheus counter vector failed: " + err.Error())
	} else {
		counter.Inc()
	}
}

// incCacheMissCounter increments cacheHitCounterVector's miss count
func incCacheMissCounter() {
	counter, err := cacheHitCounterVector.GetMetricWithLabelValues("miss")
	if err != nil {
		logger.Error("Inc cache miss prometheus counter vector failed: " + err.Error())
	} else {
		counter.Inc()
	}
}

// logProcessed logs throughput every numProcessed objects. Throughput is rounded for
// slightly prettier output.
func logProcessed() {
	numProcessed = numProcessed + 1
	if numProcessed == OBJECTS_PER_LOG {
		now := time.Since(startTime)
		throughput := fmt.Sprintf("%.2f", float64(OBJECTS_PER_LOG)/now.Seconds())
		logger.Info("Processed "+strconv.Itoa(OBJECTS_PER_LOG)+" objects in "+now.String()+" ("+throughput+" per second)", map[string]string{"took": now.String()}, map[string]string{"throughput": throughput})
		numProcessed = 0
		startTime = time.Now()

		ClearCookies()
	}
}

// set test mode, use a test http client
func SetTestClient(client http.Client) {
	httpClient = client
}
