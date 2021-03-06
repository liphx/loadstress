package main

import (
	"flag"
	"sync"
	"context"
	"log"
	"time"
	"fmt"
	"sync/atomic"

	"loadstress/client"
	"loadstress/messages"
	_ "loadstress/client/grpc"

	"github.com/sirupsen/logrus"
)

const (
	STATUS_INT int32 = iota
	STATUS_STARTING
	STATUS_RUNNING
	STATUS_STOPPING
	STATUS_STOPPED
)

var (
	driverOpts client.DriverOpts
	qps 	= flag.Int("q", 10, "The number of concurrent RPCs in seconds on each connection.")
	numConn   = flag.Int("c", 1, "The number of parallel connections.")
	duration  = flag.Int("d", 60, "Benchmark duration in seconds")
	callTimeout  = flag.Int("t", 2, "Call timeout in seconds.")
	driver_name = flag.String("driver_name", "", "Name of the driver for benchmark profiles.")
	wg	sync.WaitGroup
	mu    sync.Mutex
	testDriver client.Driver
	restulCh = make(chan *loadstress_messages.CallResult, 100000)
	status = STATUS_INT
	logger = logrus.New()
)

func init() {
	flag.StringVar(&driverOpts.Host, "server", "127.0.0.1", "Server to connect to.")
	flag.IntVar(&driverOpts.Port, "port", 50051, "Port to connect to.")
}

func readAllResults(rCh chan *loadstress_messages.CallResult) {
	for {
		select {
		case r := <-rCh:
			fmt.Printf("callId:%d result:%v elapsed:%d ns\n", r.Resp.RespId, r.Status, r.Elapsed)
		default:
			fmt.Printf("result ch emplty.\n")
			return
		}
	}
}

func main() {
	flag.Parse()

	testDriver, _ = client.GetDriver(*driver_name, &driverOpts)
	status = STATUS_RUNNING

	connectContext, connectCancel := context.WithDeadline(context.Background(), time.Now().Add(5*time.Second))
	defer connectCancel()
	css := buildConnections(connectContext)

	deadline := time.Duration(*duration+1) * time.Second
	deadlineContext, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(deadline))
	defer deadlineCancel()

	for _, cs := range css {
		wg.Add(1)
		go runWithConnection(deadlineContext, cs)
	}

	for {
		select {
		case <-deadlineContext.Done():
			stop(deadlineContext.Err())
			wg.Wait()
			readAllResults(restulCh)
			close(restulCh)
			logger.Infof("finished loadstess.")
			fmt.Printf("total calls:%v\n", testDriver.GetID())
			atomic.CompareAndSwapInt32(&status, STATUS_STOPPING, STATUS_STOPPED)
			return
		case r := <-restulCh:
			fmt.Printf("callId:%d result:%v elapsed:%d ns\n", r.Resp.RespId, r.Status, r.Elapsed)
		}
	}

	return
}

func stop(err error) error{
	logger.Infof("stop loadstress because of:%s\n", err)
	if(!atomic.CompareAndSwapInt32(&status, STATUS_RUNNING, STATUS_STOPPING)){
		return nil
	}
	return nil
}

func buildConnections(ctx context.Context) []client.ClientConnection {
	ccs := make([]client.ClientConnection, *numConn)
	var err error

	var optMap = map[string]interface{}{}
	optMap["timeout"] = int64(5)
	opts := client.CreateOpts{
		optMap,
	}

	for i := range ccs {
		ccs[i], err = testDriver.CreateConnection(ctx, &opts)
		if err != nil {
			log.Fatalf("create connection:%d failed:%v.", i, err)
		}
	}
	return ccs
}

func runWithConnection(ctx context.Context, conn client.ClientConnection) error {
	defer wg.Done()

	throttle := time.Tick(time.Second)
	var qwg sync.WaitGroup
	for {
		select {
		case <- ctx.Done():
			return stop(ctx.Err())
		case t := <- throttle:
			fmt.Printf("time omit:%v\n",t)
			qwg.Add(1)
			runQps(ctx, conn, &qwg)
		}
	}

	qwg.Wait()
	return nil
}

func handleCallError(req *loadstress_messages.SimpleRequest, err error) {

}

func sendResult(r *loadstress_messages.CallResult) bool {
	if(atomic.LoadInt32(&status) != STATUS_RUNNING) {
		return false
	}

	select {
		case restulCh <- r:
			return true
		default:
			logger.Warn("result channel full")
			return false
	}
}

func asynCall(ctx context.Context, conn client.ClientConnection, _wg *sync.WaitGroup) error {
	defer _wg.Done()

	timeDuration := time.Duration(*callTimeout)
	timeoutCtx, timeoutCancel := context.WithDeadline(context.Background(), time.Now().Add(timeDuration*time.Second))
	defer timeoutCancel()

	req, err :=  conn.BuildReq()
	if err != nil {
		handleCallError(req, err)
		return err
	}

	resp, _ := conn.Call(timeoutCtx, req)

	callResult, _ := conn.BuildResp(resp)
	sendResult(callResult)

	return nil
}

var g_qps int
func runQps(ctx context.Context, conn client.ClientConnection, _wg *sync.WaitGroup) error {
	defer _wg.Done()
	var qwg sync.WaitGroup

	select {
		case <-ctx.Done():
			return stop(ctx.Err())
		default:
			start := time.Now()
			g_qps++
			for i := 0; i < *qps; i++ {
				qwg.Add(1)
				go asynCall(ctx, conn, &qwg)
			}
			logger.Infof("%d qps cost:%d.\n", g_qps, time.Now().Sub(start))
	}

	qwg.Wait()
	return nil
}
