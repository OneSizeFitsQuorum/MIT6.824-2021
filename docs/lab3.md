<!-- TOC -->

- [思路](#思路)
- [实现](#实现)
    - [讨论](#讨论)
    - [客户端](#客户端)
    - [服务端](#服务端)
        - [状态机](#状态机)
        - [处理模型](#处理模型)
        - [日志压缩](#日志压缩)

<!-- /TOC -->

## 思路

lab3 的内容是要在 lab2 的基础上实现一个高可用的 KV 存储服务，算是要将 raft 真正的用起来。

有关此 lab 的实现，raft 作者[博士论文](https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)的第 6.3 小节——实现线性化语义已经做了很详细的介绍，强烈建议在实现之前进行阅读。此外也建议参考 dragonboat 作者对此的[讨论](https://www.zhihu.com/question/278551592)。

以下内容都会假定读者已看过以上两个资料。

相关 RPC 实现如下图：

![](https://user-images.githubusercontent.com/32640567/119603839-900e0180-be20-11eb-9f74-8b39705bc35e.png)

## 实现

### 讨论

考虑这样一个场景，客户端向服务端提交了一条日志，服务端将其在 raft 组中进行了同步并成功 commit，接着在 apply 后返回给客户端执行结果。然而不幸的是，该 rpc 在传输中发生了丢失，客户端并没有收到写入成功的回复。因此，客户端只能进行重试直到明确地写入成功或失败为止，这就可能会导致相同地命令被执行多次，从而违背线性一致性。

有人可能认为，只要写请求是幂等的，那重复执行多次也是可以满足线性一致性的，实际上则不然。考虑这样一个例子：对于一个仅支持 put 和 get 接口的 raftKV 系统，其每个请求都具有幂等性。设 x 的初始值为 0，此时有两个并发客户端，客户端 1 执行 put(x,1)，客户端 2 执行 get(x) 再执行 put(x,2)，问（客户端 2 读到的值，x 的最终值）是多少。对于线性一致的系统，答案可以是 (0,1)，(0,2) 或 (1,2)。然而，如果客户端 1 执行 put 请求时发生了上段描述的情况，然后客户端 2 读到 x 的值为 1 并将 x 置为了 2，最后客户端 1 超时重试且再次将 x 置为 1。对于这种场景，答案是 (1,1)，这就违背了线性一致性。归根究底还是由于幂等的 put(x,1) 请求在状态机上执行了两次，有两个 LZ 点。因此，即使写请求的业务语义能够保证幂等，不进行额外的处理让其重复执行多次也会破坏线性一致性。当然，读请求由于不改变系统的状态，重复执行多次是没问题的。

对于这个问题，raft 作者介绍了想要实现线性化语义，就需要保证日志仅被执行一次，即它可以被 commit 多次，但一定只能 apply 一次。其解决方案原文如下：
> The solution is for clients to assign unique serial numbers to every command. Then, the state machine tracks the latest serial number processed for each client, along with the associated response. If it receives a command whose serial number has already been executed, it responds immediately without re-executing the request.

基本思路便是：
* 每个 client 都需要一个唯一的标识符，它的每个不同命令需要有一个顺序递增的 commandId，clientId 和这个 commandId，clientId 可以唯一确定一个不同的命令，从而使得各个 raft 节点可以记录保存各命令是否已应用以及应用以后的结果。

为什么要记录应用的结果？因为通过这种方式同一个命令的多次 apply 最终只会实际应用到状态机上一次，之后相同命令 apply 的时候实际上是不应用到状态机上的而是直接返回的，那么这时候应该返回什么呢？直接返回成功吗？不行，如果第一次应用时状态机报了什么例如 key not exist 等业务上的错而没有被记录，之后就很难捕捉到这个执行结果了，所以也需要将应用结果保存下来。

如果默认一个客户端只能串行执行请求的话，服务端这边只需要记录一个 map，其 key 是 clientId，其 value 是该 clientId 执行的最后一条日志的 commandId 和状态机的输出即可。

raft 论文中还考虑了对这个 map 进行一定大小的限制，防止其无线增长。这就带来了两个问题：
* 集群间的不同节点如何就某个 clientId 过期达成共识。
* 不小心驱逐了活跃的 clientId 怎么办，其之后不论是新建一个 clientId 还是复用之前的 clientId 都可能导致命令的重执行。

这些问题在工程实现上都较为麻烦。比如后者如果业务上是事务那直接 abort 就行，但如果不是事务就很难办了。

实际上，个人感觉 clientId 是与 session 绑定的，其生命周期应该与 session 一致，开启 session 时从 map 中保存该 clientId，关闭 session 时从 map 中删除该 clientId 及其对应的 value 即可。map 中一个 clientId 对应的内存占用可能都不足 30 字节，相比携带更多业务语义的 session 其实很小，所以感觉没太大必要去严格控制该 map 的内存占用，还不如考虑下怎么控制 session 更大地内存占用呢。这样就不用去纠结前面提到的两个问题了。

### 客户端

前面提到要用 (clientId,commandId) 来唯一的标识一个客户端。

对于前者，在目前的实现中，客户端的 clientId 生成方式较为简陋，基本上是从 [0,1 << 62] 的区间中随机生成数来实现的，这样的实现方式虽然重复概率也可以忽略不计，但不是很优雅。更优雅地方式应该是采用一些分布式 id 生成算法，例如 snowflake 等，具体可以参考此[博客](https://zhuanlan.zhihu.com/p/107939861)。当然，由于 clientId 只需要保证唯一即可，不需要保证递增，使用更简单的 uuid 其实也可以。不过由于 go 的标准库并无内置的 uuid 实现，且 6.824 又要求不能更改 go mod 文件引入新的包，所以这里就直接用它默认的 nrand() 函数了。

对于后者，一个 client 可以通过为其处理的每条命令递增 commandId 的方式来确保不同的命令一定有不同的 commandId，当然，同一条命令的 commandId 在没有处理完毕之前，即明确收到服务端的写入成功或失败之前是不能改变的。

代码如下：

```Go
func MakeClerk(servers []*labrpc.ClientEnd) *Clerk {
	return &Clerk{
		servers:   servers,
		leaderId:  0,
		clientId:  nrand(),
		commandId: 0,
	}
}

func (ck *Clerk) Get(key string) string {
	return ck.Command(&CommandRequest{Key: key, Op: OpGet})
}

func (ck *Clerk) Put(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpPut})
}
func (ck *Clerk) Append(key string, value string) {
	ck.Command(&CommandRequest{Key: key, Value: value, Op: OpAppend})
}

//
//
// you can send an RPC with code like this:
// ok := ck.servers[i].Call("KVServer.Command", &request, &response)
//
// the types of args and reply (including whether they are pointers)
// must match the declared types of the RPC handler function's
// arguments. and reply must be passed as a pointer.
//
func (ck *Clerk) Command(request *CommandRequest) string {
	request.ClientId, request.CommandId = ck.clientId, ck.commandId
	for {
		var response CommandResponse
		if !ck.servers[ck.leaderId].Call("KVServer.Command", request, &response) || response.Err == ErrWrongLeader || response.Err == ErrTimeout {
			ck.leaderId = (ck.leaderId + 1) % int64(len(ck.servers))
			continue
		}
		ck.commandId++
		return response.Value
	}
}
```

### 服务端

服务端的实现的结构体如下：

```Go
type KVServer struct {
	mu      sync.Mutex
	dead    int32
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	maxRaftState int // snapshot if log grows this big
	lastApplied  int // record the lastApplied to prevent stateMachine from rollback

	stateMachine   KVStateMachine                // KV stateMachine
	lastOperations map[int64]OperationContext    // determine whether log is duplicated by recording the last commandId and response corresponding to the clientId
	notifyChans    map[int]chan *CommandResponse // notify client goroutine by applier goroutine to response
}
```

以下分三点依次介绍：

#### 状态机
为了方便扩展，我抽象出了 KVStateMachine 的接口，并实现了最简单的内存版本的 KV 状态机 MemoryKV。

实际上在生产级别的 KV 服务中，数据不可能全存在内存中，系统往往采用的是 LSM 的架构，例如 RocksDB 等。

```Go
type KVStateMachine interface {
	Get(key string) (string, Err)
	Put(key, value string) Err
	Append(key, value string) Err
}

type MemoryKV struct {
	KV map[string]string
}

func NewMemoryKV() *MemoryKV {
	return &MemoryKV{make(map[string]string)}
}

func (memoryKV *MemoryKV) Get(key string) (string, Err) {
	if value, ok := memoryKV.KV[key]; ok {
		return value, OK
	}
	return "", ErrNoKey
}

func (memoryKV *MemoryKV) Put(key, value string) Err {
	memoryKV.KV[key] = value
	return OK
}

func (memoryKV *MemoryKV) Append(key, value string) Err {
	memoryKV.KV[key] += value
	return OK
}
```

#### 处理模型

对于 raft 的日志序列，状态机需要按序 apply 才能保证不同节点上数据的一致性，这也是 RSM 模型的原理。因此，在实现中一定得有一个单独的 apply 协程去顺序的 apply 日志到状态机中去。

对于客户端的请求，rpc 框架也会生成一个协程去处理逻辑。因此，需要考虑清楚这些协程之间的通信关系。

为此，我的实现是客户端协程将日志放入 raft 层去同步后即注册一个 channel 去阻塞等待，接着 apply 协程监控 applyCh，在得到 raft 层已经 commit 的日志后，apply 协程首先将其 apply 到状态机中，接着根据 index 得到对应的 channel ，最后将状态机执行的结果 push 到 channel 中，这使得客户端协程能够解除阻塞并回复结果给客户端。对于这种只需通知一次的场景，这里使用 channel 而不是 cond 的原因是理论上一条日志被路由到 raft 层同步后，客户端协程拿锁注册 notifyChan 和 apply 协程拿锁执行该日志再进行 notify 之间的拿锁顺序无法绝对保证，虽然直观上感觉应该一定是前者先执行，但如果是后者先执行了，那前者对于 cond 变量的 wait 就永远不会被唤醒了，那情况就有点糟糕了。

在目前的实现中，读请求也会生成一条 raft 日志去同步，这样可以以最简单的方式保证线性一致性。当然，这样子实现的读性能会相当的差，实际生产级别的 raft 读请求实现一般都采用了 Read Index 或者 Lease Read 的方式，具体原理可以参考此[博客](https://tanxinyu.work/consistency-and-consensus/#etcd-%E7%9A%84-Raft)，具体实现可以参照 SOFAJRaft 的实现[博客](https://www.sofastack.tech/blog/sofa-jraft-linear-consistent-read-implementation/)。

```Go
func (kv *KVServer) Command(request *CommandRequest, response *CommandResponse) {
	defer DPrintf("{Node %v} processes CommandRequest %v with CommandResponse %v", kv.rf.Me(), request, response)
	// return result directly without raft layer's participation if request is duplicated
	kv.mu.RLock()
	if request.Op != OpGet && kv.isDuplicateRequest(request.ClientId, request.CommandId) {
		lastResponse := kv.lastOperations[request.ClientId].LastResponse
		response.Value, response.Err = lastResponse.Value, lastResponse.Err
		kv.mu.RUnlock()
		return
	}
	kv.mu.RUnlock()
	// do not hold lock to improve throughput
	// when KVServer holds the lock to take snapshot, underlying raft can still commit raft logs
	index, _, isLeader := kv.rf.Start(Command{request})
	if !isLeader {
		response.Err = ErrWrongLeader
		return
	}
	kv.mu.Lock()
	ch := kv.getNotifyChan(index)
	kv.mu.Unlock()
	select {
	case result := <-ch:
		response.Value, response.Err = result.Value, result.Err
	case <-time.After(ExecuteTimeout):
		response.Err = ErrTimeout
	}
	// release notifyChan to reduce memory footprint
	// why asynchronously? to improve throughput, here is no need to block client request
	go func() {
		kv.mu.Lock()
		kv.removeOutdatedNotifyChan(index)
		kv.mu.Unlock()
	}()
}

// a dedicated applier goroutine to apply committed entries to stateMachine, take snapshot and apply snapshot from raft
func (kv *KVServer) applier() {
	for kv.killed() == false {
		select {
		case message := <-kv.applyCh:
			DPrintf("{Node %v} tries to apply message %v", kv.rf.Me(), message)
			if message.CommandValid {
				kv.mu.Lock()
				if message.CommandIndex <= kv.lastApplied {
					DPrintf("{Node %v} discards outdated message %v because a newer snapshot which lastApplied is %v has been restored", kv.rf.Me(), message, kv.lastApplied)
					kv.mu.Unlock()
					continue
				}
				kv.lastApplied = message.CommandIndex

				var response *CommandResponse
				command := message.Command.(Command)
				if command.Op != OpGet && kv.isDuplicateRequest(command.ClientId, command.CommandId) {
					DPrintf("{Node %v} doesn't apply duplicated message %v to stateMachine because maxAppliedCommandId is %v for client %v", kv.rf.Me(), message, kv.lastOperations[command.ClientId], command.ClientId)
					response = kv.lastOperations[command.ClientId].LastResponse
				} else {
					response = kv.applyLogToStateMachine(command)
					if command.Op != OpGet {
						kv.lastOperations[command.ClientId] = OperationContext{command.CommandId, response}
					}
				}

				// only notify related channel for currentTerm's log when node is leader
				if currentTerm, isLeader := kv.rf.GetState(); isLeader && message.CommandTerm == currentTerm {
					ch := kv.getNotifyChan(message.CommandIndex)
					ch <- response
				}

				needSnapshot := kv.needSnapshot()
				if needSnapshot {
					kv.takeSnapshot(message.CommandIndex)
				}
				kv.mu.Unlock()
			} else if message.SnapshotValid {
				kv.mu.Lock()
				if kv.rf.CondInstallSnapshot(message.SnapshotTerm, message.SnapshotIndex, message.Snapshot) {
					kv.restoreSnapshot(message.Snapshot)
					kv.lastApplied = message.SnapshotIndex
				}
				kv.mu.Unlock()
			} else {
				panic(fmt.Sprintf("unexpected Message %v", message))
			}
		}
	}
}
```

需要注意的有以下几点：
* 提交日志时不持 kvserver 的锁：当有多个客户端并发向 kvserver 提交日志时，kvserver 需要将其下推到 raft 层去做共识。一方面，raft 层的 Start() 函数已经能够保证并发时的正确性；另一方面，kvserver 在生成 snapshot 时需要持锁，此时没有必要去阻塞 raft 层继续同步日志。综合这两个原因，将请求下推到 raft 层做共识时，最好不要加 kvserver 的锁，这一定程度上能提升性能。
* 客户端协程阻塞等待结果时超时返回：为了避免客户端发送到少数派 leader 后被长时间阻塞，其在交给 leader 处理后阻塞时需要考虑超时，一旦超时就返回客户端让其重试。
* apply 日志时需要防止状态机回退：lab2 的文档已经介绍过, follower 对于 leader 发来的 snapshot 和本地 commit 的多条日志，在向 applyCh 中 push 时无法保证原子性，可能会有 snapshot 夹杂在多条 commit 的日志中，如果在 kvserver 和 raft 模块都原子性更换状态之后，kvserver 又 apply 了过期的 raft 日志，则会导致节点间的日志不一致。因此，从 applyCh 中拿到一个日志后需要保证其 index 大于等于 lastApplied 才可以应用到状态机中。
* 对非读请求去重：对于写请求，由于其会改变系统状态，因此在执行到状态机之前需要去重，仅对不重复的日志进行 apply 并记录执行结果，保证其仅被执行一次。对于读请求，由于其不影响系统状态，所以直接去状态机执行即可，当然，其结果也不需要再记录到去重的数据结构中。
* 被 raft 层同步前尝试去重：对于写请求，在未调用 Start 函数即未被 raft 层同步时可以先进行一次检验，如果重复则可以直接返回上次执行的结果而不需要利用 raft 层进行同步，因为同步完成后的结果也是一样。当然，即使该写请求重复，但此时去重表中可能暂还不能判断其是否重复，此时便让其在 raft 层进行同步并在 apply 时去重即可。
* 当前 term 无提交日志时不能服务读请求：有关这个 liveness 的问题，lab2 文档的最后已经介绍过了，是需要通过 leader 上线后立即 append 一条空日志来回避的。本文档的 RPC 实现图中也进行了描述：对于查询请求，如果 leader 没有当前任期已提交日志的话，其是不能服务读请求的，因为这时候 leader 的状态机可能并不足够新，服务读请求可能会违背线性一致性。其实更准确地说，只要保证状态机应用了之前 term 的所有日志就可以提供服务。由于目前的实现中读请求是按照一条 raft 日志来实现的，所以对于当前 leader 当选后的读请求，由于 apply 的串行性，其走到 apply 那一步时已经确保了之前任期的日志都已经 apply 到了状态机中，那么此时服务读请求是没有问题的。在实际生产级别的 raft 实现中， raft 读请求肯定不是通过日志来实现的，因此需要仔细考虑此处并进行必要的阻塞。对于他们，更优雅一些的做法还是 leader 一上线就 append 一条空日志，这样其新 leader 读服务不可用的区间会大幅度减少。
* 仅对 leader 的 notifyChan 进行通知：目前的实现中读写请求都需要路由给 leader 去处理，所以在执行日志到状态机后，只有 leader 需将执行结果通过 notifyChan 唤醒阻塞的客户端协程，而 follower 则不需要；对于 leader 降级为 follower 的情况，该节点在 apply 日志后也不能对之前靠 index 标识的 channel 进行 notify，因为可能执行结果是不对应的，所以在加上只有 leader 可以 notify 的判断后，对于此刻还阻塞在该节点的客户端协程，就可以让其自动超时重试。如果读者足够细心，也会发现这里的机制依然可能会有问题，下一点会提到。
* 仅对当前 term 日志的 notifyChan 进行通知：上一点提到，对于 leader 降级为 follower 的情况，该节点需要让阻塞的请求超时重试以避免违反线性一致性。那么有没有这种可能呢？leader 降级为 follower 后又迅速重新当选了 leader，而此时依然有客户端协程未超时在阻塞等待，那么此时 apply 日志后，根据 index 获得 channel 并向其中 push 执行结果就可能出错，因为可能并不对应。对于这种情况，最直观地解决方案就是仅对当前 term 日志的 notifyChan 进行通知，让之前 term 的客户端协程都超时重试即可。当然，目前客户端超时重试的时间是 500ms，选举超时的时间是 1s，所以严格来说并不会出现这种情况，但是为了代码的鲁棒性，最好还是这么做，否则以后万一有人将客户端超时时间改到 5s 就可能出现这种问题了。

#### 日志压缩

首先，日志的 snapshot 不仅需要包含状态机的状态，还需要包含用来去重的 lastOperations 哈希表。

其次，apply 协程负责持锁阻塞式的去生成 snapshot，幸运的是，此时 raft 框架是不阻塞的，依然可以同步并提交日志，只是不 apply 而已。如果这里还想进一步优化的话，可以将状态机搞成 MVCC 等能够 COW 的机制，这样应该就可以不阻塞状态机的更新了。