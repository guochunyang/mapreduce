// Copyright 2014 pendo.io
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mapreduce provides a mapreduce pipeline for Google's appengine environment
package mapreduce

import (
	"appengine"
	"appengine/datastore"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// MappedData items are key/value pairs returned from the Map stage. The items are rearranged
// by the shuffle, and (Key, []Value) pairs are passed into the shuffle. KeyHandler interfaces
// provide the operations on MappedData items which are needed by the pipeline, and ValueHandler
// interfaces provide serialization operatons for the values.
type MappedData struct {
	Key   interface{}
	Value interface{}
}

// StatusUpdateFunc functions are passed into Map and Reduce handlers to allow those handlers
// to post arbitrary status messages which are stored in the datastore
type StatusUpdateFunc func(format string, paramList ...interface{})

// Mapper defines a map function; it is passed an item from the input and returns
// a list of mapped items.
type Mapper interface {
	Map(item interface{}, statusUpdate StatusUpdateFunc) ([]MappedData, error)

	// Called once with the job parameters for each mapper task
	SetMapParameters(jsonParameters string)

	// Called when the map is complete. Return is same as for Map()
	// to the output writer
	MapComplete(statusUpdate StatusUpdateFunc) ([]MappedData, error)
}

// Reducer defines the reduce function; it is called once for each key and is given a list
// of all of the values for that key.
type Reducer interface {
	Reduce(key interface{}, values []interface{}, statusUpdate StatusUpdateFunc) (result interface{}, err error)

	// Called once with the job parameters for each mapper task
	SetReduceParameters(jsonParameters string)

	// Called when the reduce is complete. Each item in the results array will be passed separately
	// to the output writer
	ReduceComplete(statusUpdate StatusUpdateFunc) ([]interface{}, error)
}

// TaskStatusChange allows the map reduce framework to notify tasks when their status has changed to RUNNING or DONE. Handy for
// callbacks. Always called after SetMapParameters() and SetReduceParameters()
type TaskStatusChange interface {
	Status(jobId int64, task JobTask)
}

// FatalError wraps an error. If Map or Reduce returns a FatalError the task will not be retried
type FatalError struct{ Err error }

func (fe FatalError) Error() string { return fe.Err.Error() }

// tryAgainError is the inverse of a fatal error; we rework Map() and Reduce() in terms of tryAgainError because
// it makes our internal errors not wrapped at all, making life simpler
type tryAgainError struct{ err error }

func (tae tryAgainError) Error() string { return tae.err.Error() }

// MapReducePipeline defines the complete pipeline for a map reduce job (but not the job itself).
// No per-job information is available for the pipeline functions other than what gets passed in
// via the various interfaces.
type MapReducePipeline interface {
	// The basic pipeline of read, map, shuffle, reduce, save
	InputReader
	Mapper
	IntermediateStorage
	Reducer
	OutputWriter

	// Serialization and sorting primatives for keys and values
	KeyHandler
	ValueHandler

	TaskInterface
	TaskStatusChange
}

// MapReduceJob defines a complete map reduce job, which is the pipeline and the parameters the job
// needs. The types for Inputs and Outputs must match the types for the InputReader and OutputWriter
// in the pipeline.
type MapReduceJob struct {
	MapReducePipeline
	Inputs  InputReader
	Outputs OutputWriter

	// UrlPrefix is the base url path used for mapreduce jobs posted into
	// task queues, and must match the baseUrl passed into MapReduceHandler()
	UrlPrefix string

	// OnCompleteUrl is the url to post to when a job is completed. The full url will include
	// multiple query parameters, including status=(done|error) and id=(jobId). If
	// an error occurred the error parameter will also be displayed. If this is empty, no
	// complete notification is given; it is assumed the caller will poll for results.
	OnCompleteUrl string

	// RetryCount is the number of times individual map/reduce tasks should be retried. Tasks that
	// return errors which are of type FatalError are not retried (defaults to 3, 1
	// means it will never retry).
	RetryCount int

	// SeparateReduceItems means that instead of collapsing all rows with the same key into
	// one call to the reduce function, each row is passed individually (though wrapped in
	// an array of length one to keep the reduce function signature the same)
	SeparateReduceItems bool

	// JobParameters is passed to map and reduce job. They are assumed to be json encoded, though
	// absolutely no effort is made to enforce that.
	JobParameters string
}

func Run(c appengine.Context, job MapReduceJob) (int64, error) {
	readerNames, err := job.Inputs.ReaderNames()
	if err != nil {
		return 0, fmt.Errorf("forming reader names: %s", err)
	} else if len(readerNames) == 0 {
		return 0, fmt.Errorf("no input readers")
	}

	writerNames, err := job.Outputs.WriterNames(c)
	if err != nil {
		return 0, fmt.Errorf("forming writer names: %s", err)
	} else if len(writerNames) == 0 {
		return 0, fmt.Errorf("no output writers")
	}

	reducerCount := len(writerNames)

	jobKey, err := createJob(c, job.UrlPrefix, writerNames, job.OnCompleteUrl, job.SeparateReduceItems, job.JobParameters, job.RetryCount)
	if err != nil {
		return 0, fmt.Errorf("creating job: %s", err)
	}

	firstId, _, err := datastore.AllocateIDs(c, TaskEntity, nil, len(readerNames))
	if err != nil {
		return 0, fmt.Errorf("allocating task ids: %s", err)
	}
	taskKeys := makeTaskKeys(c, firstId, len(readerNames))
	tasks := make([]JobTask, len(readerNames))

	for i, readerName := range readerNames {
		url := fmt.Sprintf("%s/map?taskKey=%s;reader=%s;shards=%d",
			job.UrlPrefix, taskKeys[i].Encode(), readerName,
			reducerCount)

		tasks[i] = JobTask{
			Status: TaskStatusPending,
			Url:    url,
			Type:   TaskTypeMap,
		}
	}

	if err := createTasks(c, jobKey, taskKeys, tasks, StageMapping); err != nil {
		if _, innerErr := markJobFailed(c, jobKey); err != nil {
			c.Errorf("failed to log job %d as failed: %s", jobKey.IntID(), innerErr)
		}
		return 0, fmt.Errorf("creating tasks: %s", err)
	}

	for i := range tasks {
		if err := job.PostTask(c, tasks[i].Url, job.JobParameters); err != nil {
			if _, innerErr := markJobFailed(c, jobKey); err != nil {
				c.Errorf("failed to log job %d as failed: %s", jobKey.IntID(), innerErr)
			}
			return 0, fmt.Errorf("posting task: %s", err)
		}
	}

	if err := job.PostStatus(c, fmt.Sprintf("%s/map-monitor?jobKey=%s", job.UrlPrefix, jobKey.Encode())); err != nil {
		c.Criticalf("failed to start map monitor task: %s", err)
	}

	return jobKey.IntID(), nil
}

type urlHandler struct {
	pipeline   MapReducePipeline
	baseUrl    string
	getContext func(r *http.Request) appengine.Context
}

// MapReduceHandler returns an http.Handler which is responsible for all of the
// urls pertaining to the mapreduce job. The baseUrl acts as the name for the
// type of job being run.
func MapReduceHandler(baseUrl string, pipeline MapReducePipeline,
	getContext func(r *http.Request) appengine.Context) http.Handler {

	return urlHandler{pipeline, baseUrl, getContext}
}

func (h urlHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := h.getContext(r)

	if strings.HasSuffix(r.URL.Path, "/map-monitor") || strings.HasSuffix(r.URL.Path, "/reduce-monitor") {
		if jobKeyStr := r.FormValue("jobKey"); jobKeyStr == "" {
			http.Error(w, "jobKey parameter required", http.StatusBadRequest)
		} else if jobKey, err := datastore.DecodeKey(jobKeyStr); err != nil {
			http.Error(w, fmt.Sprintf("invalid jobKey: %s", err.Error()),
				http.StatusBadRequest)
		} else if strings.HasSuffix(r.URL.Path, "/map-monitor") {
			mapMonitorTask(c, h.pipeline, jobKey, r, 5*time.Minute)
		} else {
			reduceMonitorTask(c, h.pipeline, jobKey, r, 5*time.Minute)
		}

		return
	}

	var taskKey *datastore.Key
	var err error

	if taskKeyStr := r.FormValue("taskKey"); taskKeyStr == "" {
		http.Error(w, "taskKey parameter required", http.StatusBadRequest)
		return
	} else if taskKey, err = datastore.DecodeKey(taskKeyStr); err != nil {
		http.Error(w, fmt.Sprintf("invalid taskKey: %s", err.Error()),
			http.StatusBadRequest)
		return
	}

	if strings.HasSuffix(r.URL.Path, "/reduce") {
		reduceTask(c, h.baseUrl, h.pipeline, taskKey, w, r)
	} else if strings.HasSuffix(r.URL.Path, "/map") {
		mapTask(c, h.baseUrl, h.pipeline, taskKey, w, r)
	} else if strings.HasSuffix(r.URL.Path, "/mapstatus") ||
		strings.HasSuffix(r.URL.Path, "/reducestatus") {

		updateTask(c, taskKey, "", r.FormValue("msg"), nil)
	} else {
		http.Error(w, "unknown request url", http.StatusNotFound)
		return
	}
}

func makeStatusUpdateFunc(c appengine.Context, pipeline MapReducePipeline, urlStr string, taskKey string) StatusUpdateFunc {
	return func(format string, paramList ...interface{}) {
		msg := fmt.Sprintf(format, paramList...)
		if key, err := datastore.DecodeKey(taskKey); err != nil {
			c.Errorf("failed to decode task key for status: %s", err)
		} else if _, err := updateTask(c, key, "", msg, nil); err != nil {
			c.Errorf("failed to update task status: %s", err)
		}
	}
}

// IgnoreTaskStatusChange is an implementation of TaskStatusChange which ignores the call
type IgnoreTaskStatusChange struct{}

func (e *IgnoreTaskStatusChange) Status(jobId int64, task JobTask) {
}
