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
	"hash/crc32"
	"strconv"
)

// KeyHandler must be implemented for each key type to enable shuffling and storing of map keys
type KeyHandler interface {
	// Less returns a< b
	Less(a, b interface{}) bool

	// Equals returns a == b
	Equal(a, b interface{}) bool

	// KeyDump converts a key into a byte array
	KeyDump(a interface{}) []byte

	// KeyDump converts a byte array into a key
	KeyLoad([]byte) (interface{}, error)

	// Shard returns the shard number a key belongs to, given the total number of shards
	// which are being used for the job
	Shard(a interface{}, shardCount int) int

	// Provides the (probably json) parameters for the job; may be useful for sharding strategy
	SetShardParameters(jsonParameters string)
}

// StringKeyHandler provides a KeyHandler for string keys
type StringKeyHandler struct{}

func (s StringKeyHandler) KeyDump(a interface{}) []byte {
	return []byte(a.(string))
}

func (s StringKeyHandler) KeyLoad(a []byte) (interface{}, error) {
	return string(a), nil
}

func (s StringKeyHandler) Less(a, b interface{}) bool {
	return a.(string) < b.(string)
}

func (s StringKeyHandler) Equal(a, b interface{}) bool {
	return a.(string) == b.(string)
}

func (s StringKeyHandler) Shard(strInt interface{}, shardCount int) int {
	str := strInt.(string)
	sum := crc32.ChecksumIEEE([]byte(str))
	return int(sum % uint32(shardCount))
}

func (s StringKeyHandler) SetShardParameters(jsonParameters string) {}

// Int64KeyHandler provides a KeyHandler for int64 keys. A hash is used for computing the shards
// to distribute evenly. We encode things are strings for readability.
type Int64KeyHandler struct{}

func (s Int64KeyHandler) KeyDump(a interface{}) []byte {
	return []byte(strconv.FormatInt(a.(int64), 10))
}

func (s Int64KeyHandler) KeyLoad(a []byte) (interface{}, error) {
	i, err := strconv.ParseInt(string(a), 10, 64)
	if err != nil {
		return nil, err
	}

	return i, nil
}

func (s Int64KeyHandler) Less(a, b interface{}) bool {
	return a.(int64) < b.(int64)
}

func (s Int64KeyHandler) Equal(a, b interface{}) bool {
	return a.(int64) == b.(int64)
}

func (s Int64KeyHandler) Shard(strInt interface{}, shardCount int) int {
	i := strInt.(int64)
	sum := crc32.ChecksumIEEE([]byte(strconv.FormatInt(i, 10)))
	return int(sum % uint32(shardCount))
}

func (s Int64KeyHandler) SetShardParameters(jsonParameters string) {}
