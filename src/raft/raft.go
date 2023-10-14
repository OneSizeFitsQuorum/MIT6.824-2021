package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	//	"bytes"
	"fmt"
	"bytes"
	"6.824/labgob"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	//	"6.824/labgob"
	"6.824/labrpc"
)

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in part 2D you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh, but set CommandValid to false for these
// other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	// For 2D:
	SnapshotValid bool
	Snapshot      []byte
	SnapshotTerm  int
	SnapshotIndex int
}

type AppendEntriesArgs struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []Entry
	LeaderCommit int
} 

type AppendEntriesReply struct {
    Term     int
    Success  bool
}

type Entry struct {
	Term int
	Index int
	Comand interface{}	
}

type NodeState int

const (
	ElectionTimeout  = time.Millisecond * 300 // 选举
	HeartBeatTimeout = time.Millisecond * 150 // leader 发送心跳
	ApplyInterval    = time.Millisecond * 100 // apply log
	RPCTimeout       = time.Millisecond * 100
	MaxLockTime      = time.Millisecond * 10 // debug
)

const (
	Leader NodeState = 0
	Follower NodeState = 1
	Candidate NodeState = 2
)

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]
	dead      int32               // set by Kill()

	// Your data here (2A, 2B, 2C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.
	applyCh        chan ApplyMsg
	applyCond      *sync.Cond   // used to wakeup applier goroutine after committing new entries
	replicatorCond []*sync.Cond // used to signal replicator goroutine to batch replicating entries
	state          NodeState

	currentTerm int
	votedFor    int
	logs        []Entry // the first entry is a dummy entry which contains LastSnapshotTerm, LastSnapshotIndex and nil Command

	commitIndex int
	lastApplied int
	votes_counts int
	nextIndex   []int
	matchIndex  []int

	electionTimer  *time.Timer
	heartbeatTimer *time.Timer
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {

	var term int
	var isleader bool
	// Your code here (2A).
	return term, isleader
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
//
func (rf *Raft) persist() {
	// Your code here (2C).
	// Example:
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(rf.votedFor)
	e.Encode(rf.currentTerm)
	e.Encode(rf.commitIndex)
	e.Encode(rf.logs)
	data := w.Bytes()
	rf.persister.SaveRaftState(data)
}


//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	// Your code here (2C).
	// Example:
	// r := bytes.NewBuffer(data)
	// d := labgob.NewDecoder(r)
	// var xxx
	// var yyy
	// if d.Decode(&xxx) != nil ||
	//    d.Decode(&yyy) != nil {
	//   error...
	// } else {
	//   rf.xxx = xxx
	//   rf.yyy = yyy
	// }
}


//
// A service wants to switch to snapshot.  Only do so if Raft hasn't
// have more recent info since it communicate the snapshot on applyCh.
//
func (rf *Raft) CondInstallSnapshot(lastIncludedTerm int, lastIncludedIndex int, snapshot []byte) bool {

	// Your code here (2D).

	return true
}

// the service says it has created a snapshot that has
// all info up to and including index. this means the
// service no longer needs the log through (and including)
// that index. Raft should now trim its log as much as possible.
func (rf *Raft) Snapshot(index int, snapshot []byte) {
	// Your code here (2D).

}


//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	// Your data here (2A, 2B).

	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int

}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).
	Term int
	VoteGranted bool
	
}

//
// example RequestVote RPC handler.
//
func (rf *Raft) RequestVote(request *RequestVoteArgs, response *RequestVoteReply) {
	// Your code here (2A, 2B).
	rf.mu.Lock()
	defer rf.mu.Unlock()
	defer rf.persist()
	defer DPrintf("{Node %v}'s state is {state %v,term %v,currentTerm %v,} before processing requestVoteRequest %v and reply requestVoteResponse %v", rf.me, rf.state, rf.currentTerm, rf.commitIndex, request, response)

	if request.Term < rf.currentTerm || (request.Term == rf.currentTerm && rf.votedFor != -1 && rf.votedFor != request.CandidateId) {
		response.Term, response.VoteGranted = rf.currentTerm, false
		return
	}

	if request.Term > rf.currentTerm {
		DPrintf("go follow")
		rf.ChangeState(Follower)
		rf.currentTerm, rf.votedFor = request.Term, -1
		response.Term, response.VoteGranted = rf.currentTerm, false
		return 
	}

	rf.votedFor = request.CandidateId
	rf.electionTimer.Reset(randElectionTimeout())
	response.VoteGranted = true
}

func (rf *Raft) AppendEntriesHandle(args *AppendEntriesArgs, reply *AppendEntriesReply) {

	rf.electionTimer.Stop()
	rf.electionTimer.Reset(randElectionTimeout())

	reply.Success = true
}

// func (rf *Raft) isLogUpToDate(LastLogTerm int, LastLogIndex int) bool {
// 	if rf.currentTerm < LastLogTerm {
// 		return true
// 	}else{
// 		return false
// 	}
// }

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) sendAppendEntriesHandle(server int, args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntriesHandle", args, reply)
	return ok
}



//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	index := -1
	term := -1
	isLeader := true

	// Your code here (2B).


	return index, term, isLeader
}

//
// the tester doesn't halt goroutines created by Raft after each test,
// but it does call the Kill() method. your code can use killed() to
// check whether Kill() has been called. the use of atomic avoids the
// need for a lock.
//
// the issue is that long-running goroutines use memory and may chew
// up CPU time, perhaps causing later tests to fail and generating
// confusing debug output. any goroutine with a long-running loop
// should call killed() to check whether it should stop.
//
func (rf *Raft) Kill() {
	atomic.StoreInt32(&rf.dead, 1)
	// Your code here, if desired.
}

func (rf *Raft) killed() bool {
	z := atomic.LoadInt32(&rf.dead)
	return z == 1
}

func (rf *Raft) BroadcastHeartbeat(isHeartBeat bool) {
	for peer := range rf.peers {
		if peer == rf.me {
			continue
		}
		if isHeartBeat {
			// need sending at once to maintain leadership
			go rf.replicateOneRound(peer)
		}
	}
}

func (rf *Raft) appendEntries(heartbeat bool) {
    lastLog := rf.logs[len(rf.logs)-1]
    for peer, _ := range rf.peers {
        if peer == rf.me {
            rf.resetElectionTimer()
            continue
        }
        // rules for leader 3
        if lastLog.Index > rf.nextIndex[peer] || heartbeat {
            nextIndex := rf.nextIndex[peer]
            if nextIndex <= 0 {
                nextIndex = 1
            }
            if lastLog.Index+1 < nextIndex {
                nextIndex = lastLog.Index
            }
            prevLog := rf.logs[nextIndex - 1]
            args := AppendEntriesArgs{
                Term:         rf.currentTerm,
                LeaderId:     rf.me,
                PrevLogIndex: prevLog.Index,
                PrevLogTerm:  prevLog.Term,
                Entries:      make([]Entry, lastLog.Index-nextIndex+1),
                LeaderCommit: rf.commitIndex,
            }
            copy(args.Entries, rf.logs[:nextIndex])
            go rf.leaderSendEntries(peer, &args)
        }
    }
}

func (rf *Raft) replicateOneRound(peer int) {
	rf.mu.Lock()
	if rf.state != Leader {
		rf.mu.Unlock()
		return
	}
}

// func (rf *Raft) genRequestVoteRequest() *RequestVoteArgs {
// 	request := new(RequestVoteArgs)
// 	request.Term = rf.currentTerm
// 	request.CandidateId = 
// 	request.LastLogIndex = rf.getLastLog().Index
// 	request.LastLogTerm = rf.lastApplied

// 	return request
// }
func (rf *Raft) resetElectionTimer() {
	rf.killed()
	rf.electionTimer.Stop()
	rf.electionTimer.Reset(randElectionTimeout())
}

// 开始选举
func (rf *Raft) StartElection() {
	DPrintf("start election")

	if rf.state != Leader {
		request := &RequestVoteArgs{
			Term:         rf.currentTerm,   // 请求者的任期
			CandidateId:  rf.me,            // 请求者的ID
			LastLogIndex: len(rf.logs) - 1, // 这一项用于在选举中选出最数据最新的节点 论文[5.4.1]
			
		}
		if len(rf.logs) > 0 {
			request.LastLogTerm = rf.logs[request.LastLogIndex].Term
		}
		DPrintf("{Node %v} starts election with RequestVoteRequest %v", rf.me, request)

		//开始给自己投一票
		rf.votedFor = rf.me
		rf.persist()
		for peer := range rf.peers {
			// 不和自己通信
			if peer == rf.me {
				continue
			}
			go func(peer int) {
				//并发通信
				response := new(RequestVoteReply)
				if rf.sendRequestVote(peer, request, response) {
					DPrintf("{Node %v} receives RequestVoteResponse %v from {Node %v} after sending RequestVoteRequest %v in term %v", rf.me, response, peer, request, rf.currentTerm)
	
					if rf.state == Candidate {
						
						if response.VoteGranted {
							rf.votes_counts += 1
							if rf.votes_counts > len(rf.peers)/2 {
								DPrintf("{Node %v} receives majority votes in term %v", rf.me, rf.currentTerm)
								rf.ChangeState(Leader)
								// rf.BroadcastHeartbeat(true)
								for i:=0; i < len(rf.peers); i++ {
									fmt.Println(i)
									if i != peer {
										args := AppendEntriesArgs{
											Term:         rf.currentTerm,
											LeaderId:     rf.me,
											
										}
										responseArgs := AppendEntriesReply{}
										rf.sendAppendEntriesHandle(i, &args, &responseArgs)
									}
									
								}
								
								
							}
						} else if response.Term > rf.currentTerm {
							DPrintf("{Node %v} finds a new leader {Node %v} with term %v and steps down in term %v", rf.me, peer, response.Term, rf.currentTerm)
							rf.ChangeState(Follower)
							rf.currentTerm, rf.votedFor = response.Term, -1
							rf.persist()
						}
					}
	
				}
			}(peer)
		}
	}
	
}

// The ticker go routine starts a new election if this peer hasn't received
// heartsbeats recently.
func (rf *Raft) ticker() {
	for rf.killed() == false {

		// Your code here to check if a leader election should
		// be started and to randomize sleeping time using
		// time.Sleep().

	}
}

func (rf *Raft) getLastLog() Entry {
	length := len(rf.logs)
	return rf.logs[length - 1]
}

func (rf *Raft) getFirstLog() Entry {
	length := len(rf.logs)
	if length > 0 {
		return rf.logs[0]
	}else{
		return Entry{}
	}
}


func (rf *Raft) ChangeState(state NodeState) {
	rf.state = state
}

// 这是加上了随机时间x
func randElectionTimeout() time.Duration {
	r := time.Duration(rand.Int63()) % ElectionTimeout
	return ElectionTimeout + r
}
//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{
		peers:          peers,
		persister:      persister,
		me:             me,
		dead:           0,
		applyCh:        applyCh,
		replicatorCond: make([]*sync.Cond, len(peers)),
		state:          Follower,
		currentTerm:    0,
		votes_counts:   0,
		votedFor:       -1,
		logs:           make([]Entry, 1),
		nextIndex:      make([]int, len(peers)),
		matchIndex:     make([]int, len(peers)),
		heartbeatTimer: time.NewTimer(HeartBeatTimeout),
		electionTimer:  time.NewTimer(randElectionTimeout()),
	}
	DPrintf("%s start ", rf.me)
	// Your initialization code here (2A, 2B, 2C).

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	// rf.applyCond = sync.NewCond(&rf.mu)
	// lastLog := rf.getLastLog()
	// for i := 0; i < len(peers); i++ {
	// 	rf.matchIndex[i], rf.nextIndex[i] = 0, lastLog.Index+1
	// 	if i != rf.me {
	// 		rf.replicatorCond[i] = sync.NewCond(&sync.Mutex{})
	// 		// start replicator goroutine to replicate entries in batch
	// 		go rf.replicator(i)
	// 	}
	// }
	// start ticker goroutine to start elections
	// go rf.ticker()
	rf.mu.Lock()
	// 先修改状态为候选人
	rf.ChangeState(Candidate)
	// 目前任期加一
	rf.currentTerm += 1

	rf.votes_counts += 1
	// 开始选举
	rf.StartElection()
	rf.electionTimer.Reset(randElectionTimeout())
	rf.mu.Unlock()

	return rf
}
