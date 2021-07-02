<!-- TOC -->

- [思路](#思路)
- [实现](#实现)
    - [Worker](#worker)
    - [Coordinator](#coordinator)
        - [worker-stateless](#worker-stateless)
        - [channel-based](#channel-based)
        - [laziest](#laziest)

<!-- /TOC -->

## 思路

很久没有使用 go 了，也很久没有感受到 go channel 的魅力了，因此我在做 lab1 的时候最朴素地想法就是实现一个 lock-free （不过其实 channel 内部也有锁）的版本，总之就是不想在我的代码里看到锁这个结构，想要全部用 channel 实现。

一番探索之后，也多亏看了 Russ Cox 的[讲课](https://www.youtube.com/watch?v=IdCbMO0Ey9I&feature=youtu.be)，最后总算实现出来了一个勉强满意的版本。

下小节会简单介绍我的实现。

## 实现

### Worker

worker 这边的逻辑比较简单，轮训做任务即可，其主要逻辑如下，分别实现对应的 `doMapTask` 和 `doReduceTask` 函数即可。

```Go
func Worker(mapF func(string, string) []KeyValue,
	reduceF func(string, []string) string) {
	for {
		response := doHeartbeat()
		log.Printf("Worker: receive coordinator's heartbeat %v \n", response)
		switch response.JobType {
		case MapJob:
			doMapTask(mapF, response)
		case ReduceJob:
			doReduceTask(reduceF, response)
		case WaitJob:
			time.Sleep(1 * time.Second)
		case CompleteJob:
			return
		default:
			panic(fmt.Sprintf("unexpected jobType %v", response.JobType))
		}
	}
}
```

需要注意的有以下两点：
* atomicWriteFile：需要保证 map 任务和 reduce 任务生成文件时的原子性，从而避免某些异常情况导致文件受损，使得之后的任务无法执行下去的 bug。具体方法就是先生成一个临时文件再利用系统调用 `OS.Rename` 来完成原子性替换，这样即可保证写文件的原子性。
* mergeSort or hashSort: 对于 `doReduceTask` 函数，其输入是一堆本地文件（或者远程文件），输出是一个文件。执行过程是在保证不 OOM 的情况下，不断把 `<key,list(intermediate_value)>` 对喂给用户的 reduce 函数去执行并得到最终的 `<key,value>` 对，然后再写到最后的输出文件中去。在本 lab 中，为了简便我直接使用了一个内存哈希表 (map[string][]string) 来将同一个 key 的 values 放在一起，然后遍历该哈希表来喂给用户的 reduce 函数，实际上这样子是没有做内存限制的。在生产级别的 MapReduce 实现中，该部分一定是一个内存+外存来 mergeSort ，然后逐个喂给 reduce 函数的，这样的鲁棒性才会更高。

### Coordinator
coordinator 这边的逻辑相对复杂些，但我的实现还算相对轻便，一百多行就已经能够过测试了。这里可以谈谈我实现的 coordinator 的三个特点：
* worker-stateless
* channel-based 
* laziest

我的结构体定义如下：
```Go
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
```

#### worker-stateless

很多人写 coordinator 的时候喜欢维护 worker 的状态，比如 worker 启动时需要先到 coordinator 注册一个 id，然后 coordinator 维护一个 worker 的 map 或者 list，定期检测所有 worker 的工作状态等等，这增加了许多 coordinator 的复杂度和代码量。

对于 6.824 的 lab1 来说，其数据文件都是存储在一个共享存储系统上的，因此其不同的 worker 实际上是对等的，即把 map 任务和 reduce 任务扔给任何一个 worker 理论上性能都不会有区别。那么我们实际上没有必要去维护 worker 的状态，我们可以把 worker 设计成 stateless ，我们只需要关注 task 的状态（是否被执行，是否超时等等）即可，这样即可极大程度的减少 coordinator 的代码复杂度。

比如在我的 coordinator 结构体中，我只有一个 []Task 数组而没有维护 worker 的状态，这样子的实现会非常简单。

此外，stateless 对于云原生可扩展性也是十分友好的，此处不再赘述。

#### channel-based

channel 和 goroutine 都是 go 中非常有用的工具，也可以说是 go 的精髓。因此尽管最开始我实现的是 mutex—based 的版本，后来还是改成了 channel-base 的版本，感觉代码上变得优雅了许多。

总结一下就是 coordinator 在启动时就开启一个后台 goroutine 去不断监控 heartbeatCh 和 reportCh 中的数据并做处理，coordinator 结构体的所有数据都在这个 goroutine 中去修改，从而避免了 data race 的问题，对于 worker 请求任务和汇报任务的 rpc handler，其可以创建 heartbeatMsg 和 reportMsg 并将其写入对应的 channel 然后等待该 msg 中的 ok channel 返回即可。

```Go
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
                        ...
			msg.ok <- struct{}{}
		case msg := <-c.reportCh:
                        ...
			msg.ok <- struct{}{}
		}
	}
}
```

需要注意的有以下两点：
* channel 传 struct{}：对于仅需要传输 happens-before 语义不需要传输数据的场景，创建的 channel 应该是 struct{} 类型，go 对其做了特别优化可以不耗费内存。
* channel 传指针：对于 report handler，其往 reportCh 中 send msg 时只需要传输 request 的指针，等 coordinator 的 schedule 协程处理完毕后即可返回，这里并没有什么问题。对于 heartbeat handler，其会相对复杂些，因为其往 heartbeatCh 中 send msg 时传输了 response 的指针，coordinator 的 schedule 协程需要对该指针对应的数据做处理后再返回，那么此时 rpc 协程在返回时是否能够看到另一个 goroutine 对其的修改呢？对于这种场景，如果协程间满足 happens-before 语义的话是可以的，如果不满足则不一定可以。那么是否满足 happens-before 语义呢？很多人都知道对于无 buffer 的 channel，其 `receive` 是 happens-before `send` 的，那么似乎就无法判断其是否满足 happens-before 语义了。实际上，`send` 与 `send 完成`是有区别的，可以参考此[博客](https://studygolang.com/articles/14129)，严格来说：对于无 buffer 的 channel，其 `send start` happens-before `receive complete` happens-before `send complete`，因此有了这个更清晰的语义，我们很显然可以得到 `schedule 协程修改 response` happens-before `worker rpc 协程返回 response`，因此这样写应该是没有问题的，我的 race detector 也没有报任何错误。（如果分析的有问题，欢迎讨论赐教）

#### laziest

这个就比较有趣了，前面说到我们可以把 worker 搞成 stateless 的，这样就只用监控 task 的状态而不是 worker 的状态，但实际上我没有去启动额外的 goroutine 去监控每个任务的状态，因为这可能又要招致额外的并发复杂度，相反我采用了最懒的方式：只有 worker 来请求任务时才遍历任务列表去查看是否可以分配任务。

这样就可能出现一个有争议的现象：比如只有一个 coordinator 和一个 worker，worker 拿了一个 map task 之后挂了，那么即使这个 task 已经超过了限定时间，coordinator 也暂时不知道，其只有在 worker 再一次申请任务时才会检测到这个现象，这样子实现有没有问题呢？我个人感觉是没有大问题的，因为如果没有 worker 来领任务，coordinator 就算检测到了任务超时又能怎么样呢，也不能分配给任何 worker 去做。

当然，由于检查任务状态是通过遍历 task 列表来做的，如果这个列表有几百上千万那么多，那么现在的写法可能会有点问题，但如果只是几百上千，那对应的 CPU 操作耗时实际上也不会有什么瓶颈。

总之，这个实现不影响正确性也不是很影响性能，但能够降低代码的复杂度，就是看起来似乎有点太懒了。