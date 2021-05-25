<!-- TOC -->

- [思路](#思路)
- [实现](#实现)
    - [结构体](#结构体)
    - [选主](#选主)
        - [handler](#handler)
        - [sender](#sender)
    - [日志复制](#日志复制)
        - [handler](#handler-1)
        - [sender](#sender-1)
        - [复制模型](#复制模型)
    - [日志压缩](#日志压缩)
        - [服务端触发的日志压缩](#服务端触发的日志压缩)
        - [leader 发送来的 InstallSnapshot](#leader-发送来的-installsnapshot)
        - [异步 applier 的 exactly once](#异步-applier-的-exactly-once)
    - [持久化](#持久化)
- [问题](#问题)
    - [问题 1：有关 safety](#问题-1有关-safety)
    - [问题 2：有关 liveness](#问题-2有关-liveness)

<!-- /TOC -->

## 思路
lab2 的内容是要实现一个除了节点变更功能外的 raft 算法，还是比较有趣的。

有关 go 实现 raft 的种种坑，可以首先参考 6.824 课程对 [locking](https://pdos.csail.mit.edu/6.824/labs/raft-locking.txt) 和 [structure](https://pdos.csail.mit.edu/6.824/labs/raft-structure.txt) 的描述，然后再参考 6.824 TA 的 [guidance](https://thesquareplanet.com/blog/students-guide-to-raft/) 。写之前一定要看看这三篇博客，否则很容易被 bug 包围。

另外，raft 论文的图 2 也很关键，一定要看懂其中的每一个描述。

![](https://user-images.githubusercontent.com/32640567/116203223-0bbb5680-a76e-11eb-8ccd-4ef3f1006fb3.png)

下面会简单介绍我实现的 raft，我个人对于代码结构还是比较满意的。 

## 实现

### 结构体
我的 raft 结构体写得较为干净，其基本都是图 2 中介绍到的字段或者 lab 自带的字段。也算是为了致敬 raft 论文吧，希望能够提升代码可读性。

```Go
type Raft struct {
	mu        sync.RWMutex        // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	applyCh        chan ApplyMsg
	applyCond      *sync.Cond   // used to wakeup applier goroutine after committing new entries
	replicatorCond []*sync.Cond // used to signal replicator goroutine to batch replicating entries
	state          NodeState

	currentTerm int
	votedFor    int
	logs        []Entry // the first entry is a dummy entry which contains LastSnapshotTerm, LastSnapshotIndex and nil Command

	commitIndex int
	lastApplied int
	nextIndex   []int
	matchIndex  []int

	electionTimer  *time.Timer
	heartbeatTimer *time.Timer
}

func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{
		peers:          peers,
		persister:      persister,
		me:             me,
		dead:           0,
		applyCh:        applyCh,
		replicatorCond: make([]*sync.Cond, len(peers)),
		state:          StateFollower,
		currentTerm:    0,
		votedFor:       -1,
		logs:           make([]Entry, 1),
		nextIndex:      make([]int, len(peers)),
		matchIndex:     make([]int, len(peers)),
		heartbeatTimer: time.NewTimer(StableHeartbeatTimeout()),
		electionTimer:  time.NewTimer(RandomizedElectionTimeout()),
	}
	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())
	rf.applyCond = sync.NewCond(&rf.mu)
	lastLog := rf.getLastLog()
	for i := 0; i < len(peers); i++ {
		rf.matchIndex[i], rf.nextIndex[i] = 0, lastLog.Index+1
		if i != rf.me {
			rf.replicatorCond[i] = sync.NewCond(&sync.Mutex{})
			// start replicator goroutine to replicate entries in batch
			go rf.replicator(i)
		}
	}
	// start ticker goroutine to start elections
	go rf.ticker()
	// start applier goroutine to push committed logs into applyCh exactly once
	go rf.applier()

	return rf
}
```

此外其有若干个后台协程，它们分别是：
* ticker：共 1 个，用来触发 heartbeat timeout 和 election timeout
* applier：共 1 个，用来往 applyCh 中 push 提交的日志并保证 exactly once
* replicator：共 len(peer) - 1 个，用来分别管理对应 peer 的复制状态

### 选主
选主的实现较为简单。

#### handler
基本参照图 2 的描述实现即可。需要注意的是只有在 grant 投票时才重置选举超时时间，这样有助于网络不稳定条件下选主的 liveness 问题，这一点 guidance 里面有介绍到。

```Go
func (rf *Raft) RequestVote(request *RequestVoteRequest, response *RequestVoteResponse) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer rf.persist()
	defer DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} before processing requestVoteRequest %v and reply requestVoteResponse %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), request, response)

	if request.Term < rf.currentTerm || (request.Term == rf.currentTerm && rf.votedFor != -1 && rf.votedFor != request.CandidateId) {
		response.Term, response.VoteGranted = rf.currentTerm, false
		return
	}
	if request.Term > rf.currentTerm {
		rf.ChangeState(StateFollower)
		rf.currentTerm, rf.votedFor = request.Term, -1
	}
	if !rf.isLogUpToDate(request.LastLogTerm, request.LastLogIndex) {
		response.Term, response.VoteGranted = rf.currentTerm, false
		return
	}
	rf.votedFor = request.CandidateId
	rf.electionTimer.Reset(RandomizedElectionTimeout())
	response.Term, response.VoteGranted = rf.currentTerm, true
}
```

#### sender
ticker 协程会定期收到两个 timer 的到期事件，如果是 election timer 到期，则发起一轮选举；如果是 heartbeat timer 到期且节点是 leader，则发起一轮心跳。

```Go
func (rf *Raft) ticker() {
	for rf.killed() == false {
		select {
		case <-rf.electionTimer.C:
			rf.mu.Lock()
			rf.ChangeState(StateCandidate)
			rf.currentTerm += 1
			rf.StartElection()
			rf.electionTimer.Reset(RandomizedElectionTimeout())
			rf.mu.Unlock()
		case <-rf.heartbeatTimer.C:
			rf.mu.Lock()
			if rf.state == StateLeader {
				rf.BroadcastHeartbeat(true)
				rf.heartbeatTimer.Reset(StableHeartbeatTimeout())
			}
			rf.mu.Unlock()
		}
	}
}

func (rf *Raft) StartElection() {
	request := rf.genRequestVoteRequest()
	DPrintf("{Node %v} starts election with RequestVoteRequest %v", rf.me, request)
	// use Closure
	grantedVotes := 1
	rf.votedFor = rf.me
	rf.persist()
	for peer := range rf.peers {
		if peer == rf.me {
			continue
		}
		go func(peer int) {
			response := new(RequestVoteResponse)
			if rf.sendRequestVote(peer, request, response) {
				rf.mu.Lock()
				defer rf.mu.Unlock()
				DPrintf("{Node %v} receives RequestVoteResponse %v from {Node %v} after sending RequestVoteRequest %v in term %v", rf.me, response, peer, request, rf.currentTerm)
				if rf.currentTerm == request.Term && rf.state == StateCandidate {
					if response.VoteGranted {
						grantedVotes += 1
						if grantedVotes > len(rf.peers)/2 {
							DPrintf("{Node %v} receives majority votes in term %v", rf.me, rf.currentTerm)
							rf.ChangeState(StateLeader)
							rf.BroadcastHeartbeat(true)
						}
					} else if response.Term > rf.currentTerm {
						DPrintf("{Node %v} finds a new leader {Node %v} with term %v and steps down in term %v", rf.me, peer, response.Term, rf.currentTerm)
						rf.ChangeState(StateFollower)
						rf.currentTerm, rf.votedFor = response.Term, -1
						rf.persist()
					}
				}
			}
		}(peer)
	}
}
```
需要注意的有以下几点：
* 并行异步投票：发起投票时要异步并行去发起投票，从而不阻塞 ticker 协程，这样 candidate 再次 election timeout 之后才能自增 term 继续发起新一轮选举。
* 投票统计：可以在函数内定义一个变量并利用 go 的闭包来实现，也可以在结构体中维护一个 votes 变量来实现。为了 raft 结构体更干净，我选择了前者。
* 抛弃过期请求的回复：对于过期请求的回复，直接抛弃就行，不要做任何处理，这一点 guidance 里面也有介绍到。

### 日志复制

日志复制是 raft 算法的核心，也是 corner case 最多的地方。此外，在引入日志压缩之后，情况会更加复杂，因此需要仔细去应对。在实现此功能时，几乎不可避免会写出 bug 来，因此建议在函数入口或出口处打全日志，这样方便以后 debug。

#### handler
可以看到基本都是按照图 2 中的伪代码实现的，此外加上了 6.824 的加速解决节点间日志冲突的优化。

```Go
func (rf *Raft) AppendEntries(request *AppendEntriesRequest, response *AppendEntriesResponse) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer rf.persist()
	defer DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} before processing AppendEntriesRequest %v and reply AppendEntriesResponse %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), request, response)

	if request.Term < rf.currentTerm {
		response.Term, response.Success = rf.currentTerm, false
		return
	}

	if request.Term > rf.currentTerm {
		rf.currentTerm, rf.votedFor = request.Term, -1
	}

	rf.ChangeState(StateFollower)
	rf.electionTimer.Reset(RandomizedElectionTimeout())

	if request.PrevLogIndex < rf.getFirstLog().Index {
		response.Term, response.Success = 0, false
		DPrintf("{Node %v} receives unexpected AppendEntriesRequest %v from {Node %v} because prevLogIndex %v < firstLogIndex %v", rf.me, request, request.LeaderId, request.PrevLogIndex, rf.getFirstLog().Index)
		return
	}

	if !rf.matchLog(request.PrevLogTerm, request.PrevLogIndex) {
		response.Term, response.Success = rf.currentTerm, false
		lastIndex := rf.getLastLog().Index
		if lastIndex < request.PrevLogIndex {
			response.ConflictTerm, response.ConflictIndex = -1, lastIndex+1
		} else {
			firstIndex := rf.getFirstLog().Index
			response.ConflictTerm = rf.logs[request.PrevLogIndex-firstIndex].Term
			index := request.PrevLogIndex - 1
			for index >= firstIndex && rf.logs[index-firstIndex].Term == response.ConflictTerm {
				index--
			}
			response.ConflictIndex = index
		}
		return
	}

	firstIndex := rf.getFirstLog().Index
	for index, entry := range request.Entries {
		if entry.Index-firstIndex >= len(rf.logs) || rf.logs[entry.Index-firstIndex].Term != entry.Term {
			rf.logs = shrinkEntriesArray(append(rf.logs[:entry.Index-firstIndex], request.Entries[index:]...))
			break
		}
	}

	rf.advanceCommitIndexForFollower(request.LeaderCommit)

	response.Term, response.Success = rf.currentTerm, true
}
```

#### sender
可以看到对于该 follower 的复制，有 snapshot 和 entries 两种方式，需要根据该 peer 的 nextIndex 来判断。

对于如何生成 request 和处理 response，可以直接去看源码，总之都是按照图 2 来的，这里为了使得代码逻辑清晰将对应逻辑都封装到了函数里面。

```Go
func (rf *Raft) replicateOneRound(peer int) {
	rf.mu.RLock()
	if rf.state != StateLeader {
		rf.mu.RUnlock()
		return
	}
	prevLogIndex := rf.nextIndex[peer] - 1
	if prevLogIndex < rf.getFirstLog().Index {
		// only snapshot can catch up
		request := rf.genInstallSnapshotRequest()
		rf.mu.RUnlock()
		response := new(InstallSnapshotResponse)
		if rf.sendInstallSnapshot(peer, request, response) {
			rf.mu.Lock()
			rf.handleInstallSnapshotResponse(peer, request, response)
			rf.mu.Unlock()
		}
	} else {
		// just entries can catch up
		request := rf.genAppendEntriesRequest(prevLogIndex)
		rf.mu.RUnlock()
		response := new(AppendEntriesResponse)
		if rf.sendAppendEntries(peer, request, response) {
			rf.mu.Lock()
			rf.handleAppendEntriesResponse(peer, request, response)
			rf.mu.Unlock()
		}
	}
}
```
需要注意以下几点：
* 锁的使用：发送 rpc，接收 rpc，push channel，receive channel 的时候一定不要持锁，否则很可能产生死锁。这一点 locking 博客里面有介绍到。当然，基本地读写锁使用方式也最好注意一下，对于仅读的代码块就不需要持写锁了。
* 抛弃过期请求的回复：对于过期请求的回复，直接抛弃就行，不要做任何处理，这一点 guidance 里面也有介绍到。
* commit 日志：图 2 中规定，raft leader 只能提交当前 term 的日志，不能提交旧 term 的日志。因此 leader 根据 matchIndex[] 来 commit 日志时需要判断该日志的 term 是否等于 leader 当前的 term，即是否为当前 leader 任期新产生的日志，若是才可以提交。此外，follower 对 leader
的 leaderCommit 就不需要判断了，无条件服从即可。

#### 复制模型
对于复制模型，很直观的方式是：包装一个 BroadcastHeartbeat() 函数，其负责向所有 follower 发送一轮同步。不论是心跳超时还是上层服务传进来一个新 command，都去调一次这个函数来发起一轮同步。

以上方式是可以 work 的，我最开始的实现也是这样的，然而在测试过程中，我发现这种方式有很大的资源浪费。比如上层服务连续调用了几十次 Start() 函数，由于每一次调用 Start() 函数都会触发一轮日志同步，则最终导致发送了几十次日志同步。一方面，这些请求包含的 entries 基本都一样，甚至有 entry 连续出现在几十次 rpc 中，这样的实现多传输了一些数据，存在一定浪费；另一方面，每次发送 rpc 都不论是发送端还是接收端都需要若干次系统调用和内存拷贝，rpc 次数过多也会对 CPU 造成不必要的压力。总之，这种资源浪费的根本原因就在于：将日志同步的触发与上层服务提交新指令强绑定，从而导致发送了很多重复的 rpc。

为此，我参考了 `sofajraft` 的[日志复制实现](https://mp.weixin.qq.com/s/jzqhLptmgcNix6xYWYL01Q) 。每个 peer 在启动时会为除自己之外的每个 peer 都分配一个 replicator 协程。对于 follower 节点，该协程利用条件变量执行 wait 来避免耗费 cpu，并等待变成 leader 时再被唤醒；对于 leader 节点，该协程负责尽最大地努力去向对应 follower 发送日志使其同步，直到该节点不再是 leader 或者该 follower 节点的 matchIndex 大于等于本地的 lastIndex。

这样的实现方式能够将日志同步的触发和上层服务提交新指令解耦，能够大幅度减少传输的数据量，rpc 次数和系统调用次数。由于 6.824 的测试能够展示测试过程中的传输 rpc 次数和数据量，因此我进行了前后的对比测试，结果显示：这样的实现方式相比直观方式的实现，不同测试数据传输量的减少倍数在 1-20 倍之间。当然，这样的实现也只是实现了粗粒度的 batching，并没有流量控制，而且也没有实现 pipeline，有兴趣的同学可以去了解 `sofajraft`, `etcd` 或者 `tikv` 的实现，他们对于复制过程进行了更细粒度的控制。

此外，虽然 leader 对于每一个节点都有一个 replicator 协程去同步日志，但其目前同时最多只能发送一个 rpc，而这个 rpc 很可能超时或丢失从而触发集群换主。因此，对于 heartbeat timeout 触发的 BroadcastHeartbeat，我们需要立即发出日志同步请求而不是让 replicator 去发。这也就是我的 BroadcastHeartbeat 函数有两种行为的真正原因。

这一块的代码我自认为抽象的还是比较优雅的，也算是对 go 异步编程的一个实践吧。

```Go
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.state != StateLeader {
		return -1, -1, false
	}
	newLog := rf.appendNewEntry(command)
	DPrintf("{Node %v} receives a new command[%v] to replicate in term %v", rf.me, newLog, rf.currentTerm)
	rf.BroadcastHeartbeat(false)
	return newLog.Index, newLog.Term, true
}

func (rf *Raft) BroadcastHeartbeat(isHeartBeat bool) {
	for peer := range rf.peers {
		if peer == rf.me {
			continue
		}
		if isHeartBeat {
			// need sending at once to maintain leadership
			go rf.replicateOneRound(peer)
		} else {
			// just signal replicator goroutine to send entries in batch
			rf.replicatorCond[peer].Signal()
		}
	}
}

func (rf *Raft) replicator(peer int) {
	rf.replicatorCond[peer].L.Lock()
	defer rf.replicatorCond[peer].L.Unlock()
	for rf.killed() == false {
		// if there is no need to replicate entries for this peer, just release CPU and wait other goroutine's signal if service adds new Command
		// if this peer needs replicating entries, this goroutine will call replicateOneRound(peer) multiple times until this peer catches up, and then wait
		for !rf.needReplicating(peer) {
			rf.replicatorCond[peer].Wait()
		}
		// maybe a pipeline mechanism is better to trade-off the memory usage and catch up time
		rf.replicateOneRound(peer)
	}
}

func (rf *Raft) needReplicating(peer int) bool {
	rf.mu.RLock()
	defer rf.mu.RUnlock()
	return rf.state == StateLeader && rf.matchIndex[peer] < rf.getLastLog().Index
}
```

### 日志压缩

加入日志压缩功能后，需要注意及时对内存中的 entries 数组进行清除。即使得废弃的 entries 切片能够被正常 gc，从而避免内存释放不掉并最终 OOM 的现象出现，具体实现就是 shrinkEntriesArray 函数，具体原理可以参考此[博客](https://www.cnblogs.com/ithubb/p/14184982.html) 。 

#### 服务端触发的日志压缩
实现很简单，删除掉对应已经被压缩的 raft log 即可

```Go
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	snapshotIndex := rf.getFirstLog().Index
	if index <= snapshotIndex {
		DPrintf("{Node %v} rejects replacing log with snapshotIndex %v as current snapshotIndex %v is larger in term %v", rf.me, index, snapshotIndex, rf.currentTerm)
		return
	}
	rf.logs = shrinkEntriesArray(rf.logs[index-snapshotIndex:])
	rf.logs[0].Command = nil
	rf.persister.SaveStateAndSnapshot(rf.encodeState(), snapshot)
	DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} after replacing log with snapshotIndex %v as old snapshotIndex %v is smaller", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), index, snapshotIndex)
}
```

#### leader 发送来的 InstallSnapshot

对于 leader 发过来的 InstallSnapshot，只需要判断 term 是否正确，如果无误则 follower 只能无条件接受。

此外，如果该 snapshot 的 lastIncludedIndex 小于等于本地的 commitIndex，那说明本地已经包含了该 snapshot 所有的数据信息，尽管可能状态机还没有这个 snapshot 新，即 lastApplied 还没更新到 commitIndex，但是 applier 协程也一定尝试在 apply 了，此时便没必要再去用 snapshot 更换状态机了。对于更新的 snapshot，这里通过异步的方式将其 push 到 applyCh 中。

对于服务上层触发的 CondInstallSnapshot，与上面类似，如果 snapshot 没有更新的话就没有必要去换，否则就接受对应的 snapshot 并处理对应状态的变更。注意，这里不需要判断 lastIncludeIndex 和 lastIncludeTerm 是否匹配，因为 follower 对于 leader 发来的更新的 snapshot 是无条件服从的。

```Go
func (rf *Raft) InstallSnapshot(request *InstallSnapshotRequest, response *InstallSnapshotResponse) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} before processing InstallSnapshotRequest %v and reply InstallSnapshotResponse %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), request, response)

	response.Term = rf.currentTerm

	if request.Term < rf.currentTerm {
		return
	}

	if request.Term > rf.currentTerm {
		rf.currentTerm, rf.votedFor = request.Term, -1
		rf.persist()
	}

	rf.ChangeState(StateFollower)
	rf.electionTimer.Reset(RandomizedElectionTimeout())

	// outdated snapshot
	if request.LastIncludedIndex <= rf.commitIndex {
		return
	}

	go func() {
		rf.applyCh <- ApplyMsg{
			SnapshotValid: true,
			Snapshot:      request.Data,
			SnapshotTerm:  request.LastIncludedTerm,
			SnapshotIndex: request.LastIncludedIndex,
		}
	}()
}


func (rf *Raft) CondInstallSnapshot(lastIncludedTerm int, lastIncludedIndex int, snapshot []byte) bool {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	DPrintf("{Node %v} service calls CondInstallSnapshot with lastIncludedTerm %v and lastIncludedIndex %v to check whether snapshot is still valid in term %v", rf.me, lastIncludedTerm, lastIncludedIndex, rf.currentTerm)

	// outdated snapshot
	if lastIncludedIndex <= rf.commitIndex {
		DPrintf("{Node %v} rejects the snapshot which lastIncludedIndex is %v because commitIndex %v is larger", rf.me, lastIncludedIndex, rf.commitIndex)
		return false
	}

	if lastIncludedIndex > rf.getLastLog().Index {
		rf.logs = make([]Entry, 1)
	} else {
		rf.logs = shrinkEntriesArray(rf.logs[lastIncludedIndex-rf.getFirstLog().Index:])
		rf.logs[0].Command = nil
	}
	// update dummy entry with lastIncludedTerm and lastIncludedIndex
	rf.logs[0].Term, rf.logs[0].Index = lastIncludedTerm, lastIncludedIndex
	rf.lastApplied, rf.commitIndex = lastIncludedIndex, lastIncludedIndex

	rf.persister.SaveStateAndSnapshot(rf.encodeState(), snapshot)
	DPrintf("{Node %v}'s state is {state %v,term %v,commitIndex %v,lastApplied %v,firstLog %v,lastLog %v} after accepting the snapshot which lastIncludedTerm is %v, lastIncludedIndex is %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, rf.lastApplied, rf.getFirstLog(), rf.getLastLog(), lastIncludedTerm, lastIncludedIndex)
	return true
}
```

#### 异步 applier 的 exactly once

异步 apply 可以提升 raft 算法的性能，具体可以参照 PingCAP 的[博客](https://pingcap.com/blog-cn/optimizing-raft-in-tikv/) 。

对于异步 apply，其触发方式无非两种，leader 提交了新的日志或者 follower 通过 leader 发来的 leaderCommit 来更新 commitIndex。很多人实现的时候可能顺手就在这两处异步启一个协程把 [lastApplied + 1, commitIndex] 的 entry push 到 applyCh 中，但其实这样子是可能重复发送 entry 的，原因是 push applyCh 的过程不能够持锁，那么这个 lastApplied 在没有 push 完之前就无法得到更新，从而可能被多次调用。虽然只要上层服务可以保证不重复 apply 相同 index 的日志到状态机就不会有问题，但我个人认为这样的做法是不优雅的。考虑到异步 apply 时最耗时的步骤是 apply channel 和 apply 日志到状态机，其他的都不怎么耗费时间。因此我们完全可以只用一个 applier 协程，让其不断的把 [lastApplied + 1, commitIndex] 区间的日志 push 到 applyCh 中去。这样既可保证每一条日志只会被 exactly once 地 push 到 applyCh 中，也可以使得日志 apply 到状态机和 raft 提交新日志可以真正的并行。我认为这是一个较为优雅的异步 apply 实现。

```Go
// a dedicated applier goroutine to guarantee that each log will be push into applyCh exactly once, ensuring that service's applying entries and raft's committing entries can be parallel
func (rf *Raft) applier() {
	for rf.killed() == false {
		rf.mu.Lock()
		// if there is no need to apply entries, just release CPU and wait other goroutine's signal if they commit new entries
		for rf.lastApplied >= rf.commitIndex {
			rf.applyCond.Wait()
		}
		firstIndex, commitIndex, lastApplied := rf.getFirstLog().Index, rf.commitIndex, rf.lastApplied
		entries := make([]Entry, commitIndex-lastApplied)
		copy(entries, rf.logs[lastApplied+1-firstIndex:commitIndex+1-firstIndex])
		rf.mu.Unlock()
		for _, entry := range entries {
			rf.applyCh <- ApplyMsg{
				CommandValid: true,
				Command:      entry.Command,
				CommandTerm:  entry.Term,
				CommandIndex: entry.Index,
			}
		}
		rf.mu.Lock()
		DPrintf("{Node %v} applies entries %v-%v in term %v", rf.me, rf.lastApplied, commitIndex, rf.currentTerm)
		// use commitIndex rather than rf.commitIndex because rf.commitIndex may change during the Unlock() and Lock()
		// use Max(rf.lastApplied, commitIndex) rather than commitIndex directly to avoid concurrently InstallSnapshot rpc causing lastApplied to rollback
		rf.lastApplied = Max(rf.lastApplied, commitIndex)
		rf.mu.Unlock()
	}
}
```
需要注意以下两点：
* 引用之前的 commitIndex：push applyCh 结束之后更新 lastApplied 的时候一定得用之前的 commitIndex 而不是 rf.commitIndex，因为后者很可能在 push channel 期间发生了改变。
* 防止与 installSnapshot 并发导致 lastApplied 回退：需要注意到，applier 协程在 push channel 时，中间可能夹杂有 snapshot 也在 push channel。如果该 snapshot 有效，那么在 CondInstallSnapshot 函数里上层状态机和 raft 模块就会原子性的发生替换，即上层状态机更新为 snapshot 的状态，raft 模块更新 log, commitIndex, lastApplied 等等，此时如果这个 snapshot 之后还有一批旧的 entry 在 push channel，那上层服务需要能够知道这些 entry 已经过时，不能再 apply，同时 applier 这里也应该加一个 Max 自身的函数来防止 lastApplied 出现回退。

### 持久化

6.824 目前的持久化方式较为简单，每次发生一些变动就要把所有的状态都编码持久化一遍，这显然是生产不可用的。生产环境中，至少对于 raft 日志，应该是通过一个类似于 wal 的方式来顺序写磁盘并有可能在内存和磁盘上都 truncate 未提交的日志。当然，是不是每一次变化都要 async 一下可能就是性能和安全性之间的考量了。

此外，有好多人会遇到跑几百次测试才能复现一次的 bug，这种多半是持久化这里没有做完备。建议检查一下代码: currentTerm, voteFor 和 logs 这三个变量一旦发生变化就一定要在被其他协程感知到之前（释放锁之前，发送 rpc 之前）持久化，这样才能保证原子性。

## 问题

尽管 6.824 的测试用例已经相当详细，对 corner case 的测试已经相当完备了。但我还是发现其至少遗漏了两个 corner case： 一个有关 safety，一个有关 liveness。

### 问题 1：有关 safety

raft leader 只能提交当前任期的日志，不能直接提交过去任期的日志，这一点在图 2 中也有展示，具体样例可以参考论文中的图 8。我最开始的实现并没有考虑此 corner case，跑了 400 次 lab2 的测试依然没有出现一次 FAIL，就以为没问题了。后面我又仔细看了一下图 2，发现这种 corner case 不处理是会有 safety 问题的： 即对于论文图 8 的情况，可能导致已经提交的日志又被覆盖。简单看了一下 6.824 对于图 8 的测试，发现其似乎并不能完备地测试出图 8 的场景，感觉这个测试也需要进一步完善吧。

### 问题 2：有关 liveness
 
leader 上任后应该提交一条空日志来提交之前的日志，否则会有 liveness 的问题，即可能再某些场景下长时间无法提供读服务。

考虑这样一个场景：三节点的集群，节点 1 是 leader，其 logs 是 [1,2]，commitIndex 和 lastApplied 是 2，节点 2 是 follower，其 logs 是 [1,2]，commitIndex 和 lastApplied 是 1，节点 3 是 follower，其 logs 是 [1]，commitIndex 和 lastApplied 是 1。即节点 1 将日志 2 发送到了节点 2 之后即可将日志 2 提交。此时对于节点 2，日志 2 的提交信息还没有到达；对于节点 3，日志 2 还没有到达。显然这种场景是可以出现的。此时 A 作为 leader 自然可以处理读请求，即此时的读请求是对于 index 为 2 的状态机做的。接着 leader 节点进行了宕机，两个 follower 随后会触发选举超时，但由于节点 2 日志最新，所以最后一定是节点 2 当选为 leader，节点 2 当选之后，其会首先判断集群中的 matchIndex 并确定 commitIndex，即 commitIndex 为 1，此时如果客户端没有新地写请求过来，这个 commitIndex 就一直会是 1，得不到更新。那么在此期间，如果又有读请求路由到了节点 2，如果其直接执行了读请求，显然是不满足线性一致性的，因为状态机的状态是更旧的。因此，其需要首先判断当前 term 有没有提交过日志，如果没有，则应该等待当前 term 提交过日志之后才能处理读请求。而当前 term 提交日志又是与客户端提交命令绑定的，客户端不提交命令，当前 term 就没有新日志，那么就一直不能处理读请求。因此，leader 上任后应该提交一条空日志来提交之前的日志。

我尝试在 leader 上任之后加了空日志，然而加了之后 lab 2b 的测试全挂了，看来他们应该暂时也还没有考虑这种 corner case。