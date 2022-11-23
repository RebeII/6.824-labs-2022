package kvraft

import (
	"bytes"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"6.824/labgob"
	"6.824/labrpc"
	"6.824/raft"
)

const Debug = false

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

func init() {
	// 获取日志文件句柄
	// 以 只写入文件|没有时创建|文件尾部追加 的形式打开这个文件
	os.Remove(`./log.log`)
	logFile, err := os.OpenFile(`./log.log`, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	// logFile, err := os.OpenFile(`/dev/null`, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}
	// 设置存储位置
	log.SetOutput(logFile)
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	SeqId    int
	Key      string
	Value    string
	ClientId int64
	OpType   string
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	seqMap    map[int64]int     //为了确保seq只执行一次	clientId / seqId
	waitChMap map[int]chan Op   //传递由下层Raft服务的appCh传过来的command	index / chan(Op)
	kvPersist map[string]string // 存储持久化的KV键值对	K / V

	lastIncludeIndex int
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	if kv.killed() {
		reply.Err = ErrWrongLeader
		return
	}
	_, ifLeader := kv.rf.GetState()
	if !ifLeader {
		reply.Err = ErrWrongLeader
		return
	}

	// 通过raft.Start() 提交Op给raft Leader, 然后开启特殊的Wait Channel 等待Raft 返回给自己这个Req结果。
	op := Op{OpType: "Get", Key: args.Key, SeqId: args.SeqId, ClientId: args.ClientId}
	lastIndex, _, _ := kv.rf.Start(op)
	ch := kv.getWaitCh(lastIndex)

	defer func(idx int) {
		kv.mu.Lock()
		delete(kv.waitChMap, idx)
		kv.mu.Unlock()
	}(lastIndex)

	select {
	case <-ch:
		kv.mu.Lock()
		if kv.kvPersist[args.Key] == "" {
			reply.Err = ErrNoKey
			kv.mu.Unlock()
			return
		}
		reply.Err = OK
		reply.Value = kv.kvPersist[args.Key]
		kv.mu.Unlock()
		return
	case <-time.After(REPLY_TIMEOUT * time.Millisecond):
		reply.Err = ErrTimeOut
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	if kv.killed() {
		reply.Err = ErrWrongLeader
		return
	}

	_, ifLeader := kv.rf.GetState()
	if !ifLeader {
		reply.Err = ErrWrongLeader
		return
	}

	// 封装Op传到下层start
	op := Op{OpType: args.Op, Key: args.Key, Value: args.Value, SeqId: args.SeqId, ClientId: args.ClientId}
	lastIndex, _, _ := kv.rf.Start(op)
	ch := kv.getWaitCh(lastIndex)

	defer func(idx int) {
		kv.mu.Lock()
		delete(kv.waitChMap, idx)
		kv.mu.Unlock()
	}(lastIndex)

	select {
	case <-ch:
		reply.Err = OK
	case <-time.After(REPLY_TIMEOUT * time.Millisecond):
		reply.Err = ErrTimeOut
	}
}

// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	// You may need initialization code here.

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	// You may need initialization code here.
	kv.seqMap = make(map[int64]int)
	kv.kvPersist = make(map[string]string)
	kv.waitChMap = make(map[int]chan Op)

	kv.lastIncludeIndex = -1

	snapshot := persister.ReadSnapshot()
	if len(snapshot) > 0 {
		kv.DecodeSnapShot(snapshot)
	}

	go kv.applyMsgHandlerLoop()

	return kv
}

func (kv *KVServer) PersistSnapShot() []byte {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	log.Printf("[log] raftId: %v\nkvPersist: %v\nseqMap: %v\n", kv.rf.GetMe(), kv.kvPersist, kv.seqMap)
	e.Encode(kv.kvPersist)
	e.Encode(kv.seqMap)
	e.Encode(kv.lastIncludeIndex)
	data := w.Bytes()
	return data
}

func (kv *KVServer) DecodeSnapShot(snapshot []byte) {
	if snapshot == nil || len(snapshot) < 1 {
		return
	}
	r := bytes.NewBuffer(snapshot)
	d := labgob.NewDecoder(r)
	var kvPersist map[string]string
	var seqMap map[int64]int
	var lastIncludeIndex int
	if d.Decode(&kvPersist) == nil && d.Decode(&seqMap) == nil && d.Decode(&lastIncludeIndex) != nil {
		kv.kvPersist = kvPersist
		kv.seqMap = seqMap
		kv.lastIncludeIndex = lastIncludeIndex
	} else {
		log.Printf("[Server(%v)] Failed to decode snapshot!!!", kv.me)
	}
}
