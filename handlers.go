package main

import (
	"errors"
	"io"
	"io/ioutil"
	"net/http"
	"time"

	rj "github.com/bottlenose-inc/rapidjson" // faster json handling
)

// SendErrorResponse sends a response with the provided error message and status code.
func SendErrorResponse(w http.ResponseWriter, message string, status int) {
	errorsCounter.Inc()
	errorJson := rj.NewDoc()
	defer errorJson.Free()
	errorCt := errorJson.GetContainerNewObj()
	errorCt.AddValue("error", message)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(errorJson.Bytes())
	if err != nil {
		logger.Error("Error writing response: "+err.Error(), map[string]string{"error": errorJson.String()})
	}
}

// GetRequests is a generic function that parses properly formatted requests to an augmentation.
// It ensures the correct Content-Type header is provided and ensures the request is properly
// formatted. Requests are also truncated to BODY_LIMIT_BYTES to avoid huge requests causing problems.
func GetRequests(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var emptyMap []byte

	// Send error response if incorrect Content-Type is provided
	if r.Header.Get("Content-Type") != "application/json" {
		invalidRequestsCounter.Inc()
		logger.Warning("Client request did not set Content-Type header to application/json", map[string]string{"Content-Type": r.Header.Get("Content-Type")})
		SendErrorResponse(w, "Content-Type must be set to application/json", http.StatusBadRequest)
		return emptyMap, errors.New("Content-Type must be set to application/json")
	}

	// Read body up to size of BODY_LIMIT_BYTES
	body, err := ioutil.ReadAll(io.LimitReader(r.Body, BODY_LIMIT_BYTES))
	if err != nil {
		logger.Error("Error reading request body: " + err.Error())
		SendErrorResponse(w, "Error reading request body", http.StatusInternalServerError)
		return emptyMap, err
	}
	if err := r.Body.Close(); err != nil {
		logger.Error("Error closing body: " + err.Error())
		SendErrorResponse(w, "Error reading request body", http.StatusInternalServerError)
		return emptyMap, err
	}

	return body, err
}

// HandlerWrapper is "wrapped" around all handlers to allow generation of
// common metrics we want for every valid api call.
func HandlerWrapper(handler http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		http.HandlerFunc(handler).ServeHTTP(w, r)
		totalRequestsCounter.Inc()
		duration := time.Since(start).Seconds() / 1000
		requestDurationCounter.Add(duration)
		requestDuration.Observe(duration)
	})
}

// NotFound sends a 404 response.
func NotFound(w http.ResponseWriter, r *http.Request) {
	invalidRequestsCounter.Inc()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, err := w.Write(notFound)
	if err != nil {
		// Should not run into this error...
		logger.Error("Error encoding 404 response: " + err.Error())
	}
}

// Usage sends the usage information response.
func Usage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(usage)
	if err != nil {
		// Should not run into this error...
		logger.Error("Error encoding usage response: " + err.Error())
	}
}
