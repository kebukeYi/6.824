package shardkv

import (
	"bytes"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"6.824/labgob"
	"6.824/lablog"
	"6.824/labrpc"
	"6.824/labutil"
	"6.824/raft"
	"6.824/shardctrler"
)

const (
	// in each RPC handler, in case of leader lose its term while waiting for applyMsg from applyCh,
	// handler will periodically check leader's currentTerm, to see if term changed
	rpcHandlerCheckRaftTermInterval = 100
	// max interval between snapshoter's checking of RaftStateSize,
	// snapshoter will sleep according to RaftStateSize,
	// if size grows big, sleep less time, and check more quickly,
	// otherwise, sleep more time
	snapshoterCheckInterval = 100
	// in which ratio of RaftStateSize/maxraftstate should KVServer going to take a snapshot
	snapshotThresholdRatio = 0.9
	// after how many ApplyMsgs KVServer received and applied, should KVServer trigger to take a snapshot
	snapshoterAppliedMsgInterval = 50
	serverRefreshConfigInterval  = 100
)

type Op struct {
	Payload interface{}
	// for duplicated op detection
	ClientId int64
	OpId     int
}

func (op Op) String() string {
	switch payload := op.Payload.(type) {
	case GetArgs:
		return fmt.Sprintf("{G%s}", payload.Key)
	case PutAppendArgs:
		if payload.Op == opPut {
			return fmt.Sprintf("{P%s}", payload.Key)
		}
		return fmt.Sprintf("{A%s}", payload.Key)
	case MigrateShardsArgs:
		return fmt.Sprintf("{M%d}", payload.ConfigNum)
	default:
		return ""
	}
}

// channel message from applier to RPC handler,
// and as cached operation result, to respond to duplicated operation
type applyResult struct {
	Err   Err
	Value string
	OpId  int
}

// reply channel from applier to RPC handler, of a commandIndex
type commandEntry struct {
	op      Op
	replyCh chan applyResult
}

// when reconfigure, migrate-out data from my group to other group
type migrateOut struct {
	configNum  int
	shards     []int
	mergedData map[string]string
}

// for a other group, act as a FIFO queue element of migrate-out data
type migrateEntry struct {
	configNum int
	leader    int
	ch        chan migrateOut
}

type ShardKV struct {
	mu                   sync.Mutex
	me                   int
	rf                   *raft.Raft
	sm                   *shardctrler.Clerk // shard manager
	dead                 int32              // set by Kill()
	make_end             func(string) *labrpc.ClientEnd
	gid                  int       // my group id
	maxraftstate         float64   // snapshot if log grows this big
	configFetcherTrigger chan bool // trigger configFetcher to update shard config
	quit                 chan bool // signal to quit

	commandTbl          map[int]commandEntry  // map from commandIndex to commandEntry, maintained by leader, initialized to empty when restart
	appliedCommandIndex int                   // last applied commandIndex from applyCh
	config              shardctrler.Config    // latest known shard config
	migrateTbl          map[int]*migrateEntry // for each other group, map of gid -> migrate-out data

	// need to persist between restart
	Tbl       map[string]string     // key-value table
	ClientTbl map[int64]applyResult // map from clientId to last RPC operation result (for duplicated operation detection)
	ClientId  int64                 // when migrate shards data to other group, act as a ShardKV client, so need a ClientId
	OpId      int                   // also for duplicated migration detection, need an OpId
}

// common logic for RPC handler, with mutex held
// and is RESPONSIBLE for mutex release
//
// 1. check is leader
// 2. check request's configNum
// 3. start Raft consensus
// 4. wait for agreement result from applier goroutine
// 5. return with RPC reply result
func (kv *ShardKV) commonHandler(requestConfigNum int, op Op) (e Err, r string) {
	_, isLeader := kv.rf.GetState()
	if !isLeader {
		// only leader should take responsibility to check request's configNum, then start Raft consensus
		kv.mu.Unlock()
		e = ErrWrongLeader
		return
	}
	if kv.config.Num < requestConfigNum {
		// my shard config seems outdated, tell configFetcher to update
		kv.mu.Unlock()
		select {
		case kv.configFetcherTrigger <- true:
		default:
		}
		// cannot accept request because of unknown config from future, tell client to try later
		e = ErrUnknownConfig
		return
	}

	if kv.config.Num > requestConfigNum {
		// request's config is outdated, abort request, and tell client to update its shard config
		kv.mu.Unlock()
		e = ErrOutdatedConfig
		return
	}

	index, term, isLeader := kv.rf.Start(op)
	if !isLeader {
		kv.mu.Unlock()
		e = ErrWrongLeader
		return
	}

	lablog.ShardDebug(kv.gid, kv.me, lablog.Server, "Start %v@%d, %d/%d", op, index, op.OpId, op.ClientId%100)
	c := make(chan applyResult) // reply channel for applier goroutine
	kv.commandTbl[index] = commandEntry{op: op, replyCh: c}
	kv.mu.Unlock()

CheckTermAndWaitReply:
	for !kv.killed() {
		select {
		case result, ok := <-c:
			if !ok {
				e = ErrShutdown
				return
			}
			// get reply from applier goroutine
			if l := len(result.Value); l > 0 {
				lablog.ShardDebug(kv.gid, kv.me, lablog.Info, "Done %v@%d-> %v", op, index, result.Value[:labutil.Min(4, l)])
			} else {
				lablog.ShardDebug(kv.gid, kv.me, lablog.Info, "Done %v@%d", op, index)
			}
			e = result.Err
			r = result.Value
			return
		case <-time.After(rpcHandlerCheckRaftTermInterval * time.Millisecond):
			t, _ := kv.rf.GetState()
			if term != t {
				e = ErrWrongLeader
				break CheckTermAndWaitReply
			}
		}
	}

	go func() { <-c }() // avoid applier from blocking, and avoid resource leak
	if kv.killed() {
		e = ErrShutdown
	}
	return
}

// Get RPC handler
func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	if kv.killed() {
		reply.Err = ErrShutdown
		return
	}
	// IMPORTANT: lock before rf.Start,
	// to avoid raft finish too quick before kv.commandTbl has set replyCh for this commandIndex
	kv.mu.Lock()
	if kv.config.Num == 0 || kv.config.Shards[key2shard(args.Key)] != kv.gid {
		// no config fetched, or is not responsible for key's shard
		kv.mu.Unlock()
		reply.Err = ErrWrongGroup
		return
	}
	reply.Err, reply.Value = kv.commonHandler(args.ConfigNum, Op{Payload: *args, ClientId: args.ClientId, OpId: args.OpId})
}

// PutAppend RPC handler
func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	if kv.killed() {
		reply.Err = ErrShutdown
		return
	}
	kv.mu.Lock()
	if kv.config.Num == 0 || kv.config.Shards[key2shard(args.Key)] != kv.gid {
		// no config fetched, or is not responsible for key's shard
		kv.mu.Unlock()
		reply.Err = ErrWrongGroup
		return
	}
	reply.Err, _ = kv.commonHandler(args.ConfigNum, Op{Payload: *args, ClientId: args.ClientId, OpId: args.OpId})
}

// MigrateShards RPC handler
func (kv *ShardKV) MigrateShards(args *MigrateShardsArgs, reply *MigrateShardsReply) {
	if kv.killed() {
		reply.Err = ErrShutdown
		return
	}

	kv.mu.Lock()
	lablog.ShardDebug(kv.gid, kv.me, lablog.Migrate, "My CN:%d, get mig@%d, %v", kv.config.Num, args.ConfigNum, args.Shards)
	reply.Err, _ = kv.commonHandler(args.ConfigNum, Op{Payload: *args, ClientId: args.ClientId, OpId: args.OpId})
}

// MigrateShards RPC caller, migrate shards to a group
func (kv *ShardKV) migrateShards(configNum int, gid int, shards []int, data map[string]string) {
	kv.mu.Lock()
	args := &MigrateShardsArgs{
		ConfigNum: configNum,
		Gid:       gid,
		Shards:    shards,
		Data:      data,
		ClientId:  kv.ClientId,
		OpId:      kv.OpId, // opId is fixed for this migration
	}
	kv.OpId++
	kv.mu.Unlock()

	for !kv.killed() {
		_, isLeader := kv.rf.GetState()
		if !isLeader {
			// not leader any more, abort
			return
		}
		kv.mu.Lock()
		if configNum != kv.config.Num {
			// this migration's config is outdated, abort
			return
		}

		servers := kv.config.Groups[gid]
		serverId := kv.migrateTbl[gid].leader
		kv.mu.Unlock()

		for i, nServer := 0, len(servers); i < nServer && !kv.killed(); {
			srv := kv.make_end(servers[serverId])
			reply := &MigrateShardsReply{}
			ok := srv.Call("ShardKV.MigrateShards", args, reply)
			if !ok || reply.Err == ErrWrongLeader || reply.Err == ErrShutdown {
				serverId = (serverId + 1) % nServer
				i++
				continue
			}

			kv.mu.Lock()
			kv.migrateTbl[gid].leader = serverId
			kv.mu.Unlock()
			if reply.Err == ErrUnknownConfig {
				// target server is trying to update shard config, so wait a while and retry this server
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if reply.Err == ErrOutdatedConfig {
				select {
				case kv.configFetcherTrigger <- true:
				default:
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			if reply.Err == OK {
				return // migration done
			}
		}

		// migration not done in this turn, wait a while and retry this group
		time.Sleep(50 * time.Millisecond)
	}
}

// The shardsMigrator go routine act as a long-run goroutine to migrate shards for ONE group,
// it is created when my group try to migrate shards to this group at first time,
// and take migrateOut in FIFO manner through channel for this group
func (kv *ShardKV) shardsMigrator(gid int, ch <-chan migrateOut) {
	for !kv.killed() {
		// new migrateOut need to send to group of gid
		out := <-ch

		kv.mu.Lock()
		if out.configNum != kv.config.Num {
			// this to-send migrateOut's config is outdated, abort it
			kv.mu.Unlock()
			continue
		}
		kv.mu.Unlock()

		lablog.ShardDebug(kv.gid, kv.me, lablog.Migrate, "Start mig->G%d@%d, %v", gid, out.configNum, out.shards)
		// start migrate shards to group of gid
		kv.migrateShards(out.configNum, gid, out.shards, out.mergedData)
	}
}

// when reconfigure, compare oldConfig and newConfig,
// get shards data of each group that needs to migrate out from my group
func (kv *ShardKV) buildMigrateOut(oldConfig shardctrler.Config, newConfig shardctrler.Config) (byGroup map[int]*migrateOut) {
	byGroup = make(map[int]*migrateOut)
	for shard, oldGid := range oldConfig.Shards {
		if newGid := newConfig.Shards[shard]; oldGid == kv.gid && newGid != kv.gid {
			// shard in my group of oldConfig, but not in my group of newConfig,
			// so need to migrate out
			out, ok := byGroup[newGid]
			if !ok {
				out = &migrateOut{configNum: newConfig.Num, shards: make([]int, 0), mergedData: make(map[string]string)}
				byGroup[newGid] = out
			}

			// build migration data for target group
			out.shards = append(out.shards, shard)
			for k, v := range kv.Tbl {
				if key2shard(k) == shard {
					out.mergedData[k] = v
				}
			}
		}
	}
	return
}

// when reconfigure, after buildMigrateOut,
// for each target group, trigger shards migration process,
// by sending migrateOut to the group's shardsMigrator goroutine
func (kv *ShardKV) triggerMigrateShards(gid int, out migrateOut) {
	kv.mu.Lock()
	entry, ok := kv.migrateTbl[gid]
	switch {
	case !ok:
		entry = &migrateEntry{configNum: out.configNum, ch: make(chan migrateOut, 1)}
		kv.migrateTbl[gid] = entry
		// kick-off this group's shardsMigrator
		go kv.shardsMigrator(gid, entry.ch)
	case out.configNum > entry.configNum:
		entry.configNum = out.configNum
	default:
		// out.configNum <= entry.configNum:
		// migrateOut's config is outdated, don't send this migrateOut
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()

	select {
	case entry.ch <- out:
	case <-kv.quit:
	}
}

// install migration's shards data into my kv table
func (kv *ShardKV) installMigration(data map[string]string) {
	for k, v := range data {
		kv.Tbl[k] = v
	}
}

//
// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *ShardKV) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	close(kv.quit)
}

func (kv *ShardKV) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardctrler.
//
// pass ctrlers[] to shardctrler.MakeClerk() so you can send
// RPCs to the shardctrler.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use ctrlers[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartServer(
	servers []*labrpc.ClientEnd,
	me int,
	persister *raft.Persister,
	maxraftstate int,
	gid int,
	ctrlers []*labrpc.ClientEnd,
	make_end func(string) *labrpc.ClientEnd,
) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(GetArgs{})
	labgob.Register(PutAppendArgs{})
	labgob.Register(shardctrler.Config{})
	labgob.Register(MigrateShardsArgs{})

	kv := new(ShardKV)
	kv.me = me
	kv.make_end = make_end
	kv.gid = gid
	kv.maxraftstate = float64(maxraftstate)
	applyCh := make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, applyCh)
	kv.sm = shardctrler.MakeClerk(ctrlers)
	kv.configFetcherTrigger = make(chan bool, 1)
	kv.quit = make(chan bool)

	kv.appliedCommandIndex = kv.rf.LastIncludedIndex
	kv.commandTbl = make(map[int]commandEntry)
	kv.migrateTbl = make(map[int]*migrateEntry)
	kv.Tbl = make(map[string]string)
	kv.ClientTbl = make(map[int64]applyResult)
	kv.ClientId = labutil.Nrand()
	kv.OpId = 1

	// initialize from snapshot persisted before a crash
	kv.readSnapshot(persister.ReadSnapshot())

	// communication between applier and snapshoter,
	// let applier trigger snapshoter to take a snapshot when certain amount of msgs have been applied
	snapshotTrigger := make(chan bool, 1)

	go kv.applier(applyCh, snapshotTrigger, kv.appliedCommandIndex)

	kv.configFetcherTrigger <- true // trigger very first time config fetch
	go kv.configFetcher(kv.configFetcherTrigger)

	go kv.snapshoter(persister, snapshotTrigger)

	return kv
}

// The applier go routine accept applyMsg from applyCh (from underlying raft),
// modify key-value table accordingly,
// reply modified result back to KVServer's RPC handler, if any, through channel identified by commandIndex
// after every snapshoterAppliedMsgInterval msgs, trigger a snapshot
func (kv *ShardKV) applier(applyCh <-chan raft.ApplyMsg, snapshotTrigger chan<- bool, lastSnapshoterTriggeredCommandIndex int) {
	var r string
	var e Err

	for m := range applyCh {
		if m.SnapshotValid {
			// is snapshot, reset kv server state according to this snapshot
			kv.mu.Lock()
			kv.appliedCommandIndex = m.SnapshotIndex
			kv.readSnapshot(m.Snapshot)
			// clear all pending reply channel, to avoid goroutine resource leak
			for _, ce := range kv.commandTbl {
				ce.replyCh <- applyResult{Err: ErrWrongLeader}
			}
			kv.commandTbl = make(map[int]commandEntry)
			kv.mu.Unlock()
			continue
		}

		if !m.CommandValid {
			continue
		}

		if m.CommandIndex-lastSnapshoterTriggeredCommandIndex > snapshoterAppliedMsgInterval {
			// certain amount of msgs have been applied, going to tell snapshoter to take a snapshot
			select {
			case snapshotTrigger <- true:
				lastSnapshoterTriggeredCommandIndex = m.CommandIndex // record as last time triggered commandIndex
			default:
			}
		}

		op := m.Command.(Op)
		kv.mu.Lock()

		kv.appliedCommandIndex = m.CommandIndex

		if op.ClientId == 0 && op.OpId == 0 {
			// internal Raft consensus command, for
			// - shard config agreement
			switch payload := op.Payload.(type) {
			case shardctrler.Config:
				if payload.Num <= kv.config.Num {
					// outdated config, ignore it
					break
				}
				oldConfig := kv.config
				// update my group's shard config
				// it's the ONLY place where shard config can be updated
				kv.config = payload

				_, isLeader := kv.rf.GetState()
				if isLeader && oldConfig.Num > 0 {
					// only leader, and not the initial config update,
					// should my group try to migrate any shards data out
					for gid, out := range kv.buildMigrateOut(oldConfig, payload) {
						go kv.triggerMigrateShards(gid, *out)
					}
				}
			}

			kv.mu.Unlock()
			continue
		}

		lastOpResult := kv.ClientTbl[op.ClientId]
		if lastOpResult.OpId >= op.OpId {
			// detect duplicated operation
			// reply with cached result, don't update kv table
			r, e = lastOpResult.Value, lastOpResult.Err
		} else {
			switch payload := op.Payload.(type) {
			case GetArgs:
				r, e = kv.Tbl[payload.Key], OK
			case PutAppendArgs:
				if payload.Op == opPut {
					kv.Tbl[payload.Key] = payload.Value
				} else {
					kv.Tbl[payload.Key] += payload.Value
				}
				r, e = "", OK
			case MigrateShardsArgs:
				if kv.config.Num > payload.ConfigNum {
					// other group start migrate shards to my group,
					// but when this request reach applier at this point,
					// my group's shard config has been updated,
					// and so the request's migration is outdated, reply the same error
					r, e = "", ErrOutdatedConfig
				} else {
					// migration request accepted, install shards' data from this migration
					kv.installMigration(payload.Data)
					r, e = "", OK
				}
			}

			// cache operation result
			kv.ClientTbl[op.ClientId] = applyResult{Err: e, Value: r, OpId: op.OpId}
		}

		ce, ok := kv.commandTbl[m.CommandIndex]
		if ok {
			delete(kv.commandTbl, m.CommandIndex) // delete won't-use reply channel
		}
		kv.mu.Unlock()

		// only leader server maintains commandTbl, followers just apply kv modification
		if ok {
			if ce.op.ClientId != op.ClientId || ce.op.OpId != op.OpId {
				// Your solution needs to handle a leader that has called Start() for a Clerk's RPC,
				// but loses its leadership before the request is committed to the log.
				// In this case you should arrange for the Clerk to re-send the request to other servers
				// until it finds the new leader.
				//
				// One way to do this is for the server to detect that it has lost leadership,
				// by noticing that a different request has appeared at the index returned by Start()
				ce.replyCh <- applyResult{Err: ErrWrongLeader}
			} else {
				ce.replyCh <- applyResult{Err: e, Value: r}
			}
		}
	}

	// clean all pending RPC handler reply channel, avoid goroutine resource leak
	kv.mu.Lock()
	defer kv.mu.Unlock()
	for _, ce := range kv.commandTbl {
		close(ce.replyCh)
	}
}

// The configFetcher go routine periodically poll the shardctrler,
// to learn about new configurations
func (kv *ShardKV) configFetcher(trigger <-chan bool) {
	for !kv.killed() {
		// get triggered by immediate fetch request,
		// or fetch periodically
		select {
		case _, ok := <-trigger:
			if !ok {
				return
			}
		case <-time.After(serverRefreshConfigInterval * time.Millisecond):
		}

		_, isLeader := kv.rf.GetState()
		if !isLeader {
			// only leader should update shard config
			continue
		}

		config := kv.sm.Query(-1)
		if config.Num >= 0 { // config.Num maybe < 0 when shardctrler not respond for a while
			kv.mu.Lock()
			if config.Num > kv.config.Num {
				// fetch newer config, start Raft consensus to reach agreement and tell all followers
				lablog.ShardDebug(kv.gid, kv.me, lablog.Ctrler, "To update config %d>%d", config.Num, kv.config.Num)
				kv.rf.Start(Op{Payload: config})
			}
			kv.mu.Unlock()
		}
	}
}

// The snapshoter go routine periodically check if raft state size is approaching maxraftstate threshold,
// if so, save a snapshot,
// or, if receive a trigger event from applier, also take a snapshot
func (kv *ShardKV) snapshoter(persister *raft.Persister, snapshotTrigger <-chan bool) {
	if kv.maxraftstate < 0 {
		// no need to take snapshot
		return
	}

	for !kv.killed() {
		ratio := float64(persister.RaftStateSize()) / kv.maxraftstate
		if ratio > snapshotThresholdRatio {
			// is approaching threshold
			kv.mu.Lock()
			if data := kv.kvServerSnapshot(); data == nil {
				lablog.ShardDebug(kv.gid, kv.me, lablog.Error, "Write snapshot failed")
			} else {
				// take a snapshot
				kv.rf.Snapshot(kv.appliedCommandIndex, data)
			}
			kv.mu.Unlock()

			ratio = 0.0
		}

		select {
		// sleep according to current RaftStateSize/maxraftstate ratio
		case <-time.After(time.Duration((1-ratio)*snapshoterCheckInterval) * time.Millisecond):
		// waiting for trigger
		case <-snapshotTrigger:
		}
	}
}

// get KVServer instance state to be snapshotted, with mutex held
func (kv *ShardKV) kvServerSnapshot() []byte {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	if e.Encode(kv.Tbl) != nil ||
		e.Encode(kv.ClientTbl) != nil ||
		e.Encode(kv.ClientId) != nil ||
		e.Encode(kv.OpId) != nil {
		return nil
	}
	return w.Bytes()
}

// restore previously persisted snapshot, with mutex held
func (kv *ShardKV) readSnapshot(data []byte) {
	if len(data) == 0 {
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	var tbl map[string]string
	var clientTbl map[int64]applyResult
	var clientId int64
	var opId int
	if d.Decode(&tbl) != nil ||
		d.Decode(&clientTbl) != nil ||
		d.Decode(&clientId) != nil ||
		d.Decode(&opId) != nil {
		lablog.ShardDebug(kv.gid, kv.me, lablog.Error, "Read broken snapshot")
		return
	}
	kv.Tbl = tbl
	kv.ClientTbl = clientTbl
}
