// Copyright 2015-2016 mozilla
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build linux

package native

import (
	"github.com/coreos/go-systemd/sdjournal"
	"github.com/trivago/gollum/core"
	"github.com/trivago/gollum/core/log"
	"github.com/trivago/gollum/shared"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sdOffsetTail = "newest"
	sdOffsetHead = "oldest"
)

// Systemd consumer plugin
// The systemd consumer allows to read from the systemd journal.
// When attached to a fuse, this consumer will stop reading messages in case
// that fuse is burned.
// Configuration example
//
//  - "native.Systemd":
//    SystemdUnit: "sshd.service"
//    DefaultOffset: "Newest"
//    OffsetFile: ""
//
// SystemdUnit defines what journal will be followed. This uses
// journal.add_match with _SYSTEMD_UNIT. By default this is set to "", which
// disables the filter.
//
// DefaultOffset defines where to start reading the file. Valid values are
// "oldest" and "newest". If OffsetFile is defined the DefaultOffset setting
// will be ignored unless the file does not exist.
// By default this is set to "newest".
//
// OffsetFile defines the path to a file that stores the current offset. If
// the consumer is restarted that offset is used to continue reading. By
// default this is set to "" which disables the offset file.

type SystemdConsumer struct {
	core.ConsumerBase
	journal    *sdjournal.Journal
	offset     uint64
	offsetFile string
	running    bool
}

func init() {
	shared.TypeRegistry.Register(SystemdConsumer{})
}

// Configure initializes this consumer with values from a plugin config.
func (cons *SystemdConsumer) Configure(conf core.PluginConfig) error {
	err := cons.ConsumerBase.Configure(conf)
	if err != nil {
		return err
	}

	cons.journal, err = sdjournal.NewJournal()
	if err != nil {
		return err
	}

	if sdUnit := conf.GetString("SystemdUnit", ""); sdUnit != "" {
		err = cons.journal.AddMatch("_SYSTEMD_UNIT=" + sdUnit)
		if err != nil {
			return err
		}
	}

	// Offset
	offsetValue := strings.ToLower(conf.GetString("DefaultOffset", sdOffsetTail))

	cons.offsetFile = conf.GetString("OffsetFile", "")
	if cons.offsetFile != "" {
		fileContents, err := ioutil.ReadFile(cons.offsetFile)
		if err != nil {
			Log.Error.Print("Error reading offset file: ", err)
		}
		if string(fileContents) != "" {
			offsetValue = string(fileContents)
		}
	}

	switch offsetValue {
	case sdOffsetHead:
		err = cons.journal.SeekHead()

	case sdOffsetTail:
		err = cons.journal.SeekTail()
		if err != nil {
			return err
		}
		// start *after* the newest record
		_, err = cons.journal.Next()

	default:
		offset, err := strconv.ParseUint(offsetValue, 10, 64)
		if err != nil {
			return err
		}
		err = cons.journal.SeekRealtimeUsec(offset)
		if err != nil {
			return err
		}
		// start *after* the specified time
		_, err = cons.journal.Next()
	}

	if err != nil {
		return err
	}

	// Register close to the control message handler
	cons.SetStopCallback(cons.close)

	return nil
}

func (cons *SystemdConsumer) storeOffset(offset uint64) {
	ioutil.WriteFile(cons.offsetFile, []byte(strconv.FormatUint(offset, 10)), 0644)
}

func (cons *SystemdConsumer) enqueueAndPersist(data []byte, sequence uint64) {
	cons.Enqueue(data, sequence)
	cons.storeOffset(sequence)
}

func (cons *SystemdConsumer) close() {
	if cons.journal != nil {
		cons.journal.Close()
	}
	cons.WorkerDone()
}

func (cons *SystemdConsumer) read() {
	sendFunction := cons.Enqueue
	if cons.offsetFile != "" {
		sendFunction = cons.enqueueAndPersist
	}

	for cons.IsActive() {
		cons.WaitOnFuse()

		c, err := cons.journal.Next()
		if err != nil {
			Log.Error.Print("Failed to advance journal: ", err)
		} else if c == 0 {
			// reached end of log
			cons.journal.Wait(1 * time.Second)
		} else {
			msg, err := cons.journal.GetDataValue("MESSAGE")
			if err != nil {
				Log.Error.Print("Failed to read journal message: ", err)
			} else {
				offset, err := cons.journal.GetRealtimeUsec()
				if err != nil {
					Log.Error.Print("Failed to read journal realtime: ", err)
				} else {
					sendFunction([]byte(msg), offset)
				}
			}
		}
	}
}

func (cons *SystemdConsumer) Consume(workers *sync.WaitGroup) {
	cons.AddMainWorker(workers)
	go cons.read()
	cons.ControlLoop()
}
