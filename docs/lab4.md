
<!-- TOC -->

- [思路](#思路)
- [shardctrler](#shardctrler)
    - [讨论](#讨论)
    - [实现](#实现)
- [shardkv](#shardkv)
    - [讨论](#讨论-1)
    - [实现](#实现-1)
        - [结构](#结构)
        - [日志类型](#日志类型)
        - [分片结构](#分片结构)
        - [读写操作](#读写操作)
        - [配置更新](#配置更新)
        - [分片迁移](#分片迁移)
        - [分片清理](#分片清理)
        - [空日志检测](#空日志检测)
        - [客户端](#客户端)

<!-- /TOC -->
## 思路

lab4 的内容是要在 lab2 的基础上实现一个 multi-raft 的 KV 存储服务，同时也要支持切片在不同 raft 组上的动态迁移而不违背线性一致性，不过其不需要实现集群的动态伸缩。总体来看，lab4 是一个相对贴近生产场景的 lab。

lab4 应该是 6.824 最难的 lab，如果要为 6.824 的 lab 难度排序的话，我给出的排序是 lab4 > lab2 >> lab3 = lab1。之所以说 lab4 比 lab2 难，主要是因为虽然 raft 实现起来较为琐碎，很难一次写正确，但只要我们对图 2 有着足够的敬畏，再加上阅读 TA 的 guide 和一些相关资料，基本还是能够做出来的。但对于 lab4 来说，基本没有可以参考的相关资料，需要自己动手设计整个迁移流程，期间还要保证线性一致性，这就可能会有很多坑了。对于不熟悉分布式存储的同学来说，很多时候会很痛苦，怎么写都写不对，而且有些 bug 并不是稳定复现的，找起来十分费劲。此外，lab4 的两个 challenge 对代码提出了更高的要求，需要想清楚很多 corner case，基本上一旦最开始不考虑清楚 challenge 的 corner case，实现完就一定会有 bug。

lab4 是我耗时最久的 lab，加起来的总耗时可能都有 10 个工作日了。庆幸的是，我总算啃下了这块硬骨头，能够稳定通过 shardctrler，shardkv 及两个 challenge 的所有测试。在实现过程中，我尽量加了一些注释并不断的调整代码结构，以期望更高的代码可读性。

以下会分别介绍 shardctrler 和 shardkv 的实现细节。

## shardctrler

### 讨论

有关 shardctrler，其实它就是一个高可用的集群配置管理服务。它主要记录了当前每个 raft 组对应的副本数个节点的 endpoint 以及当前每个 shard 被分配到了哪个 raft 组这两个 map。

对于前者，shardctrler 可以通过用户手动或者内置策略自动的方式来增删 raft 组，从而更有效地利用集群的资源。对于后者，客户端的每一次请求都可以通过询问 shardctrler 来路由到对应正确的数据节点，其实有点类似于 HDFS Master 的角色，当然客户端也可以缓存配置来减少 shardctrler 的压力。

在工业界，shardctrler 的角色就类似于 TiDB 的 PD 或者 Kafka 的 ZK，只不过工业界的集群配置管理服务往往更复杂些，一般还要兼顾负载均衡，事务授时等功能。

### 实现

对于 shardctrler 的实现，其实没什么好讲的，基本就是完全照抄 lab3 kvraft 的实现，甚至由于配置一般比较小，甚至都不用实现 raft 的 snapshot，那么 lab3 文档中提到的好几个与 snapshot 相关的 corner case 就都不用考虑了。此外，得益于 raft 层异步 applier 对日志 push channel 的 exactly once 语义，在没有 snapshot 之后，这一层从 channel 中读到的日志可以放心大胆的直接 apply 而不用判断 commandIndex ，这就是将下层写好的收益吧。

此外可能有同学觉得将不同的 rpc 参数都塞到一个结构体中并不是一个好做法，这虽然简化了客户端和服务端的逻辑，但也导致多传输了许多无用数据。对于 6.824 来说，的确是这样，但对于成熟的 rpc 框架例如 gRPC，thrift 等，其字段都可以设置成 optional，在实际传输中，只需要一个 bit 就能够区分是否含有对应字段，这不会是一个影响性能的大问题。因此在我看来，多传输几个 bit 数据带来的弊端远不如简化代码逻辑的收益大。

```Go
type CommandRequest struct {
	Servers   map[int][]string // for Join
	GIDs      []int            // for Leave
	Shard     int              // for Move
	GID       int              // for Move
	Num       int              // for Query
	Op        OperationOp
	ClientId  int64
	CommandId int64
}

type CommandResponse struct {
	Err    Err
	Config Config
}
```

至于有关分区表的四个函数，对于 Query 和 Move 没什么好说的，直接实现就好。对于 Join 和 Leave，由于 shard 是不变的，所以需要在增删 raft 组后需要将 shard 分配地更为均匀且尽量产生较少的迁移任务。对于 Join，可以通过多次平均地方式来达到这个目的：每次选择一个拥有 shard 数最多的 raft 组和一个拥有 shard 数最少的 raft，将前者管理的一个 shard 分给后者，周而复始，直到它们之前的差值小于等于 1 且 0 raft 组无 shard 为止。对于 Leave，如果 Leave 后集群中无 raft 组，则将分片所属 raft 组都置为无效的 0；否则将删除 raft 组的分片均匀地分配给仍然存在的 raft 组。通过这样的分配，可以将 shard 分配地十分均匀且产生了几乎最少的迁移任务。

```Go
func (cf *MemoryConfigStateMachine) Join(groups map[int][]string) Err {
	lastConfig := cf.Configs[len(cf.Configs)-1]
	newConfig := Config{len(cf.Configs), lastConfig.Shards, deepCopy(lastConfig.Groups)}
	for gid, servers := range groups {
		if _, ok := newConfig.Groups[gid]; !ok {
			newServers := make([]string, len(servers))
			copy(newServers, servers)
			newConfig.Groups[gid] = newServers
		}
	}
	s2g := Group2Shards(newConfig)
	for {
		source, target := GetGIDWithMaximumShards(s2g), GetGIDWithMinimumShards(s2g)
		if source != 0 && len(s2g[source])-len(s2g[target]) <= 1 {
			break
		}
		s2g[target] = append(s2g[target], s2g[source][0])
		s2g[source] = s2g[source][1:]
	}
	var newShards [NShards]int
	for gid, shards := range s2g {
		for _, shard := range shards {
			newShards[shard] = gid
		}
	}
	newConfig.Shards = newShards
	cf.Configs = append(cf.Configs, newConfig)
	return OK
}

func (cf *MemoryConfigStateMachine) Leave(gids []int) Err {
	lastConfig := cf.Configs[len(cf.Configs)-1]
	newConfig := Config{len(cf.Configs), lastConfig.Shards, deepCopy(lastConfig.Groups)}
	s2g := Group2Shards(newConfig)
	orphanShards := make([]int, 0)
	for _, gid := range gids {
		if _, ok := newConfig.Groups[gid]; ok {
			delete(newConfig.Groups, gid)
		}
		if shards, ok := s2g[gid]; ok {
			orphanShards = append(orphanShards, shards...)
			delete(s2g, gid)
		}
	}
	var newShards [NShards]int
	// load balancing is performed only when raft groups exist
	if len(newConfig.Groups) != 0 {
		for _, shard := range orphanShards {
			target := GetGIDWithMinimumShards(s2g)
			s2g[target] = append(s2g[target], shard)
		}
		for gid, shards := range s2g {
			for _, shard := range shards {
				newShards[shard] = gid
			}
		}
	}
	newConfig.Shards = newShards
	cf.Configs = append(cf.Configs, newConfig)
	return OK
}
```

需要注意的是，这些命令会在一个 raft 组内的所有节点上执行，因此需要保证同一个命令在不同节点上计算出来的新配置一致，而 go 中 map 的遍历是不确定性的，因此需要稍微注意一下确保相同地命令在不同地节点上能够产生相同的配置。

```Go
func GetGIDWithMinimumShards(s2g map[int][]int) int {
	// make iteration deterministic
	var keys []int
	for k := range s2g {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	// find GID with minimum shards
	index, min := -1, NShards+1
	for _, gid := range keys {
		if gid != 0 && len(s2g[gid]) < min {
			index, min = gid, len(s2g[gid])
		}
	}
	return index
}

func GetGIDWithMaximumShards(s2g map[int][]int) int {
	// always choose gid 0 if there is any
	if shards, ok := s2g[0]; ok && len(shards) > 0 {
		return 0
	}
	// make iteration deterministic
	var keys []int
	for k := range s2g {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	// find GID with maximum shards
	index, max := -1, -1
	for _, gid := range keys {
		if len(s2g[gid]) > max {
			index, max = gid, len(s2g[gid])
		}
	}
	return index
}
```

## shardkv

### 讨论

有关 shardkv，其可以算是一个 multi-raft 的实现，只是缺少了物理节点的抽象概念。在实际的生产系统中，不同 raft 组的成员可能存在于一个物理节点上，而且一般情况下都是一个物理节点拥有一个状态机，不同 raft 组使用不同地命名空间或前缀来操作同一个状态机。基于此，下文所提到的的节点都代指 raft 组的某个成员，而不代指某个物理节点。比如节点宕机代指 raft 组的某个成员被 kill 掉，而不是指某个物理节点宕机，从而可能影响多个 raft 的成员。

有关如何实现 lab4b 以及两个 challenge，可以首先仔细阅读官网给出的[提示](https://pdos.csail.mit.edu/6.824/labs/lab-shard.html)，接着可以参考这篇[博客](https://www.jianshu.com/p/f5c8ab9cd577)，该博客较为详细地介绍了如果从 0 开始实现并完成 lab4b，阅读过后可以了解实现过程中的大部分坑。个人感觉该博客的代码实现还可以再做优化，比如其为不同的 config 都保存了全量数据，这显然是一个生产不可用的设计；再比如其通过在 shardkv 结构体中塞了一堆 map 变量来维护配置变更的状态，其实还可以做得更优雅，可读性更高一些。

我们可以首先明确系统的运行方式：一开始系统会创建一个 shardctrler 组来负责配置更新，分片分配等任务，接着系统会创建多个 raft 组来承载所有分片的读写任务。此外，raft 组增删，节点宕机，节点重启，网络分区等各种情况都可能会出现。

对于集群内部，我们需要保证所有分片能够较为均匀的分配在所有 raft 组上，还需要能够支持动态迁移和容错。

对于集群外部，我们需要向用户保证整个集群表现的像一个永远不会挂的单节点 KV 服务一样，即具有线性一致性。

lab4b 的基本测试要求了上述属性，challenge1 要求及时清理不再属于本分片的数据，challenge2 不仅要求分片迁移时不影响未迁移分片的读写服务，还要求不同地分片数据能够独立迁移，即如果一个配置导致当前 raft 组需要向其他两个 raft 组拉取数据时，即使一个被拉取数据的 raft 组全挂了，也不能导致另一个未挂的被拉取数据的 raft 组分片始终不能在当前 raft 组提供服务。

首先明确三点：
* 所有涉及修改集群分片状态的操作都应该通过 raft 日志的方式去提交，这样才可以保证同一 raft 组内的所有分片数据和状态一致。
* 在 6.824 的框架下，涉及状态的操作都需要 leader 去执行才能保持正确性，否则需要添加一些额外的同步措施，而这显然不是 6.824 所推荐的。因此配置更新，分片迁移，分片清理和空日志检测等逻辑都只能由 leader 去检测并执行。
* 数据迁移的实现为 pull 还是 push？其实都可以，个人感觉难度差不多，这里实现成了 pull 的方式。

接着我们来讨论解决方案：

首先，每个 raft 组的 leader 需要有一个协程去向 shardctrler 定时拉取最新配置，一旦拉取到就需要提交到该 raft 组中以更新配置。此外，为了防止集群的分片状态被覆盖，从而使得某些任务永远被丢弃，因此一旦存在某一分片的状态不是默认状态，配置更新协程就会停止获取和提交新配置直至所有分片的状态都为默认状态为止。

然后，配置在 apply 协程应用时的更新是否预示着其所属分片可以立刻动态的提供服务呢？如果不做 challenge2 那是有可能的，大不了可以阻塞 apply 协程，等到所有数据全拉过来，然后将所有数据进行重放并更新配置。但 challenge2 不仅要求 apply 协程不被阻塞，还要求配置的更新和分片的状态变化彼此独立，所以很显然地一个答案就是我们不仅不能在配置更新时同步阻塞的去拉取数据，也不能异步的去拉取所有数据并当做一条 raft 日志提交，而是应该将不同 raft 组所属的分片数据独立起来，分别提交多条 raft 日志来维护状态。因此，ShardKV 应该对每个分片额外维护其它的一些状态变量。

那么，我们是否可以在 apply 协程更新配置的时候由 leader 异步启动对应的协程，让其独立的根据 raft 组为粒度拉取数据呢？答案也是不可以的，设想这样一个场景：leader apply 了新配置后便挂了，然后此时 follower 也 apply 了该配置但并不会启动该任务，在该 raft 组的新 leader 选出来后，该任务已经无法被执行了。因此，我们不能在 apply 配置的时候启动异步任务，而是应该只更新 shard 的状态，由单独的协程去异步的执行分片迁移，分片清理等任务。当然，为了让单独的协程能够得知找谁去要数据或让谁去删数据，ShardKV 不仅需要维护 currentConfig，还要保留 lastConfig，这样其他协程便能够通过 lastConfig，currentConfig 和所有分片的状态来不遗漏的执行任务。

做完这些之后足够了吗？其实还不够，我们还需要保证集群状态操作的幂等性。举个例子，由于分片迁移协程和 apply 协程是并行的，当 raft 组集体被 kill 并重启时，raft 组首先需要重放所有日志，在重放的过程中，迁移协程可能检测到某个中间状态从而发起数据的迁移，这时如果对数据进行覆盖就会破坏正确性。基于此，我们可以让每个分片迁移和清理请求都携带一个配置版本，保证只有版本相等时才可以覆盖，这样即使重启时迁移协程检测到了中间状态做了重复操作，也不会导致分片状态发生改变，从而保证了幂等性。

听起来是不是已经足够了呢？实际上还不够，考虑这样一个场景：当前 raft 组首先更新了配置，然后迁移协程根据配置从其他 raft 组拉了一个分片的数据和去重表过来并进行了提交，接着系统开始提供服务，这时迁移协程也不会再去重复拉该分片的数据。然而就在这时整个 raft 组被 kill 掉了，重启后 raft 组开始重放日志。当重放到更新配置的日志时，迁移协程恰好捕捉到最新配置的中间状态，并再次向其他 raft 组拉数据和去重表并尝试提交，这样在 apply 该分片更新日志时两者版本一致，从而进行了直接覆盖。这就可能出错了，因为可能在 raft 回放完毕和迁移协程拉到数据并提交日志中间该 raft 组又执行了该分片的写操作，那么直接覆盖就会导致这些改动丢失。因此，仅为分片迁移和清理请求携带配置版本还不完备。在 apply 时，即使配置版本相同也要保证幂等性，这一点可以通过判断分片的状态实现。

最后，在 lab2 的文档中我就提到了 leader 上线后应该立刻 append 一条空日志，这样才可以保证 leader 的状态机最新，然而不幸的是，lab2 的测试在加了空日志后便 Fail 了，因此我便没有再关注。在实现 lab4 时，我最开始并没有关注这件事，最后成了一个大坑，导致我花费了一天的时间才找到问题。该 bug 一般跑 100 次测试能够复现一次，对外的表现是集群出现活锁，无法再服务请求直到超时，而且仅会在几个涉及到重启的测试中出现。经过一番探索，最终发现是在节点频繁的重启过后，出现了 lab2 中描述空日志必要性的例子。这导致某一 raft 组的状态机无法达到最新且不全是默认状态，这使得配置更新协程也无法提交新的配置日志，此时客户端碰巧没有向该 raft 组执行读写请求，因而该 raft 组始终没有当前 term 的日志，从而无法推进 commitIndex，因此整个集群便出现了活锁。该 bug 的解决方法很简单，就是让 raft 层的 leader 在 kv 层周期性的去检测下层是否包含当前 term 的日志，如果没有便 append 一条空日志，这样即可保证新选出的 leader 状态机能够迅速达到最新。其实我认为将空日志检测做到 KV 层并不够优雅，KV 层不需要去了解 raft 层有无空日志会怎么样，更优雅地方式应该是 raft 层的 leader 一上线就提交一个空日志。但总之目前在 6.824 的框架下，也只能在 KV 层做检测了。

讨论清楚这些问题，具体的实现其实就比较朴素了。接下来简单介绍一下实现：

### 实现 

#### 结构

首先给出 shardKV 结构体和 apply 协程的实现，较为干净整洁：

```Go
type ShardKV struct {
	mu      sync.RWMutex
	dead    int32
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	makeEnd func(string) *labrpc.ClientEnd
	gid     int
	sc      *shardctrler.Clerk

	maxRaftState int // snapshot if log grows this big
	lastApplied  int // record the lastApplied to prevent stateMachine from rollback

	lastConfig    shardctrler.Config
	currentConfig shardctrler.Config

	stateMachines  map[int]*Shard                // KV stateMachines
	lastOperations map[int64]OperationContext    // determine whether log is duplicated by recording the last commandId and response corresponding to the clientId
	notifyChans    map[int]chan *CommandResponse // notify client goroutine by applier goroutine to response
}

// a dedicated applier goroutine to apply committed entries to stateMachine, take snapshot and apply snapshot from raft
func (kv *ShardKV) applier() {
	for kv.killed() == false {
		select {
		case message := <-kv.applyCh:
			DPrintf("{Node %v}{Group %v} tries to apply message %v", kv.rf.Me(), kv.gid, message)
			if message.CommandValid {
				kv.mu.Lock()
				if message.CommandIndex <= kv.lastApplied {
					DPrintf("{Node %v}{Group %v} discards outdated message %v because a newer snapshot which lastApplied is %v has been restored", kv.rf.Me(), kv.gid, message, kv.lastApplied)
					kv.mu.Unlock()
					continue
				}
				kv.lastApplied = message.CommandIndex

				var response *CommandResponse
				command := message.Command.(Command)
				switch command.Op {
				case Operation:
					operation := command.Data.(CommandRequest)
					response = kv.applyOperation(&message, &operation)
				case Configuration:
					nextConfig := command.Data.(shardctrler.Config)
					response = kv.applyConfiguration(&nextConfig)
				case InsertShards:
					shardsInfo := command.Data.(ShardOperationResponse)
					response = kv.applyInsertShards(&shardsInfo)
				case DeleteShards:
					shardsInfo := command.Data.(ShardOperationRequest)
					response = kv.applyDeleteShards(&shardsInfo)
				case EmptyEntry:
					response = kv.applyEmptyEntry()
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

接着给出 shardKV 的启动流程，可以看到除了基本地数据初始化外，还会启动 apply 协程，配置更新协程，数据迁移协程，数据清理协程，空日志检测协程。

由于后四个协程都需要 leader 来执行，因此抽象出了一个简单地周期执行函数 `Monitor`。为了防止后四个协程做重复的工作，该四个协程的实现均为同步方式，因而当前一轮任务未完成时不会进行下一轮任务。可以参考之后相应部分的代码。

```Go
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxRaftState int, gid int, ctrlers []*labrpc.ClientEnd, makeEnd func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Command{})
	labgob.Register(CommandRequest{})
	labgob.Register(shardctrler.Config{})
	labgob.Register(ShardOperationResponse{})
	labgob.Register(ShardOperationRequest{})

	applyCh := make(chan raft.ApplyMsg)

	kv := &ShardKV{
		dead:           0,
		rf:             raft.Make(servers, me, persister, applyCh),
		applyCh:        applyCh,
		makeEnd:        makeEnd,
		gid:            gid,
		sc:             shardctrler.MakeClerk(ctrlers),
		lastApplied:    0,
		maxRaftState:   maxRaftState,
		currentConfig:  shardctrler.DefaultConfig(),
		lastConfig:     shardctrler.DefaultConfig(),
		stateMachines:  make(map[int]*Shard),
		lastOperations: make(map[int64]OperationContext),
		notifyChans:    make(map[int]chan *CommandResponse),
	}
	kv.restoreSnapshot(persister.ReadSnapshot())
	// start applier goroutine to apply committed logs to stateMachine
	go kv.applier()
	// start configuration monitor goroutine to fetch latest configuration
	go kv.Monitor(kv.configureAction, ConfigureMonitorTimeout)
	// start migration monitor goroutine to pull related shards
	go kv.Monitor(kv.migrationAction, MigrationMonitorTimeout)
	// start gc monitor goroutine to delete useless shards in remote groups
	go kv.Monitor(kv.gcAction, GCMonitorTimeout)
	// start entry-in-currentTerm monitor goroutine to advance commitIndex by appending empty entries in current term periodically to avoid live locks
	go kv.Monitor(kv.checkEntryInCurrentTermAction, EmptyEntryDetectorTimeout)

	DPrintf("{Node %v}{Group %v} has started", kv.rf.Me(), kv.gid)
	return kv
}

func (kv *ShardKV) Monitor(action func(), timeout time.Duration) {
	for kv.killed() == false {
		if _, isLeader := kv.rf.GetState(); isLeader {
			action()
		}
		time.Sleep(timeout)
	}
}
```

#### 日志类型

为了实现上述题意，我定义了五种类型的日志，这样 apply 协程就可以根据不同地类型来强转 `Data` 来进一步操作：
* Operation：客户端传来的读写操作日志，有 Put，Get，Append 等请求。
* Configuration：配置更新日志，包含一个配置。
* InsertShards：分片更新日志，包含至少一个分片的数据和配置版本。
* DeleteShards：分片删除日志，包含至少一个分片的 id 和配置版本。
* EmptyEntry：空日志，`Data` 为空，使得状态机达到最新。

```Go
type Command struct {
	Op   CommandType
	Data interface{}
}

func (command Command) String() string {
	return fmt.Sprintf("{Type:%v,Data:%v}", command.Op, command.Data)
}

func NewOperationCommand(request *CommandRequest) Command {
	return Command{Operation, *request}
}

func NewConfigurationCommand(config *shardctrler.Config) Command {
	return Command{Configuration, *config}
}

func NewInsertShardsCommand(response *ShardOperationResponse) Command {
	return Command{InsertShards, *response}
}

func NewDeleteShardsCommand(request *ShardOperationRequest) Command {
	return Command{DeleteShards, *request}
}

func NewEmptyEntryCommand() Command {
	return Command{EmptyEntry, nil}
}

type CommandType uint8

const (
	Operation CommandType = iota
	Configuration
	InsertShards
	DeleteShards
	EmptyEntry
)
```

#### 分片结构

每个分片都由一个具体存储数据的哈希表和状态变量构成。可能有同学认为应该将去重表也放到分片结构中去，这样就可以在 raft 组间迁移分片时只传输对应分片去重表的内容。这样的说法的确正确，最开始我的直观想法是让分片结构尽可能地保持干净简单，这样其扩展性更高，所以就没有将去重表放进来。实现完后又仔细想了想，其实将去重表放进来也不会影响扩展性，大不了再抽象一层，即将 KV 的类型再抽象为一个接口，这样其不论是内存哈希表还是 LSM 都可扩展，shard 这一层就负责维护数据的状态，去重表和 KV 接口即可。

总之，目前的实现就是这样，去重表在分片外面会导致分片迁移时多传一些数据，但影响不会很大。

目前每个分片共有 4 种状态：
* Serving：分片的默认状态，如果当前 raft 组在当前 config 下负责管理此分片，则该分片可以提供读写服务，否则该分片暂不可以提供读写服务，但不会阻塞配置更新协程拉取新配置。
* Pulling：表示当前 raft 组在当前 config 下负责管理此分片，暂不可以提供读写服务，需要当前 raft 组从上一个配置该分片所属 raft 组拉数据过来之后才可以提供读写服务，系统会有一个分片迁移协程检测所有分片的 Pulling 状态，接着以 raft 组为单位去对应 raft 组拉取数据，接着尝试重放该分片的所有数据到本地并将分片状态置为 Serving，以继续提供服务。之后的分片迁移部分会介绍得更为详细。 
* BePulling：表示当前 raft 组在当前 config 下不负责管理此分片，不可以提供读写服务，但当前 raft 组在上一个 config 时复制管理此分片，因此当前 config 下负责管理此分片的 raft 组拉取完数据后会向本 raft 组发送分片清理的 rpc，接着本 raft 组将数据清空并重置为 serving 状态即可。之后的分片清理部分会介绍得更为详细。
* GCing：表示当前 raft 组在当前 config 下负责管理此分片，可以提供读写服务，但需要清理掉上一个配置该分片所属 raft 组的数据。系统会有一个分片清理协程检测所有分片的 GCing 状态，接着以 raft 组为单位去对应 raft 组删除数据，一旦远程 raft 组删除数据成功，则本地会尝试将相关分片的状态置为 Serving。之后的分片清理部分会介绍得更为详细。

```Go
type ShardStatus uint8

const (
	Serving ShardStatus = iota
	Pulling
	BePulling
	GCing
)

type Shard struct {
	KV     map[string]string
	Status ShardStatus
}

func NewShard() *Shard {
	return &Shard{make(map[string]string), Serving}
}

func (shard *Shard) Get(key string) (string, Err) {
	if value, ok := shard.KV[key]; ok {
		return value, OK
	}
	return "", ErrNoKey
}

func (shard *Shard) Put(key, value string) Err {
	shard.KV[key] = value
	return OK
}

func (shard *Shard) Append(key, value string) Err {
	shard.KV[key] += value
	return OK
}

func (shard *Shard) deepCopy() map[string]string {
	newShard := make(map[string]string)
	for k, v := range shard.KV {
		newShard[k] = v
	}
	return newShard
}
```

#### 读写操作

可以看到，如果当前 raft 组在当前 config 下负责管理此分片，则只要分片的 status 为 Serving 或 GCing，本 raft 组就可以为该分片提供读写服务，否则返回 ErrWrongGroup 让客户端重新 fecth 最新的 config 并重试即可。

读写操作的基本逻辑和 lab3 一致，可以在向 raft 提交前和 apply 时都检测一遍是否重复以保证线性化语义。

当然，canServe 的判断和去重类似，也需要在向 raft 提交前和 apply 时都检测一遍以保证正确性并尽可能提升性能。

```Go
// check whether this raft group can serve this shard at present
func (kv *ShardKV) canServe(shardID int) bool {
	return kv.currentConfig.Shards[shardID] == kv.gid && (kv.stateMachines[shardID].Status == Serving || kv.stateMachines[shardID].Status == GCing)
}

func (kv *ShardKV) Command(request *CommandRequest, response *CommandResponse) {
	kv.mu.RLock()
	// return result directly without raft layer's participation if request is duplicated
	if request.Op != OpGet && kv.isDuplicateRequest(request.ClientId, request.CommandId) {
		lastResponse := kv.lastOperations[request.ClientId].LastResponse
		response.Value, response.Err = lastResponse.Value, lastResponse.Err
		kv.mu.RUnlock()
		return
	}
	// return ErrWrongGroup directly to let client fetch latest configuration and perform a retry if this key can't be served by this shard at present
	if !kv.canServe(key2shard(request.Key)) {
		response.Err = ErrWrongGroup
		kv.mu.RUnlock()
		return
	}
	kv.mu.RUnlock()
	kv.Execute(NewOperationCommand(request), response)
}

func (kv *ShardKV) applyOperation(message *raft.ApplyMsg, operation *CommandRequest) *CommandResponse {
	var response *CommandResponse
	shardID := key2shard(operation.Key)
	if kv.canServe(shardID) {
		if operation.Op != OpGet && kv.isDuplicateRequest(operation.ClientId, operation.CommandId) {
			DPrintf("{Node %v}{Group %v} doesn't apply duplicated message %v to stateMachine because maxAppliedCommandId is %v for client %v", kv.rf.Me(), kv.gid, message, kv.lastOperations[operation.ClientId], operation.ClientId)
			return kv.lastOperations[operation.ClientId].LastResponse
		} else {
			response = kv.applyLogToStateMachines(operation, shardID)
			if operation.Op != OpGet {
				kv.lastOperations[operation.ClientId] = OperationContext{operation.CommandId, response}
			}
			return response
		}
	}
	return &CommandResponse{ErrWrongGroup, ""}
}
```

#### 配置更新

配置更新协程负责定时检测所有分片的状态，一旦存在至少一个分片的状态不为默认状态，则预示其他协程仍然还没有完成任务，那么此时需要阻塞新配置的拉取和提交。

在 apply 配置更新日志时需要保证幂等性：
* 不同版本的配置更新日志：apply 时仅可逐步递增的去更新配置，否则返回失败。
* 相同版本的配置更新日志：由于配置更新日志仅由配置更新协程提交，而配置更新协程只有检测到比本地更大地配置时才会提交配置更新日志，所以该情形不会出现。

```Go
func (kv *ShardKV) configureAction() {
	canPerformNextConfig := true
	kv.mu.RLock()
	for _, shard := range kv.stateMachines {
		if shard.Status != Serving {
			canPerformNextConfig = false
			DPrintf("{Node %v}{Group %v} will not try to fetch latest configuration because shards status are %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), kv.currentConfig)
			break
		}
	}
	currentConfigNum := kv.currentConfig.Num
	kv.mu.RUnlock()
	if canPerformNextConfig {
		nextConfig := kv.sc.Query(currentConfigNum + 1)
		if nextConfig.Num == currentConfigNum+1 {
			DPrintf("{Node %v}{Group %v} fetches latest configuration %v when currentConfigNum is %v", kv.rf.Me(), kv.gid, nextConfig, currentConfigNum)
			kv.Execute(NewConfigurationCommand(&nextConfig), &CommandResponse{})
		}
	}
}

func (kv *ShardKV) applyConfiguration(nextConfig *shardctrler.Config) *CommandResponse {
	if nextConfig.Num == kv.currentConfig.Num+1 {
		DPrintf("{Node %v}{Group %v} updates currentConfig from %v to %v", kv.rf.Me(), kv.gid, kv.currentConfig, nextConfig)
		kv.updateShardStatus(nextConfig)
		kv.lastConfig = kv.currentConfig
		kv.currentConfig = *nextConfig
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v} rejects outdated config %v when currentConfig is %v", kv.rf.Me(), kv.gid, nextConfig, kv.currentConfig)
	return &CommandResponse{ErrOutDated, ""}
}
```


#### 分片迁移

分片迁移协程负责定时检测分片的 Pulling 状态，利用 lastConfig 计算出对应 raft 组的 gid 和要拉取的分片，然后并行地去拉取数据。

注意这里使用了 waitGroup 来保证所有独立地任务完成后才会进行下一次任务。此外 wg.Wait() 一定要在释放读锁之后，否则无法满足 challenge2 的要求。

在拉取分片的 handler 中，首先仅可由 leader 处理该请求，其次如果发现请求中的配置版本大于本地的版本，那说明请求拉取的是未来的数据，则返回 ErrNotReady 让其稍后重试，否则将分片数据和去重表都深度拷贝到 response 即可。

在 apply 分片更新日志时需要保证幂等性：
* 不同版本的配置更新日志：仅可执行与当前配置版本相同地分片更新日志，否则返回 ErrOutDated。
* 相同版本的配置更新日志：仅在对应分片状态为 Pulling 时为第一次应用，此时覆盖状态机即可并修改状态为 GCing，以让分片清理协程检测到 GCing 状态并尝试删除远端的分片。否则说明已经应用过，直接 break 即可。

```Go
func (kv *ShardKV) migrationAction() {
	kv.mu.RLock()
	gid2shardIDs := kv.getShardIDsByStatus(Pulling)
	var wg sync.WaitGroup
	for gid, shardIDs := range gid2shardIDs {
		DPrintf("{Node %v}{Group %v} starts a PullTask to get shards %v from group %v when config is %v", kv.rf.Me(), kv.gid, shardIDs, gid, kv.currentConfig)
		wg.Add(1)
		go func(servers []string, configNum int, shardIDs []int) {
			defer wg.Done()
			pullTaskRequest := ShardOperationRequest{configNum, shardIDs}
			for _, server := range servers {
				var pullTaskResponse ShardOperationResponse
				srv := kv.makeEnd(server)
				if srv.Call("ShardKV.GetShardsData", &pullTaskRequest, &pullTaskResponse) && pullTaskResponse.Err == OK {
					DPrintf("{Node %v}{Group %v} gets a PullTaskResponse %v and tries to commit it when currentConfigNum is %v", kv.rf.Me(), kv.gid, pullTaskResponse, configNum)
					kv.Execute(NewInsertShardsCommand(&pullTaskResponse), &CommandResponse{})
				}
			}
		}(kv.lastConfig.Groups[gid], kv.currentConfig.Num, shardIDs)
	}
	kv.mu.RUnlock()
	wg.Wait()
}

func (kv *ShardKV) GetShardsData(request *ShardOperationRequest, response *ShardOperationResponse) {
	// only pull shards from leader
	if _, isLeader := kv.rf.GetState(); !isLeader {
		response.Err = ErrWrongLeader
		return
	}
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	defer DPrintf("{Node %v}{Group %v} processes PullTaskRequest %v with response %v", kv.rf.Me(), kv.gid, request, response)

	if kv.currentConfig.Num < request.ConfigNum {
		response.Err = ErrNotReady
		return
	}

	response.Shards = make(map[int]map[string]string)
	for _, shardID := range request.ShardIDs {
		response.Shards[shardID] = kv.stateMachines[shardID].deepCopy()
	}

	response.LastOperations = make(map[int64]OperationContext)
	for clientID, operation := range kv.lastOperations {
		response.LastOperations[clientID] = operation.deepCopy()
	}

	response.ConfigNum, response.Err = request.ConfigNum, OK
}

func (kv *ShardKV) applyInsertShards(shardsInfo *ShardOperationResponse) *CommandResponse {
	if shardsInfo.ConfigNum == kv.currentConfig.Num {
		DPrintf("{Node %v}{Group %v} accepts shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
		for shardId, shardData := range shardsInfo.Shards {
			shard := kv.stateMachines[shardId]
			if shard.Status == Pulling {
				for key, value := range shardData {
					shard.KV[key] = value
				}
				shard.Status = GCing
			} else {
				DPrintf("{Node %v}{Group %v} encounters duplicated shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
				break
			}
		}
		for clientId, operationContext := range shardsInfo.LastOperations {
			if lastOperation, ok := kv.lastOperations[clientId]; !ok || lastOperation.MaxAppliedCommandId < operationContext.MaxAppliedCommandId {
				kv.lastOperations[clientId] = operationContext
			}
		}
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v} rejects outdated shards insertion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
	return &CommandResponse{ErrOutDated, ""}
}
```


#### 分片清理

分片清理协程负责定时检测分片的 GCing 状态，利用 lastConfig 计算出对应 raft 组的 gid 和要拉取的分片，然后并行地去删除分片。

注意这里使用了 waitGroup 来保证所有独立地任务完成后才会进行下一次任务。此外 wg.Wait() 一定要在释放读锁之后，否则无法满足 challenge2 的要求。

在删除分片的 handler 中，首先仅可由 leader 处理该请求，其次如果发现请求中的配置版本小于本地的版本，那说明该请求已经执行过，否则本地的 config 也无法增大，此时直接返回 OK 即可，否则在本地提交一个删除分片的日志。

在 apply 分片删除日志时需要保证幂等性：
* 不同版本的配置更新日志：仅可执行与当前配置版本相同地分片删除日志，否则已经删除过，直接返回 OK 即可。
* 相同版本的配置更新日志：如果分片状态为 GCing，说明是本 raft 组已成功删除远端 raft 组的数据，现需要更新分片状态为默认状态以支持配置的进一步更新；否则如果分片状态为 BePulling，则说明本 raft 组第一次删除该分片的数据，此时直接重置分片即可。否则说明该请求已经应用过，直接 break 返回 OK 即可。

```Go
func (kv *ShardKV) gcAction() {
	kv.mu.RLock()
	gid2shardIDs := kv.getShardIDsByStatus(GCing)
	var wg sync.WaitGroup
	for gid, shardIDs := range gid2shardIDs {
		DPrintf("{Node %v}{Group %v} starts a GCTask to delete shards %v in group %v when config is %v", kv.rf.Me(), kv.gid, shardIDs, gid, kv.currentConfig)
		wg.Add(1)
		go func(servers []string, configNum int, shardIDs []int) {
			defer wg.Done()
			gcTaskRequest := ShardOperationRequest{configNum, shardIDs}
			for _, server := range servers {
				var gcTaskResponse ShardOperationResponse
				srv := kv.makeEnd(server)
				if srv.Call("ShardKV.DeleteShardsData", &gcTaskRequest, &gcTaskResponse) && gcTaskResponse.Err == OK {
					DPrintf("{Node %v}{Group %v} deletes shards %v in remote group successfully when currentConfigNum is %v", kv.rf.Me(), kv.gid, shardIDs, configNum)
					kv.Execute(NewDeleteShardsCommand(&gcTaskRequest), &CommandResponse{})
				}
			}
		}(kv.lastConfig.Groups[gid], kv.currentConfig.Num, shardIDs)
	}
	kv.mu.RUnlock()
	wg.Wait()
}

func (kv *ShardKV) DeleteShardsData(request *ShardOperationRequest, response *ShardOperationResponse) {
	// only delete shards when role is leader
	if _, isLeader := kv.rf.GetState(); !isLeader {
		response.Err = ErrWrongLeader
		return
	}

	defer DPrintf("{Node %v}{Group %v} processes GCTaskRequest %v with response %v", kv.rf.Me(), kv.gid, request, response)

	kv.mu.RLock()
	if kv.currentConfig.Num > request.ConfigNum {
		DPrintf("{Node %v}{Group %v}'s encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, request, kv.currentConfig)
		response.Err = OK
		kv.mu.RUnlock()
		return
	}
	kv.mu.RUnlock()

	var commandResponse CommandResponse
	kv.Execute(NewDeleteShardsCommand(request), &commandResponse)

	response.Err = commandResponse.Err
}

func (kv *ShardKV) applyDeleteShards(shardsInfo *ShardOperationRequest) *CommandResponse {
	if shardsInfo.ConfigNum == kv.currentConfig.Num {
		DPrintf("{Node %v}{Group %v}'s shards status are %v before accepting shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), shardsInfo, kv.currentConfig)
		for _, shardId := range shardsInfo.ShardIDs {
			shard := kv.stateMachines[shardId]
			if shard.Status == GCing {
				shard.Status = Serving
			} else if shard.Status == BePulling {
				kv.stateMachines[shardId] = NewShard()
			} else {
				DPrintf("{Node %v}{Group %v} encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
				break
			}
		}
		DPrintf("{Node %v}{Group %v}'s shards status are %v after accepting shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, kv.getShardStatus(), shardsInfo, kv.currentConfig)
		return &CommandResponse{OK, ""}
	}
	DPrintf("{Node %v}{Group %v}'s encounters duplicated shards deletion %v when currentConfig is %v", kv.rf.Me(), kv.gid, shardsInfo, kv.currentConfig)
	return &CommandResponse{OK, ""}
}
```
#### 空日志检测

分片清理协程负责定时检测 raft 层的 leader 是否拥有当前 term 的日志，如果没有则提交一条空日志，这使得新 leader 的状态机能够迅速达到最新状态，从而避免多 raft 组间的活锁状态。 

```Go
func (kv *ShardKV) checkEntryInCurrentTermAction() {
	if !kv.rf.HasLogInCurrentTerm() {
		kv.Execute(NewEmptyEntryCommand(), &CommandResponse{})
	}
}

func (kv *ShardKV) applyEmptyEntry() *CommandResponse {
	return &CommandResponse{OK, ""}
}
```

#### 客户端

对于 shardKV 的实现，需要满足以下要求：
* 缓存每个分片的 leader。
* rpc 请求成功且服务端返回 OK 或 ErrNoKey，则可直接返回。
* rpc 请求成功且服务端返回 ErrWrongGroup，则需要重新获取最新 config 并再次发送请求。
* rpc 请求失败一次，需要继续遍历该 raft 组的其他节点。
* rpc 请求失败副本数次，此时需要重新获取最新 config 并再次发送请求。

实际上 client 实现的不好也可能导致活锁，有些读写请求会卡在客户端的 for 循环中而无法脱身，因此当出现活锁时，也可以先检查一下客户端是否能够满足以上要求。

```Go
type Clerk struct {
	sm        *shardctrler.Clerk
	config    shardctrler.Config
	makeEnd   func(string) *labrpc.ClientEnd
	leaderIds map[int]int
	clientId  int64 // generated by nrand(), it would be better to use some distributed ID generation algorithm that guarantees no conflicts
	commandId int64 // (clientId, commandId) defines a operation uniquely
}

func MakeClerk(ctrlers []*labrpc.ClientEnd, makeEnd func(string) *labrpc.ClientEnd) *Clerk {
	ck := &Clerk{
		sm:        shardctrler.MakeClerk(ctrlers),
		makeEnd:   makeEnd,
		leaderIds: make(map[int]int),
		clientId:  nrand(),
		commandId: 0,
	}
	ck.config = ck.sm.Query(-1)
	return ck
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

func (ck *Clerk) Command(request *CommandRequest) string {
	request.ClientId, request.CommandId = ck.clientId, ck.commandId
	for {
		shard := key2shard(request.Key)
		gid := ck.config.Shards[shard]
		if servers, ok := ck.config.Groups[gid]; ok {
			if _, ok = ck.leaderIds[gid]; !ok {
				ck.leaderIds[gid] = 0
			}
			oldLeaderId := ck.leaderIds[gid]
			newLeaderId := oldLeaderId
			for {
				var response CommandResponse
				ok := ck.makeEnd(servers[newLeaderId]).Call("ShardKV.Command", request, &response)
				if ok && (response.Err == OK || response.Err == ErrNoKey) {
					ck.commandId++
					return response.Value
				} else if ok && response.Err == ErrWrongGroup {
					break
				} else {
					newLeaderId = (newLeaderId + 1) % len(servers)
					if newLeaderId == oldLeaderId {
						break
					}
					continue
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
		// ask controller for the latest configuration.
		ck.config = ck.sm.Query(-1)
	}
}

```