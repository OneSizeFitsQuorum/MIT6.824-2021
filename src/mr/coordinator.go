package mr

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"time"
)

const (
	MaxTaskRunInterval = time.Second * 10
)

type Task struct {
	fileName  string
	id        int
	startTime time.Time
	status    TaskStatus
}

// A laziest, worker-stateless, channel-based implementation of Coordinator
type Coordinator struct {
	files   []string
	nReduce int
	nMap    int
	phase   SchedulePhase
	tasks   []Task

	heartbeatCh chan heartbeatMsg
	reportCh    chan reportMsg
	doneCh      chan struct{}
}

type heartbeatMsg struct {
	response *HeartbeatResponse
	ok       chan struct{}
}

type reportMsg struct {
	request *ReportRequest
	ok      chan struct{}
}

// the RPC argument and reply types are defined in rpc.go.
//
func (c *Coordinator) Heartbeat(request *HeartbeatRequest, response *HeartbeatResponse) error {
	msg := heartbeatMsg{response, make(chan struct{})}
	c.heartbeatCh <- msg
	<-msg.ok
	return nil
}

func (c *Coordinator) Report(request *ReportRequest, response *ReportResponse) error {
	msg := reportMsg{request, make(chan struct{})}
	c.reportCh <- msg
	<-msg.ok
	return nil
}

func (c *Coordinator) schedule() {
	c.initMapPhase()
	for {
		select {
		case msg := <-c.heartbeatCh:
			if c.phase == CompletePhase {
				msg.response.JobType = CompleteJob
			} else if c.selectTask(msg.response) {
				switch c.phase {
				case MapPhase:
					log.Printf("Coordinator: %v finished, start %v \n", MapPhase, ReducePhase)
					c.initReducePhase()
					c.selectTask(msg.response)
				case ReducePhase:
					log.Printf("Coordinator: %v finished, Congratulations \n", ReducePhase)
					c.initCompletePhase()
					msg.response.JobType = CompleteJob
				case CompletePhase:
					panic(fmt.Sprintf("Coordinator: enter unexpected branch"))
				}
			}
			log.Printf("Coordinator: assigned a task %v to worker \n", msg.response)
			msg.ok <- struct{}{}
		case msg := <-c.reportCh:
			if msg.request.Phase == c.phase {
				log.Printf("Coordinator: Worker has executed task %v \n", msg.request)
				c.tasks[msg.request.Id].status = Finished
			}
			msg.ok <- struct{}{}
		}
	}
}

func (c *Coordinator) selectTask(response *HeartbeatResponse) bool {
	allFinished, hasNewJob := true, false
	for id, task := range c.tasks {
		switch task.status {
		case Idle:
			allFinished, hasNewJob = false, true
			c.tasks[id].status, c.tasks[id].startTime = Working, time.Now()
			response.NReduce, response.Id = c.nReduce, id
			if c.phase == MapPhase {
				response.JobType, response.FilePath = MapJob, c.files[id]
			} else {
				response.JobType, response.NMap = ReduceJob, c.nMap
			}
		case Working:
			allFinished = false
			if time.Now().Sub(task.startTime) > MaxTaskRunInterval {
				hasNewJob = true
				c.tasks[id].startTime = time.Now()
				response.NReduce, response.Id = c.nReduce, id
				if c.phase == MapPhase {
					response.JobType, response.FilePath = MapJob, c.files[id]
				} else {
					response.JobType, response.NMap = ReduceJob, c.nMap
				}
			}
		case Finished:
		}
		if hasNewJob {
			break
		}
	}

	if !hasNewJob {
		response.JobType = WaitJob
	}
	return allFinished
}

func (c *Coordinator) initMapPhase() {
	c.phase = MapPhase
	c.tasks = make([]Task, len(c.files))
	for index, file := range c.files {
		c.tasks[index] = Task{
			fileName: file,
			id:       index,
			status:   Idle,
		}
	}
}

func (c *Coordinator) initReducePhase() {
	c.phase = ReducePhase
	c.tasks = make([]Task, c.nReduce)
	for i := 0; i < c.nReduce; i++ {
		c.tasks[i] = Task{
			id:     i,
			status: Idle,
		}
	}
}

func (c *Coordinator) initCompletePhase() {
	c.phase = CompletePhase
	c.doneCh <- struct{}{}
}

//
// start a thread that listens for RPCs from worker.go
//
func (c *Coordinator) server() {
	rpc.Register(c)
	rpc.HandleHTTP()
	//l, e := net.Listen("tcp", ":1234")
	name := coordinatorSock()
	os.Remove(name)
	l, e := net.Listen("unix", name)
	if e != nil {
		log.Fatal("listen error:", e)
	}
	go http.Serve(l, nil)
}

//
// main/mrcoordinator.go calls Done() periodically to find out
// if the entire job has finished.
//
func (c *Coordinator) Done() bool {
	<-c.doneCh
	return true
}

//
// create a Coordinator.
// main/mrcoordinator.go calls this function.
// nReduce is the number of reduce tasks to use.
//
func MakeCoordinator(files []string, nReduce int) *Coordinator {
	c := Coordinator{
		files:       files,
		nReduce:     nReduce,
		nMap:        len(files),
		heartbeatCh: make(chan heartbeatMsg),
		reportCh:    make(chan reportMsg),
		doneCh:      make(chan struct{}, 1),
	}
	c.server()
	go c.schedule()
	return &c
}
