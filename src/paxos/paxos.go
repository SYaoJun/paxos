package paxos

//
// Paxos library, to be included in an application.
// Multiple applications will run, each including
// a Paxos peer.
//
// Manages a sequence of agreed-on values.
// The set of peers is fixed.
// Copes with network failures (partition, msg loss, &c).
// Does not store anything persistently, so cannot handle crash+restart.
//
// The application interface:
//
// px = paxos.Make(peers []string, me string)
// px.Start(seq int, v interface{}) -- start agreement on new instance
// px.Status(seq int) (Fate, v interface{}) -- get info about an instance
// px.Done(seq int) -- ok to forget all instances <= seq
// px.Max() int -- highest instance seq known, or -1
// px.Min() int -- instances before this seq have been forgotten
//

import "net"
import "net/rpc"
import "log"

import "os"
import "syscall"
import "sync"
import "sync/atomic"
import "fmt"
import (
	"math/rand"
	"strconv"
	"time"
)

// px.Status() return values, indicating
// whether an agreement has been decided,
// or Paxos has not yet reached agreement,
// or it was agreed but forgotten (i.e. < Min()).
type Fate int

const (
	Decided   Fate = iota + 1
	Pending        // not yet decided.
	Forgotten      // decided but forgotten.
)

const (
	PrintDebug = false
)

type instance struct {
	state Fate        // instance state
	n_p   string      // propose num
	n_a   string      // accept num
	v_a   interface{} // accept value
}

type Paxos struct {
	mu         sync.Mutex
	l          net.Listener
	dead       int32 // for testing
	unreliable int32 // for testing
	rpcCount   int32 // for testing
	peers      []string
	me         int // index into peers[]

	// Your data here.
	dones     []int
	instances map[int]*instance
}

// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the replys contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it does not get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please do not change this function.
func call(srv string, name string, args interface{}, reply interface{}) bool {
	c, err := rpc.Dial("unix", srv)
	if err != nil {
		err1 := err.(*net.OpError)
		if err1.Err != syscall.ENOENT && err1.Err != syscall.ECONNREFUSED {
			//fmt.Printf("paxos Dial() failed: %v\n", err1)
		}
		return false
	}
	defer c.Close()

	err = c.Call(name, args, reply)
	if err == nil {
		return true
	}

	//fmt.Println(err)
	return false
}

// RPC handler
func (px *Paxos) Prepare(args *PrepareArgs, reply *PrepareReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()

	instance, exist := px.instances[args.Seq]
	if !exist {
		px.instances[args.Seq] = px.newInstance()
		instance, _ = px.instances[args.Seq]
		reply.Err = OK
	} else {
		if args.PNum > instance.n_p {
			reply.Err = OK
		} else {
			reply.Err = Reject
		}
	}

	if reply.Err == OK {
		if PrintDebug {
			fmt.Printf("%s:%d accept prepare\n", px.peers[px.me], args.Seq)
		}
		reply.AcceptPnum = instance.n_a
		reply.AcceptValue = instance.v_a

		px.instances[args.Seq].n_p = args.PNum
	} else {
		if PrintDebug {
			fmt.Printf("%s:%d reject prepare\n", px.peers[px.me], args.Seq)
		}
	}

	return nil
}

func (px *Paxos) Accept(args *AcceptArgs, reply *AcceptReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()

	instance, exist := px.instances[args.Seq]
	if !exist {
		px.instances[args.Seq] = px.newInstance()
		reply.Err = OK
	} else {
		if args.PNum >= instance.n_p {
			reply.Err = OK
		} else {
			reply.Err = Reject
		}
	}

	if reply.Err == OK {
		if PrintDebug {
			fmt.Printf("%s:%d accept accept %v\n", px.peers[px.me], args.Seq, args.Value)
		}
		px.instances[args.Seq].n_a = args.PNum
		px.instances[args.Seq].n_p = args.PNum
		px.instances[args.Seq].v_a = args.Value
	} else {
		if PrintDebug {
			fmt.Printf("%s:%d reject accept %v\n", px.peers[px.me], args.Seq, args.Value)
		}
	}

	return nil
}

func (px *Paxos) Decide(args *DecideArgs, reply *DecideReply) error {
	px.mu.Lock()
	defer px.mu.Unlock()

	if PrintDebug {
		fmt.Printf("%s decide %d:%v\n", px.peers[px.me], args.Seq, args.Value)
	}
	_, exist := px.instances[args.Seq]
	if !exist {
		px.instances[args.Seq] = px.newInstance()
	}

	px.instances[args.Seq].v_a = args.Value
	px.instances[args.Seq].n_a = args.PNum
	px.instances[args.Seq].n_p = args.PNum
	px.instances[args.Seq].state = Decided
	px.dones[args.Me] = args.Done
	return nil
}

// helper functions
func (px *Paxos) newInstance() *instance {
	return &instance{n_a: "", n_p: "", v_a: nil, state: Pending}
}

func (px *Paxos) majority() int {
	return len(px.peers)/2 + 1
}

func (px *Paxos) generatePNum() string {
	begin := time.Date(2015, time.May, 6, 22, 0, 0, 0, time.UTC)
	duration := time.Now().Sub(begin)
	return strconv.FormatInt(duration.Nanoseconds(), 10) + "-" + strconv.Itoa(px.me)
}

func (px *Paxos) sendPrepare(seq int, v interface{}) (bool, string, interface{}) {
	/*生成一个唯一的提案编号*/
	pnum := px.generatePNum()

	if PrintDebug {
		fmt.Printf("%s send prepare %d:%v\n", px.peers[px.me], seq, v)
	}
	/*seq这个变量是用来干嘛的？*/
	arg := PrepareArgs{Seq: seq, PNum: pnum}
	num := 0
	replyPnum := ""
	replyValue := v
	/*这里peers存储字符串具体是什么内容*/
	for i, peer := range px.peers {
		/*传出参数，不进行赋值*/
		var reply = PrepareReply{}
		if i == px.me {
			/*如果是自身，直接调用函数*/
			px.Prepare(&arg, &reply)
		} else {
			/*如果不是自身节点，调用RPC*/
			call(peer, "Paxos.Prepare", &arg, &reply)
		}

		if reply.Err == OK {
			num += 1
			/*如果其他节点相应过更大的编号，则需要修改自身的编号和值*/
			if reply.AcceptPnum > replyPnum {
				replyPnum = reply.AcceptPnum
				replyValue = reply.AcceptValue
			}
		}
	}
	/*是否收到大多数节点的OK相应，以及自身编号，Acceptor中的最大accept值*/
	return num >= px.majority(), pnum, replyValue
}

func (px *Paxos) sendAccept(seq int, pnum string, v interface{}) bool {
	arg := AcceptArgs{Seq: seq, PNum: pnum, Value: v}
	num := 0

	if PrintDebug {
		fmt.Printf("%s send accept %d:%v\n", px.peers[px.me], seq, v)
	}
	for i, peer := range px.peers {
		var reply AcceptReply
		if i == px.me {
			px.Accept(&arg, &reply)
		} else {
			call(peer, "Paxos.Accept", &arg, &reply)
		}

		if reply.Err == OK {
			num += 1
		}
	}

	return num >= px.majority()
}

func (px *Paxos) sendDecide(seq int, pnum string, v interface{}) {
	px.mu.Lock()
	/*这个seq是索引吗？记录每个索引的结果*/
	px.instances[seq].state = Decided
	px.instances[seq].n_a = pnum
	px.instances[seq].n_p = pnum
	px.instances[seq].v_a = v
	px.mu.Unlock()

	if PrintDebug {
		fmt.Printf("%s send decide %d:%v\n", px.peers[px.me], seq, v)
	}

	arg := DecideArgs{Seq: seq, PNum: pnum, Value: v, Me: px.me, Done: px.dones[px.me]}
	for i, peer := range px.peers {
		if i == px.me {
			continue
		}
		var reply DecideReply
		call(peer, "Paxos.Decide", &arg, &reply)
	}
}

func (px *Paxos) proposer(seq int, v interface{}) {
	for {
		ok, pnum, value := px.sendPrepare(seq, v)
		if ok {
			ok = px.sendAccept(seq, pnum, value)
		}
		if ok {
			px.sendDecide(seq, pnum, value)
			break
		}
		/*什么时候更新的自身的状态呢？*/
		state, _ := px.Status(seq)
		if state == Decided {
			break
		}
	}
}

// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
func (px *Paxos) Start(seq int, v interface{}) {
	go func() {
		if seq < px.Min() {
			return
		}
		px.proposer(seq, v)
	}()
}

// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
func (px *Paxos) Done(seq int) {
	// Your code here.
	px.mu.Lock()
	defer px.mu.Unlock()

	if seq > px.dones[px.me] {
		px.dones[px.me] = seq
	}
}

// the application wants to know the
// highest instance sequence known to
// this peer.
func (px *Paxos) Max() int {
	// Your code here.
	px.mu.Lock()
	defer px.mu.Unlock()

	max := 0
	for k, _ := range px.instances {
		if k > max {
			max = k
		}
	}

	return max
}

// Min() should return one more than the minimum among z_i,
// where z_i is the highest number ever passed
// to Done() on peer i. A peers z_i is -1 if it has
// never called Done().
//
// Paxos is required to have forgotten all information
// about any instances it knows that are < Min().
// The point is to free up memory in long-running
// Paxos-based servers.
//
// Paxos peers need to exchange their highest Done()
// arguments in order to implement Min(). These
// exchanges can be piggybacked on ordinary Paxos
// agreement protocol messages, so it is OK if one
// peers Min does not reflect another Peers Done()
// until after the next instance is agreed to.
//
// The fact that Min() is defined as a minimum over
// *all* Paxos peers means that Min() cannot increase until
// all peers have been heard from. So if a peer is dead
// or unreachable, other peers Min()s will not increase
// even if all reachable peers call Done. The reason for
// this is that when the unreachable peer comes back to
// life, it will need to catch up on instances that it
// missed -- the other peers therefor cannot forget these
// instances.
func (px *Paxos) Min() int {
	// You code here.

	px.mu.Lock()
	defer px.mu.Unlock()

	min := px.dones[px.me]
	for i := range px.dones {
		if px.dones[i] < min {
			min = px.dones[i]
		}
	}

	for k, instance := range px.instances {
		if k > min {
			continue
		}
		if instance.state != Decided {
			continue
		}

		delete(px.instances, k)
	}

	//fmt.Printf("min: %d\n", min)
	return min + 1
}

// the application wants to know whether this
// peer thinks an instance has been decided,
// and if so what the agreed value is. Status()
// should just inspect the local peer state;
// it should not contact other Paxos peers.
func (px *Paxos) Status(seq int) (Fate, interface{}) {
	// Your code here.
	if seq < px.Min() {
		return Forgotten, nil
	}

	px.mu.Lock()
	defer px.mu.Unlock()

	instance, exist := px.instances[seq]
	if !exist {
		return Pending, nil
	}

	return instance.state, instance.v_a
}

// tell the peer to shut itself down.
// for testing.
// please do not change these two functions.
func (px *Paxos) Kill() {
	atomic.StoreInt32(&px.dead, 1)
	if px.l != nil {
		px.l.Close()
	}
}

// has this peer been asked to shut down?
func (px *Paxos) isdead() bool {
	return atomic.LoadInt32(&px.dead) != 0
}

// please do not change these two functions.
func (px *Paxos) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&px.unreliable, 1)
	} else {
		atomic.StoreInt32(&px.unreliable, 0)
	}
}

func (px *Paxos) isunreliable() bool {
	return atomic.LoadInt32(&px.unreliable) != 0
}

// the application wants to create a paxos peer.
// the ports of all the paxos peers (including this one)
// are in peers[]. this servers port is peers[me].
func Make(peers []string, me int, rpcs *rpc.Server) *Paxos {
	px := &Paxos{}
	px.peers = peers
	px.me = me

	// Your initialization code here.
	px.instances = map[int]*instance{}
	px.dones = make([]int, len(px.peers))
	for i := range px.peers {
		px.dones[i] = -1
	}

	if rpcs != nil {
		// caller will create socket &c
		rpcs.Register(px)
	} else {
		rpcs = rpc.NewServer()
		rpcs.Register(px)

		// prepare to receive connections from clients.
		// change "unix" to "tcp" to use over a network.
		os.Remove(peers[me]) // only needed for "unix"
		l, e := net.Listen("unix", peers[me])
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		px.l = l

		// please do not change any of the following code,
		// or do anything to subvert it.

		// create a thread to accept RPC connections
		go func() {
			for px.isdead() == false {
				conn, err := px.l.Accept()
				if err == nil && px.isdead() == false {
					if px.isunreliable() && (rand.Int63()%1000) < 100 {
						// discard the request.
						conn.Close()
					} else if px.isunreliable() && (rand.Int63()%1000) < 200 {
						// process the request but force discard of reply.
						c1 := conn.(*net.UnixConn)
						f, _ := c1.File()
						err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
						if err != nil {
							fmt.Printf("shutdown: %v\n", err)
						}
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					} else {
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					}
				} else if err == nil {
					conn.Close()
				}
				if err != nil && px.isdead() == false {
					//fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
				}
			}
		}()
	}

	return px
}
