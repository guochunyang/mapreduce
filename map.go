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

package mapreduce

import (
	"appengine"
	"appengine/datastore"
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"time"
)

func mapMonitorTask(c appengine.Context, pipeline MapReducePipeline, jobKey *datastore.Key, r *http.Request, timeout time.Duration) {
	start := time.Now()

	job, err := waitForStageCompletion(c, pipeline, jobKey, StageMapping, StageReducing, timeout)
	if err != nil {
		c.Criticalf("waitForStageCompletion() failed: %s", err)
		return
	} else if job.Stage == StageMapping {
		c.Infof("wait timed out -- restarting monitor")
		if err := pipeline.PostStatus(c, fmt.Sprintf("%s/map-monitor?jobKey=%s", job.UrlPrefix, jobKey.Encode())); err != nil {
			c.Criticalf("failed to start map monitor task: %s", err)
		}

		return
	}

	c.Infof("map stage completed -- stage is now %s", job.Stage)

	// erm... we just did this in jobStageComplete. dumb to do it again
	mapTasks, err := gatherTasks(c, job)
	if err != nil {
		c.Errorf("failed loading tasks: %s", mapTasks)
		jobFailed(c, pipeline, jobKey, fmt.Errorf("error loading tasks after map complete: %s", err.Error()))
		return
	}

	// we have one set for each reducer task
	storageNames := make([][]string, len(job.WriterNames))

	for i := range mapTasks {
		var shardNames map[string]int
		if err = json.Unmarshal([]byte(mapTasks[i].Result), &shardNames); err != nil {
			c.Errorf(`unmarshal error for result from map %d result '%+v'`, job.FirstTaskId+int64(i), mapTasks[i].Result)
			jobFailed(c, pipeline, jobKey, fmt.Errorf("cannot unmarshal map shard names: %s", err.Error()))
			return
		} else {
			for name, shard := range shardNames {
				storageNames[shard] = append(storageNames[shard], name)
			}
		}
	}

	firstId, _, err := datastore.AllocateIDs(c, TaskEntity, nil, len(job.WriterNames))
	if err != nil {
		jobFailed(c, pipeline, jobKey, fmt.Errorf("failed to allocate ids for reduce tasks: %s", err.Error()))
		return
	}
	taskKeys := makeTaskKeys(c, firstId, len(job.WriterNames))
	tasks := make([]JobTask, 0, len(job.WriterNames))

	for shard := range job.WriterNames {
		if shards := storageNames[shard]; len(shards) > 0 {
			url := fmt.Sprintf("%s/reduce?taskKey=%s;shard=%d;writer=%s",
				job.UrlPrefix, taskKeys[len(tasks)].Encode(), shard, url.QueryEscape(job.WriterNames[shard]))

			firstId++

			shardJson, _ := json.Marshal(shards)
			shardZ := &bytes.Buffer{}
			w := zlib.NewWriter(shardZ)
			w.Write(shardJson)
			w.Close()

			tasks = append(tasks, JobTask{
				Status:              TaskStatusPending,
				Url:                 url,
				ReadFrom:            shardZ.Bytes(),
				SeparateReduceItems: job.SeparateReduceItems,
				Type:                TaskTypeReduce,
			})
		}
	}

	taskKeys = taskKeys[0:len(tasks)]

	if err := createTasks(c, jobKey, taskKeys, tasks, StageReducing); err != nil {
		jobFailed(c, pipeline, jobKey, fmt.Errorf("failed to create reduce tasks: %s", err.Error()))
		return
	}

	for i := range tasks {
		if err := pipeline.PostTask(c, tasks[i].Url, job.JsonParameters); err != nil {
			jobFailed(c, pipeline, jobKey, fmt.Errorf("failed to post reduce task: %s", err.Error()))
			return
		}
	}

	if err := pipeline.PostStatus(c, fmt.Sprintf("%s/reduce-monitor?jobKey=%s", job.UrlPrefix, jobKey.Encode())); err != nil {
		jobFailed(c, pipeline, jobKey, fmt.Errorf("failed to start reduce monitor: %s", err.Error()))
		return
	}

	c.Infof("mapping complete after %s of monitoring ", time.Now().Sub(start))
}

func mapTask(c appengine.Context, baseUrl string, mr MapReducePipeline, taskKey *datastore.Key, w http.ResponseWriter, r *http.Request) {
	var finalErr error
	var shardNames map[string]int
	var task JobTask

	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 16384)
			bytes := runtime.Stack(stack, false)
			c.Criticalf("panic inside of map task %s: %s\n%s\n", taskKey.Encode(), r, stack[0:bytes])

			if err := retryTask(c, mr, task.Job, taskKey); err != nil {
				panic(fmt.Errorf("failed to retry task after panic: %s", err))
			}
		}
	}()

	start := time.Now()

	// we do this before starting the task below so that the parameters are set before
	// the task status callback is invoked
	jsonParameters := r.FormValue("json")
	mr.SetMapParameters(jsonParameters)
	mr.SetShardParameters(jsonParameters)

	if t, err, retry := startTask(c, mr, taskKey); err != nil && retry {
		c.Criticalf("failed updating task to running: %s", err)
		http.Error(w, err.Error(), 500) // this will run us again
		return
	} else if err != nil {
		c.Criticalf("(fatal) failed updating task to running: %s", err)
		http.Error(w, err.Error(), 200) // this will run us again
		return
	} else {
		task = t
	}

	if readerName := r.FormValue("reader"); readerName == "" {
		finalErr = fmt.Errorf("reader parameter required")
	} else if shardStr := r.FormValue("shards"); shardStr == "" {
		finalErr = fmt.Errorf("shards parameter required")
	} else if shardCount, err := strconv.ParseInt(shardStr, 10, 32); err != nil {
		finalErr = fmt.Errorf("error parsing shard count: %s", err.Error())
	} else if reader, err := mr.ReaderFromName(c, readerName); err != nil {
		finalErr = fmt.Errorf("error making reader: %s", err)
	} else {
		shardNames, finalErr = mapperFunc(c, mr, reader, int(shardCount),
			makeStatusUpdateFunc(c, mr, fmt.Sprintf("%s/mapstatus", baseUrl), taskKey.Encode()))
	}

	if err := endTask(c, mr, task.Job, taskKey, finalErr, shardNames); err != nil {
		c.Criticalf("Could not finish task: %s", err)
		http.Error(w, err.Error(), 500)
		return
	}

	c.Infof("mapper done after %s", time.Now().Sub(start))
}

func mapperFunc(c appengine.Context, mr MapReducePipeline, reader SingleInputReader, shardCount int,
	statusFunc StatusUpdateFunc) (map[string]int, error) {

	dataSets := make([]mappedDataList, shardCount)
	spills := make([]spillStruct, 0)
	for i := range dataSets {
		dataSets[i] = mappedDataList{data: make([]MappedData, 0), compare: mr}
	}

	var err error
	var item interface{}
	size := 0
	count := 0
	for item, err = reader.Next(); item != nil && err == nil; item, err = reader.Next() {
		itemList, err := mr.Map(item, statusFunc)

		if err != nil {
			if _, ok := err.(FatalError); ok {
				err = err.(FatalError).Err
			} else {
				err = tryAgainError{err}
			}

			return nil, err
		}

		for _, mappedItem := range itemList {
			shard := mr.Shard(mappedItem.Key, shardCount)
			dataSets[shard].data = append(dataSets[shard].data, mappedItem)

			val, _ := mr.ValueDump(mappedItem.Value)
			size += len(val)
			count++
		}

		if size > 4*1024*1024 {
			if spill, err := writeSpill(c, mr, dataSets); err != nil {
				return nil, tryAgainError{err}
			} else {
				spills = append(spills, spill)
			}

			c.Infof("wrote spill of %d items", count)

			size = 0
			count = 0
			for shard := range dataSets {
				dataSets[shard].data = dataSets[shard].data[0:0]
			}
		}
	}

	reader.Close()

	if err != nil {
		if _, ok := err.(FatalError); ok {
			err = err.(FatalError).Err
		} else {
			err = tryAgainError{err}
		}

		return nil, err
	}

	itemList, err := mr.MapComplete(statusFunc)
	if err != nil {
		if _, ok := err.(FatalError); ok {
			err = err.(FatalError).Err
		} else {
			err = tryAgainError{err}
		}

		return nil, err
	}

	for _, item := range itemList {
		shard := mr.Shard(item.Key, shardCount)
		dataSets[shard].data = append(dataSets[shard].data, item)
	}

	if spill, err := writeSpill(c, mr, dataSets); err != nil {
		return nil, tryAgainError{err}
	} else {
		spills = append(spills, spill)
	}

	finalNames := make(map[string]int)
	for try := 0; try < 5; try++ {
		if names, err := mergeSpills(c, mr, mr, spills); err != nil {
			c.Infof("spill merge failed try %d: %s", try, err)
		} else {
			for shard, name := range names {
				finalNames[name] = shard
			}
			break
		}
	}

	if err != nil {
		return nil, tryAgainError{err}
	}

	c.Infof("finalNames: %#v", finalNames)

	return finalNames, nil
}
