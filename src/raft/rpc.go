package raft

import "fmt"

type RequestVoteRequest struct {
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

func (request RequestVoteRequest) String() string {
	return fmt.Sprintf("{Term:%v,CandidateId:%v,LastLogIndex:%v,LastLogTerm:%v}", request.Term, request.CandidateId, request.LastLogIndex, request.LastLogTerm)
}

type RequestVoteResponse struct {
	Term        int
	VoteGranted bool
}

func (response RequestVoteResponse) String() string {
	return fmt.Sprintf("{Term:%v,VoteGranted:%v}", response.Term, response.VoteGranted)
}

type AppendEntriesRequest struct {
	Term         int
	LeaderId     int
	PrevLogIndex int
	PrevLogTerm  int
	LeaderCommit int
	Entries      []Entry
}

func (request AppendEntriesRequest) String() string {
	return fmt.Sprintf("{Term:%v,LeaderId:%v,PrevLogIndex:%v,PrevLogTerm:%v,LeaderCommit:%v,Entries:%v}", request.Term, request.LeaderId, request.PrevLogIndex, request.PrevLogTerm, request.LeaderCommit, request.Entries)
}

type AppendEntriesResponse struct {
	Term          int
	Success       bool
	ConflictIndex int
	ConflictTerm  int
}

func (response AppendEntriesResponse) String() string {
	return fmt.Sprintf("{Term:%v,Success:%v,ConflictIndex:%v,ConflictTerm:%v}", response.Term, response.Success, response.ConflictIndex, response.ConflictTerm)
}

type InstallSnapshotRequest struct {
	Term              int
	LeaderId          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

func (request InstallSnapshotRequest) String() string {
	return fmt.Sprintf("{Term:%v,LeaderId:%v,LastIncludedIndex:%v,LastIncludedTerm:%v,DataSize:%v}", request.Term, request.LeaderId, request.LastIncludedIndex, request.LastIncludedTerm, len(request.Data))
}

type InstallSnapshotResponse struct {
	Term int
}

func (response InstallSnapshotResponse) String() string {
	return fmt.Sprintf("{Term:%v}", response.Term)
}
