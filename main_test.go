package main

import (
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gopkg.in/redis.v3"

	irukaLogger "github.com/bottlenose-inc/go-common-tools/logger" // go-common-tools bunyan-style logger package
	"github.com/bottlenose-inc/go-common-tools/metrics"            // go-common-tools Prometheus metrics package
	irukatest "github.com/bottlenose-inc/go-common-tools/testhttp" // go-common-tools test helper package
	rj "github.com/bottlenose-inc/rapidjson"                       // faster json handling
	"github.com/stretchr/testify/assert"                           // Assertion package
)

var (
	server    *httptest.Server
	serverUrl string
)

func TestMain(m *testing.M) {
	// load config
	if err := InitCfg(); err != nil {
		logger.Fatal("Error loading config: " + err.Error())
		os.Exit(1)
	}

	server = httptest.NewServer(getRouter())
	serverUrl = fmt.Sprintf("%s/", server.URL)
	logger, _ = irukaLogger.NewLogger(SERVICE_NAME+"-test", "/dev/null") // Ignore log messages
	defer logger.Close()

	// Start Prometheus metrics server and initialize metrics to avoid panic during tests
	go metrics.StartPrometheusMetricsServer(SERVICE_NAME+"-test", logger, cfg.PrometheusPort)
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

	// Prepare responses
	GenerateResponses()

	// Run tests
	run := m.Run()
	os.Exit(run)
}

func TestUsage(t *testing.T) {
	fmt.Println(">> Testing GET / (usage information)...")

	// Perform request
	resp, err := http.Get(serverUrl)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	bodyJSON, _ := rj.NewParsedJson(body)

	expected, _ := rj.NewParsedStringJson(USAGE_STRING)

	assert.True(t, bodyJSON.GetContainer().IsEqual(expected.GetContainer()))
	bodyJSON.Free()
	expected.Free()
}

func TestNotFound(t *testing.T) {
	fmt.Println(">> Testing GET /fourohfour (not found)...")

	// Perform request
	resp, err := http.Get(serverUrl + "fourohfour")
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 404, resp.StatusCode, "response status code should be 404")
	expected := `{"error":"Not found"}`
	assert.Equal(t, []byte(expected), body, "not found response should match")
}

func TestBadJson(t *testing.T) {
	fmt.Println(">> Testing POST / (with bad JSON)...")

	// Prepare request
	reader := strings.NewReader(`{]}`)

	// Perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 400, resp.StatusCode, "response status code should be 400")
	expected := `{"error":"Unable to parse request - invalid JSON detected"}`
	assert.Equal(t, []byte(expected), body, "not found response should match")
}

func TestMissingRequestKey(t *testing.T) {
	fmt.Println(">> Testing POST / (with missing url key)...")

	// Prepare request
	reader := strings.NewReader(`{"bad_request": [{"url": "http://www.google.com"}]}`)

	// Perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 400, resp.StatusCode, "response status code should be 400")
	expected := `{"error":"Unable to parse request - invalid JSON detected"}`
	assert.Equal(t, []byte(expected), body, "not found response should match")
}

func TestMissingUrlKey(t *testing.T) {
	fmt.Println(">> Testing POST / (with missing url key)...")

	// Prepare request
	reader := strings.NewReader(`{"request": [{"bad_url": "http://www.google.com"}]}`)

	// Perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 400, resp.StatusCode, "response status code should be 400")
	expected := `{"response":[{"error":"Missing url key"}]}`
	assert.Equal(t, []byte(expected), body, "not found response should match")
}

func TestBlacklist(t *testing.T) {
	fmt.Println(">> Testing POST / (with blacklisted url)...")

	// Prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://squidos.com/"}]}`)

	// Perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 203, resp.StatusCode, "response status code should be 204")
	expected := `{"response":[{"error":"Invalid URL (blacklisted)"}]}`
	assert.Equal(t, []byte(expected), body, "not found response should match")
}

func TestSuccessfulGoogle(t *testing.T) {
	fmt.Println(">> Testing POST / (with proper request for www.google.com)...")

	// remove redis records
	hash := fmt.Sprintf("%x", md5.Sum([]byte("www.google.com")))
	redisClient.Del(hash)

	google, err := ioutil.ReadFile("test/google.out")

	mock := irukatest.InitMockHTTP()
	mock.AddTestData("http://www.google.com/", 200, google)
	defer mock.Close()
	SetTestClient(mock.Client)

	// prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://www.google.com/"}]}`)

	// perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 200, resp.StatusCode, "response status code should be 200")

	responseJson, _ := rj.NewParsedJson(body)
	defer responseJson.Free()
	respCt := responseJson.GetContainer()
	responses, _ := respCt.GetMember("response")
	respArray, _, _ := responses.GetArray()
	link, _ := respArray[0].GetMember("link")
	title, _ := link.GetMember("title")
	titleStr, _ := title.GetString()
	assert.Equal(t, "Google", titleStr, "Title should be Google")
	id, _ := link.GetMember("id")
	idStr, _ := id.GetString()
	assert.Equal(t, "www.google.com", idStr, "ID should be www.google.com")
	contentType, _ := link.GetMember("type")
	ctStr, _ := contentType.GetString()
	assert.Equal(t, "website", ctStr, "type should be website")
}

func TestSuccessfulTheRock(t *testing.T) {
	fmt.Println(">> Testing POST / (with proper request for www.imdb.com/title/tt0117500/)...")

	// remove redis records
	hash := fmt.Sprintf("%x", md5.Sum([]byte("www.imdb.com/title/tt0117500/")))
	redisClient.Del(hash)

	imdb, err := ioutil.ReadFile("test/imdb.out")

	mock := irukatest.InitMockHTTP()
	mock.AddTestData("http://www.imdb.com/title/tt0117500/", 200, imdb)
	defer mock.Close()
	SetTestClient(mock.Client)

	// prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://www.imdb.com/title/tt0117500/"}]}`)

	// perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 200, resp.StatusCode, "response status code should be 200")

	responseJson, _ := rj.NewParsedJson(body)
	defer responseJson.Free()
	respCt := responseJson.GetContainer()
	responses, _ := respCt.GetMember("response")
	respArray, _, _ := responses.GetArray()
	link, _ := respArray[0].GetMember("link")
	title, _ := link.GetMember("title")
	titleStr, _ := title.GetString()
	assert.Equal(t, "The Rock (1996)", titleStr, "Title should be The Rock (1996)")
	id, _ := link.GetMember("id")
	idStr, _ := id.GetString()
	assert.Equal(t, "www.imdb.com/title/tt0117500", idStr, "ID should be www.imdb.com/title/tt0117500")
	contentType, _ := link.GetMember("type")
	ctStr, _ := contentType.GetString()
	assert.Equal(t, "video.movie", ctStr, "type should be video.movie")
}

func TestTHR(t *testing.T) {
	fmt.Println(">> Testing THR js redirect")

	thr, err := ioutil.ReadFile("test/thr.cm.out")
	basic, err := ioutil.ReadFile("test/basic.out")

	mock := irukatest.InitMockHTTP()
	mock.AddTestData("http://thr.cm/scmf/RedirectMe", 200, thr)
	mock.AddTestData("http://trib.al/QNAQUT9", 200, basic)
	defer mock.Close()
	SetTestClient(mock.Client)

	// prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://thr.cm/scmf/RedirectMe"}]}`)

	// perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 200, resp.StatusCode, "response status code should be 200")

	responseJson, _ := rj.NewParsedJson(body)
	defer responseJson.Free()
	respCt := responseJson.GetContainer()
	responses, _ := respCt.GetMember("response")
	respArray, _, _ := responses.GetArray()
	link, _ := respArray[0].GetMember("link")
	title, _ := link.GetMember("title")
	titleStr, _ := title.GetString()
	assert.Equal(t, "Basic Test Page", titleStr, "Title should be Basic Test Page")
}

func TestBadImage(t *testing.T) {
	fmt.Println(">> Testing bad image data ignored")

	bad, err := ioutil.ReadFile("test/badimage.out")

	mock := irukatest.InitMockHTTP()
	mock.AddTestData("http://frotissanguineo.blogspot.com/2016/05/frotissanguineo.html", 200, bad)
	defer mock.Close()
	SetTestClient(mock.Client)

	// prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://frotissanguineo.blogspot.com/2016/05/frotissanguineo.html"}]}`)

	// perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 200, resp.StatusCode, "response status code should be 200")

	responseJson, _ := rj.NewParsedJson(body)
	defer responseJson.Free()
	respCt := responseJson.GetContainer()
	responses, _ := respCt.GetMember("response")
	respArray, _, _ := responses.GetArray()
	assert.Equal(t, false, respArray[0].HasMember("imageUrl"), "Should not have imageUrl")
}

func TestRedirectToGoogle(t *testing.T) {
	fmt.Println(">> Testing POST / (with redirecting url to www.google.com)...")

	// remove redis records
	hash := fmt.Sprintf("%x", md5.Sum([]byte("www.google.com")))
	redisClient.Del(hash)

	google, err := ioutil.ReadFile("test/google.out")

	mock := irukatest.InitMockHTTP()
	mock.AddTestData("http://www.google.com/", 200, google)
	defer mock.Close()
	SetTestClient(mock.Client)

	// prepare request
	reader := strings.NewReader(`{"request": [{"url": "http://adf.ly/13775363/http://www.google.com/"}]}`)

	// perform request
	resp, err := http.Post(serverUrl, "application/json", reader)
	assert.Nil(t, err, "request should not error")
	defer resp.Body.Close()

	// Read response
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err, "should not error reading response")
	assert.Equal(t, 200, resp.StatusCode, "response status code should be 200")

	responseJson, _ := rj.NewParsedJson(body)
	defer responseJson.Free()
	respCt := responseJson.GetContainer()
	responses, _ := respCt.GetMember("response")
	respArray, _, _ := responses.GetArray()
	link, _ := respArray[0].GetMember("link")
	title, _ := link.GetMember("title")
	titleStr, _ := title.GetString()
	assert.Equal(t, "Google", titleStr, "Title should be Google")
	id, _ := link.GetMember("id")
	idStr, _ := id.GetString()
	assert.Equal(t, "www.google.com", idStr, "ID should be www.google.com")
	contentType, _ := link.GetMember("type")
	ctStr, _ := contentType.GetString()
	assert.Equal(t, "website", ctStr, "type should be website")
}
