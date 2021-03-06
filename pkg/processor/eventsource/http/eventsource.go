/*
Copyright 2017 The Nuclio Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	net_http "net/http"
	"strconv"
	"time"

	"github.com/nuclio/nuclio/pkg/common"
	"github.com/nuclio/nuclio/pkg/errors"
	"github.com/nuclio/nuclio/pkg/processor/eventsource"
	"github.com/nuclio/nuclio/pkg/processor/worker"
	"github.com/nuclio/nuclio/pkg/zap"

	"github.com/nuclio/nuclio-sdk"
	"github.com/valyala/fasthttp"
)

type http struct {
	eventsource.AbstractEventSource
	configuration    *Configuration
	event            Event
	bufferLoggerPool *nucliozap.BufferLoggerPool
}

func newEventSource(logger nuclio.Logger,
	workerAllocator worker.Allocator,
	configuration *Configuration) (eventsource.EventSource, error) {

	bufferLoggerPool, err := nucliozap.NewBufferLoggerPool(8,
		configuration.ID,
		"json",
		nucliozap.DebugLevel)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create buffer loggers")
	}

	// we need a shareable allocator to support multiple go-routines. check that we were provided
	// with a valid allocator
	if !workerAllocator.Shareable() {
		return nil, errors.New("HTTP event source requires a shareable worker allocator")
	}

	newEventSource := http{
		AbstractEventSource: eventsource.AbstractEventSource{
			ID:              configuration.ID,
			Logger:          logger,
			WorkerAllocator: workerAllocator,
			Class:           "sync",
			Kind:            "http",
		},
		configuration:    configuration,
		event:            Event{},
		bufferLoggerPool: bufferLoggerPool,
	}

	return &newEventSource, nil
}

func (h *http) Start(checkpoint eventsource.Checkpoint) error {
	h.Logger.InfoWith("Starting", "listenAddress", h.configuration.ListenAddress)

	s := &fasthttp.Server{
		Handler: h.requestHandler,
		Name:    "nuclio",
	}

	// start listening
	go s.ListenAndServe(h.configuration.ListenAddress)

	return nil
}

func (h *http) Stop(force bool) (eventsource.Checkpoint, error) {

	// TODO
	return nil, nil
}

func (h *http) GetConfig() map[string]interface{} {
	return common.StructureToMap(h.configuration)
}

func (h *http) requestHandler(ctx *fasthttp.RequestCtx) {
	var functionLogger nuclio.Logger
	var bufferLogger *nucliozap.BufferLogger

	// attach the context to the event
	h.event.ctx = ctx

	// get the log level required
	responseLogLevel := ctx.Request.Header.Peek("X-nuclio-log-level")

	// check if we need to return the logs as part of the response in the header
	if responseLogLevel != nil {

		// set the function logger to the runtime's logger capable of writing to a buffer
		bufferLogger, _ = h.bufferLoggerPool.Allocate(nil)

		// set the logger level
		bufferLogger.Logger.SetLevel(nucliozap.GetLevelByName(string(responseLogLevel)))

		// write open bracket for JSON
		bufferLogger.Buffer.Write([]byte("["))

		// set the function logger to that of the chosen buffer logger
		functionLogger, _ = nucliozap.NewMuxLogger(bufferLogger.Logger, h.Logger)
	}

	response, submitError, processError := h.AllocateWorkerAndSubmitEvent(&h.event, functionLogger, 10*time.Second)

	if responseLogLevel != nil {

		// remove trailing comma
		logContents := bufferLogger.Buffer.Bytes()

		// if there are no logs, we will only happen the open bracket [ we wrote above and we
		// want to keep that. so only remove the last character if there's more than the open bracket
		if len(logContents) > 1 {
			logContents = logContents[:len(logContents)-1]
		}

		// write open bracket for JSON
		logContents = append(logContents, byte(']'))

		ctx.Response.Header.SetBytesV("X-nuclio-logs", logContents)

		// return the buffer logger to the pool
		h.bufferLoggerPool.Release(bufferLogger)
	}

	// if we failed to submit the event to a worker
	if submitError != nil {
		switch errors.Cause(submitError) {

		// no available workers
		case worker.ErrNoAvailableWorkers:
			ctx.Response.SetStatusCode(net_http.StatusServiceUnavailable)

		// something else - most likely a bug
		default:
			h.Logger.WarnWith("Failed to submit event", "err", submitError)
			ctx.Response.SetStatusCode(net_http.StatusInternalServerError)
		}

		return
	}

	// if the function returned an error - just return 500
	if processError != nil {
		var statusCode int

		// check if the user returned an error with a status code
		errorWithStatusCode, errorHasStatusCode := processError.(nuclio.ErrorWithStatusCode)

		// if the user didn't use one of the errors with status code, return internal error
		// otherwise return the status code the user wanted
		if !errorHasStatusCode {
			statusCode = net_http.StatusInternalServerError
		} else {
			statusCode = errorWithStatusCode.StatusCode()
		}

		ctx.Response.SetStatusCode(statusCode)
		return
	}

	// format the response into the context, based on its type
	switch typedResponse := response.(type) {
	case nuclio.Response:

		// set body
		ctx.Response.SetBody(typedResponse.Body)

		// set headers
		for headerKey, headerValue := range typedResponse.Headers {
			switch typedHeaderValue := headerValue.(type) {
			case string:
				ctx.Response.Header.Set(headerKey, typedHeaderValue)
			case int:
				ctx.Response.Header.Set(headerKey, strconv.Itoa(typedHeaderValue))
			}
		}

		// set content type if set
		if typedResponse.ContentType != "" {
			ctx.SetContentType(typedResponse.ContentType)
		}

		// set status code if set
		if typedResponse.StatusCode != 0 {
			ctx.Response.SetStatusCode(typedResponse.StatusCode)
		}

	case []byte:
		ctx.Write(typedResponse)

	case string:
		ctx.WriteString(typedResponse)
	}
}
