package kvraft

import (
	"log"
	"sync"
	"sync/atomic"

	"6.824/labgob"
	"6.824/labrpc"
	"6.824/raft"
)

const Debug = true

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug {
		log.Printf(format, a...)
	}
	return
}

type Op struct {
	Key   string
	Value string
	Op    string // "Put" or "Append" or "Get"

	ClientId int64
	Seq      int32
}

/* 需要持久化 */
type ClientsOpRecord struct {
	/* 操作 */
	Op Op
	/* 结果 */
	ResultValue string
	Error       string
}

type ActiveClient struct {
	ClientId int64
	Seq      int32
	NoticeCh chan *ClientsOpRecord
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg
	dead    int32 // set by Kill()

	maxraftstate int // snapshot if log grows this big

	stateMachine    map[string]string         /* 服务器状态机 */
	ClientsOpRecord map[int64]ClientsOpRecord /* 客户端的最后请求的历史记录（后期可以修改为所有的历史记录） */
	activeClients   map[int64]*ActiveClient   /* clientId -> activeClient */

	// clientsReplylog
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	kv.mu.Lock()
	/* 幂等操作处理 */
	if record, ok := kv.ClientsOpRecord[args.ClientId]; ok && record.Op.Seq == args.Seq {
		reply.Error = record.Error
		reply.Value = record.ResultValue
		kv.mu.Unlock()
		return
	}

	op := Op{
		ClientId: args.ClientId,
		Seq:      args.Seq,
		Key:      args.Key,
		Op:       "Get",
	}

	kv.activeClients[args.ClientId] = &ActiveClient{
		ClientId: args.ClientId,
		Seq:      args.Seq,
		NoticeCh: make(chan *ClientsOpRecord, 1),
	}

	noticeCh := kv.activeClients[args.ClientId].NoticeCh

	_, oldTerm, isLeader := kv.rf.Start(op)
	if !isLeader {
		reply.Error = ErrWrongLeader
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()
	/* 等待 raft 处理 */
	select {
	case msg := <-noticeCh:
		kv.mu.Lock()
		defer log.Printf("KV[%d] Get reply %+v", kv.me, reply)
		defer func() {
			// kv.clientsInfo
		}()
		if term, isLeader := kv.rf.GetState(); !isLeader || term != oldTerm {
			reply.Error = ErrWrongLeader
		}

		reply.Error = (string)(msg.Error)
		reply.Value = msg.ResultValue

		kv.mu.Unlock()
		return
		// case timeout
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	kv.mu.Lock()
	/* 幂等操作处理 */
	if record, ok := kv.ClientsOpRecord[args.ClientId]; ok && record.Op.Seq == args.Seq {
		reply.Error = record.Error
		kv.mu.Unlock()
		return
	}

	op := Op{
		ClientId: args.ClientId,
		Seq:      args.Seq,
		Key:      args.Key,
		Value:    args.Value,
		Op:       args.Op,
	}

	kv.activeClients[args.ClientId] = &ActiveClient{
		ClientId: args.ClientId,
		Seq:      args.Seq,
		NoticeCh: make(chan *ClientsOpRecord, 1),
	}

	noticeCh := kv.activeClients[args.ClientId].NoticeCh

	_, oldTerm, isLeader := kv.rf.Start(op)
	if !isLeader {
		log.Printf("KV[%d] want start %v ,but it's not otLeader\n", kv.me, op)
		reply.Error = ErrWrongLeader
		kv.mu.Unlock()
		return
	}
	kv.mu.Unlock()
	/* 等待 raft 处理 */
	select {
	case msg := <-noticeCh:
		defer log.Printf("KV[%d] PutAppend reply %+v", kv.me, reply)
		kv.mu.Lock()
		if term, isLeader := kv.rf.GetState(); !isLeader || term != oldTerm {
			reply.Error = ErrWrongLeader
		}
		reply.Error = (string)(msg.Error)
		kv.mu.Unlock()
		return
		// case timeout
	}
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. for your convenience, we supply
// code to set rf.dead (without needing a lock),
// and a killed() method to test rf.dead in
// long-running loops. you can also add your own
// code to Kill(). you're not required to do anything
// about this, but it may be convenient (for example)
// to suppress debug output from a Kill()ed instance.
//
func (kv *KVServer) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *KVServer) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

//
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
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	kv.stateMachine = make(map[string]string)
	/* 暂且不考虑 kv.ClientsOpRecord 的崩溃恢复，因为日志反正都是会重放的， */
	kv.ClientsOpRecord = make(map[int64]ClientsOpRecord)
	kv.activeClients = make(map[int64]*ActiveClient)

	/* 监听客户端从 applyCh 的提交 */
	go func() {
		for msg := range kv.applyCh {
			log.Printf("KV[%d] applyCh: %+v\n", kv.me, msg)
			if msg.CommandValid {

				cmdOp := msg.Command.(Op)

				switch cmdOp.Op {
				case "Get":
					kv.mu.Lock()
					/* 寻找已经发送的历史记录中是否存在该操作，有则直接返回记录 */
					if record, ok := kv.ClientsOpRecord[cmdOp.ClientId]; ok && record.Op.Seq == cmdOp.Seq {
						if activeClient, ok := kv.activeClients[cmdOp.ClientId]; ok && activeClient.Seq == cmdOp.Seq {
							activeClient.NoticeCh <- &record
						}
					} else {
						/* 否则构造记录 */
						record := ClientsOpRecord{
							Op: cmdOp,
						}
						/* resultValue 和 err */
						if value, ok := kv.stateMachine[cmdOp.Key]; ok {
							record.ResultValue = value
							record.Error = OK
						} else {
							record.Error = ErrNoKey
						}

						kv.ClientsOpRecord[cmdOp.ClientId] = record

						if activeClient, ok := kv.activeClients[cmdOp.ClientId]; ok && activeClient.Seq == cmdOp.Seq {
							activeClient.NoticeCh <- &record
						}
						/* 使用有缓冲区的 channel，
						即便是对面没有人接受也是可以接受的 */
					}
					kv.mu.Unlock()
				case "Put", "Append":
					kv.mu.Lock()
					/* 寻找已经发送的历史记录中是否存在该操作，有则直接返回记录 */
					if record, ok := kv.ClientsOpRecord[cmdOp.ClientId]; ok && record.Op.Seq == cmdOp.Seq {
						if activeClient, ok := kv.activeClients[cmdOp.ClientId]; ok && activeClient.Seq == cmdOp.Seq {
							activeClient.NoticeCh <- &record
						}
					} else {
						/* 否则构造记录 */
						kv.stateMachine[cmdOp.Key] += cmdOp.Value
						record := ClientsOpRecord{
							Op:    cmdOp,
							Error: OK,
						}
						kv.ClientsOpRecord[cmdOp.ClientId] = record

						if activeClient, ok := kv.activeClients[cmdOp.ClientId]; ok && activeClient.Seq == cmdOp.Seq {
							activeClient.NoticeCh <- &record
						}
					}
					kv.mu.Unlock()
				default:
					log.Fatalf("KV[%d] applyCh: unknown command\n", kv.me)
				}
			} else {
				log.Fatalf("KV[%d] applyCh: unknown command\n", kv.me)
			}
		}
	}()
	return kv
}
