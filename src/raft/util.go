package raft

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"time"
)

const (
	// special NULL value for voteFor
	voteForNull = -1
	// election timeout range, in millisecond
	electionTimeoutMax = 1600
	electionTimeoutMin = 800
	// heartbeat interval, in millisecond (8 heartbeat RPCs per second)
	// heartbeat interval should be one order less than election timeout
	heartbeatInterval = 100
)

type serverState string

const (
	Leader    serverState = "L"
	Candidate serverState = "C"
	Follower  serverState = "F"
)

// random range
func nextElectionAlarm() time.Time {
	return time.Now().Add(time.Duration(rand.Intn(electionTimeoutMax-electionTimeoutMin)+electionTimeoutMin) * time.Millisecond)
}

// actual intention name of AppendEntries RPC call
func intentOfAppendEntriesRPC(args *AppendEntriesArgs) string {
	if len(args.Entries) == 0 {
		return "HB"
	}
	return "AE"
}

// Retrieve the verbosity level from an environment variable
// ref: https://blog.josejg.com/debugging-pretty/
func getVerbosity() int {
	v := os.Getenv("VERBOSE")
	level := 0
	if v != "" {
		var err error
		level, err = strconv.Atoi(v)
		if err != nil {
			log.Fatalf("Invalid verbosity %v", v)
		}
	}
	return level
}

type logTopic string

const (
	dClient  logTopic = "CLNT"
	dCommit  logTopic = "CMIT"
	dDrop    logTopic = "DROP"
	dError   logTopic = "ERRO"
	dInfo    logTopic = "INFO"
	dLeader  logTopic = "LEAD"
	dLog     logTopic = "LOG1"
	dLog2    logTopic = "LOG2"
	dPersist logTopic = "PERS"
	dSnap    logTopic = "SNAP"
	dTerm    logTopic = "TERM"
	dTest    logTopic = "TEST"
	dTimer   logTopic = "TIMR"
	dTrace   logTopic = "TRCE"
	dVote    logTopic = "VOTE"
	dWarn    logTopic = "WARN"
)

var debugStart time.Time
var debugVerbosity int

func init() {
	debugVerbosity = getVerbosity()
	debugStart = time.Now()
	// disable datetime logging
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
}

func Debug(rf *Raft, topic logTopic, format string, a ...interface{}) {
	if debugVerbosity >= 1 {
		time := time.Since(debugStart).Microseconds()
		time /= 100
		prefix := fmt.Sprintf("%06d %v ", time, string(topic))
		if rf != nil {
			prefix += fmt.Sprintf("S%d ", rf.me)
		}
		format = prefix + format
		log.Printf(format, a...)
	}
}
