package kvserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kvservice/v1/common"
	"github.com/kvservice/v1/kvserver/conf"
	"github.com/kvservice/v1/raft"
	"github.com/kvservice/v1/tool"
	uuid "github.com/satori/go.uuid"
	"google.golang.org/grpc"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

const DefaultSessionTimeout = 60 * 60 // 1h

const SessionIdSeparator = "-"

const DefaultServerPort = 8080

type ApplyResult struct {
	Term      int
	SessionId string
}

type KVServer struct {
	// initialize when starting
	common.KVServerServer
	mu         sync.Mutex
	rf         *raft.Raft
	applyCh    chan raft.ApplyMsg
	storage    *tool.Storage
	replyChan  map[int]chan ApplyResult
	sessionMap map[string]time.Time

	// persistent
	uniqueId    int64
	commitIndex int
	tab         map[string]string

	// configurable
	me                int
	password          string
	maxRaftState      int
	nextSnapshotIndex int
	logEnabled        bool
	sessionTimeout    time.Duration
}

//
// StartKVServer starts a key/value server for rpc call which using raft to keep consistency
//
func StartKVServer(config []byte) (*KVServer, error) {
	serviceConf, err := conf.ReadConf(config)
	if err != nil {
		return nil, err
	}
	kvServerConf := serviceConf.KVServer
	if err != nil {
		return nil, &tool.RuntimeError{Stage: "load KVService config", Err: err}
	}
	port := kvServerConf.Port
	if port < 0 {
		return nil, &tool.RuntimeError{Stage: "configure KVServer", Err: errors.New("KVServer port " + strconv.Itoa(port) + " is invalid")}
	} else if port == 0 {
		port = DefaultServerPort
	}
	me := serviceConf.Me
	storage, err := tool.MakeStorage(me)
	if err != nil {
		return nil, &tool.RuntimeError{Stage: "make storage", Err: err}
	}

	kv := new(KVServer)
	kv.me = me
	kv.storage = storage
	applyCh := make(chan raft.ApplyMsg)
	kv.applyCh = applyCh
	kv.replyChan = make(map[int]chan ApplyResult)
	kv.sessionMap = make(map[string]time.Time)

	snapshot := storage.ReadSnapshot()
	if nil != snapshot && len(snapshot) > 0 {
		err = kv.recoverFrom(snapshot)
		if err != nil {
			return nil, &tool.RuntimeError{Stage: "restore KVServer snapshot", Err: err}
		}
	} else {
		kv.uniqueId = 1
		kv.tab = make(map[string]string)
		kv.commitIndex = 0
	}

	// apply configuration
	maxRaftState := kvServerConf.MaxRaftState
	var nextSnapshotIndex int
	if maxRaftState > 0 {
		nextSnapshotIndex = kv.commitIndex + maxRaftState
		log.Printf("configure KVServer info: KVServer will make a snapshot per %d operations", maxRaftState)
	} else {
		log.Println("configure KVServer info: KVServer won't make snapshot")
		nextSnapshotIndex = -1
		maxRaftState = -1
	}

	sessionTimeout := kvServerConf.SessionTimeout
	if sessionTimeout >= 0 {
		if sessionTimeout == 0 {
			log.Println("configure KVServer info: sessionTimeout is not set, use default")
			sessionTimeout = DefaultSessionTimeout
		}
		log.Printf("configure KVServer info: any idle sessions will expired after %d s", sessionTimeout)
	} else {
		log.Println("configure KVServer info: session will never expire")
	}

	kv.password = kvServerConf.Password
	kv.maxRaftState = maxRaftState
	kv.nextSnapshotIndex = nextSnapshotIndex
	kv.sessionTimeout = time.Duration(sessionTimeout) * time.Second
	if kvServerConf.LogEnabled {
		kv.logEnabled = true
		log.Println("configure KVServer info: enable service log")
	}

	// start listener
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		err = &tool.RuntimeError{Stage: "start KVServer", Err: err}
		return nil, err
	}

	// initialize kvserver success, start raft
	raftConf := serviceConf.Raft
	kv.rf, err = raft.StartRaft(me, storage, applyCh, raftConf)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}

	go kv.apply()
	if sessionTimeout > 0 {
		go kv.cleanupSessions()
	}

	// start grpc server
	server := grpc.NewServer()
	common.RegisterKVServerServer(server, kv)
	go func() {
		_ = server.Serve(listener)
	}()

	log.Println("start KVServer success, serves at port:", port)
	return kv, nil
}

func (kv *KVServer) logPrintf(format string, v ...interface{}) {
	if kv.logEnabled {
		prefix := fmt.Sprintf("[%d]: ", kv.me)
		log.Printf(prefix+format, v...)
	}
}

func (kv *KVServer) OpenSession(_ context.Context, request *common.OpenSessionRequest) (*common.OpenSessionReply, error) {
	kv.logPrintf("receive OpenSession request from client")
	reply := &common.OpenSessionReply{}
	reply.SessionId = ""
	if request.RequestType != common.RequestType_OPEN_SESSION {
		reply.ErrCode = common.ErrCode_INVALID_REQUEST_TYPE
		return reply, nil
	}
	if request.GetPassword() != kv.password {
		reply.ErrCode = common.ErrCode_INVALID_PASSWORD
		return reply, nil
	}
	command := raft.Op{
		RequestType: common.RequestType_OPEN_SESSION,
		Key:         "",
		Value:       "",
		UUID:        uuid.NewV4().String(),
	}
	applyResult, errCode := kv.submit(command)
	if errCode == common.ErrCode_OK {
		sessionId := applyResult.SessionId
		reply.SessionId = sessionId
		reply.ErrCode = common.ErrCode_OK
		kv.logPrintf("OpenSession request finished, sessionId=%s", sessionId)
	} else {
		reply.ErrCode = errCode
		kv.logPrintf("OpenSession request fail to finish, errCode=%s", errCode.String())
	}
	return reply, nil
}

func (kv *KVServer) Get(_ context.Context, args *common.GetRequest) (*common.GetReply, error) {
	key := args.Key
	sessionId := args.SessionId
	kv.logPrintf("receive Get request from client, key=%s, sessionId=%s, ", key, sessionId)
	reply := &common.GetReply{}
	if args.RequestType != common.RequestType_GET {
		reply.ErrCode = common.ErrCode_INVALID_REQUEST_TYPE
		return reply, nil
	}
	_, isLeader := kv.rf.GetState()
	if !isLeader {
		reply.ErrCode = common.ErrCode_WRONG_LEADER
		return reply, nil
	}
	if !kv.checkSession(args.SessionId) {
		reply.ErrCode = common.ErrCode_INVALID_SESSION
		return reply, nil
	}
	command := raft.Op{
		RequestType: common.RequestType_GET,
		Key:         args.Key,
		Value:       "",
	}
	_, errCode := kv.submit(command)
	if errCode == common.ErrCode_OK {
		// ??????????????????????????????????????????data race
		var value string
		if v, exist := kv.tab[args.Key]; !exist {
			reply.ErrCode = common.ErrCode_NO_KEY
			value = ""
		} else {
			reply.ErrCode = common.ErrCode_OK
			value = v
		}
		reply.Value = value
		kv.logPrintf("Get request finished, key=%s, value=%s, errCode=%s, sessionId=%s", key, value, errCode.String(), sessionId)
	} else {
		reply.ErrCode = errCode
		kv.logPrintf("Get request fail to finish, errCode=%s, sessionId=%s", errCode.String(), sessionId)
	}

	return reply, nil
}

func (kv *KVServer) Update(_ context.Context, args *common.UpdateRequest) (*common.UpdateReply, error) {
	sessionId := args.SessionId
	reqType := args.RequestType
	kv.logPrintf("receive %s request from client, sessionId=%s", reqType, sessionId)
	reply := &common.UpdateReply{}
	if reqType != common.RequestType_PUT && reqType != common.RequestType_APPEND && reqType != common.RequestType_DELETE {
		reply.ErrCode = common.ErrCode_INVALID_REQUEST_TYPE
		return reply, nil
	}
	_, isLeader := kv.rf.GetState()
	if !isLeader {
		reply.ErrCode = common.ErrCode_WRONG_LEADER
		return reply, nil
	}
	if !kv.checkSession(args.SessionId) {
		reply.ErrCode = common.ErrCode_INVALID_SESSION
		return reply, nil
	}

	key := args.Key
	value := args.Value
	command := raft.Op{
		RequestType: reqType,
		Key:         key,
		Value:       value,
	}
	_, errCode := kv.submit(command)
	reply.ErrCode = errCode
	if errCode == common.ErrCode_OK {
		kv.logPrintf("%s request finished, key=%s, value=%s, errCode=%s, sessionId=%s", reqType, key, value, errCode.String(), sessionId)
	} else {
		kv.logPrintf("%s fail to finish, key=%s, value=%s, errCode=%s, sessionId=%s", reqType, key, value, errCode.String(), sessionId)
	}
	return reply, nil
}

func (kv *KVServer) checkSession(sessionId string) bool {
	kv.mu.Lock()
	if _, valid := kv.sessionMap[sessionId]; valid {
		kv.sessionMap[sessionId] = time.Now()
		kv.mu.Unlock()
		return true
	}
	kv.mu.Unlock()
	return false
}

// submit
// Submits an Op to Raft
// If the submitted command reaches consensus successfully, the pointer points to the corresponding ApplyResult will be returned;
// and the pb.ErrCode is pb.ErrCode_OK
// otherwise, the returned *ApplyResult would be nil and the ErrCode indicating the reason why the command fails to complete
func (kv *KVServer) submit(op raft.Op) (*ApplyResult, common.ErrCode) {
	// start????????????????????????????????????????????? KVServer ???????????? raft ??? log
	// start?????????????????????????????????????????? raft ??????????????????
	commandIndex, commandTerm, isLeader := kv.rf.Start(op)
	if !isLeader {
		return nil, common.ErrCode_WRONG_LEADER
	}
	// leader1(current leader) may be partitioned by itself,
	// its log may be trimmed by leader2 (if and only if leader2's term > leader1's term)
	// but len(leader2's log) may less than len(leader1's log)
	// if leader1 becomes leader again, then commands submitted later may get the same log index
	// that's to say, previously submitted commands will never be completed
	kv.mu.Lock()
	if c, _ := kv.replyChan[commandIndex]; c != nil {
		// tell the previous client to stop waiting
		c <- ApplyResult{Term: commandTerm}
		close(c)
	}
	ch := make(chan ApplyResult, 1)
	kv.replyChan[commandIndex] = ch
	kv.mu.Unlock()

	res := <-ch
	// log's index and term identifies the unique log
	if res.Term == commandTerm {
		return &res, common.ErrCode_OK
	} else {
		return nil, common.ErrCode_WRONG_LEADER
	}
}

func (kv *KVServer) makeSnapshot() ([]byte, error) {
	w := new(bytes.Buffer)
	//e := gob.NewEncoder(w)
	e := json.NewEncoder(w)
	err := e.Encode(kv.uniqueId)
	if err != nil {
		return nil, errors.New("encode uniqueId fails: " + err.Error())
	}
	err = e.Encode(kv.commitIndex)
	if err != nil {
		return nil, errors.New("encode commitIndex fails: " + err.Error())
	}
	err = e.Encode(kv.tab)
	if err != nil {
		return nil, errors.New("encode tab fails: " + err.Error())
	}
	return w.Bytes(), nil
}

func (kv *KVServer) recoverFrom(snapshot []byte) error {
	if snapshot == nil || len(snapshot) < 1 {
		return nil
	}
	r := bytes.NewBuffer(snapshot)
	d := json.NewDecoder(r)
	var tab map[string]string
	var nextClientId int64
	var commitIndex int
	var err error
	if err = d.Decode(&nextClientId); err != nil {
		return errors.New("recover from snapshot: decode currentTerm fails: " + err.Error())
	}
	if err = d.Decode(&commitIndex); err != nil {
		return errors.New("recover from snapshot: decode commitIndex fails: " + err.Error())
	}
	if err = d.Decode(&tab); err != nil {
		return errors.New("recover from snapshot: decode tab fails: " + err.Error())
	}
	kv.uniqueId = nextClientId
	kv.commitIndex = commitIndex
	kv.tab = tab
	return nil
}

// startApply listen to the log sent from applyCh and execute the corresponding command.
// The same command will only be executed once
func (kv *KVServer) apply() {
	for {
		msg := <-kv.applyCh
		if msg.CommandValid {
			commandIndex := msg.CommandIndex
			commandTerm := msg.CommandTerm
			kv.logPrintf("receive log, commandIndex=%d, commandTerm=%d", commandIndex, commandTerm)
			kv.mu.Lock()
			if commandIndex != kv.commitIndex+1 {
				kv.mu.Unlock()
				log.Printf("warning: ignore out dated log, expected log index %d, but receive %d", kv.commitIndex+1, commandIndex)
				continue
			}
			kv.commitIndex = commandIndex
			op := msg.Command
			commandType := op.RequestType
			sessionId := ""
			if common.RequestType_OPEN_SESSION == commandType {
				sessionId = strconv.FormatInt(kv.uniqueId, 10) + SessionIdSeparator + op.UUID
				kv.sessionMap[sessionId] = time.Now()
				kv.uniqueId++
				kv.logPrintf("open a new session, sessionId=%s", sessionId)
			} else if common.RequestType_PUT == commandType {
				kv.tab[op.Key] = op.Value
				kv.logPrintf("put value %s on key=%v", op.Value, op.Key)
			} else if common.RequestType_APPEND == commandType {
				v := kv.tab[op.Key]
				v += op.Value
				kv.tab[op.Key] = v
				kv.logPrintf("append value %s on key=%s, now value is %s", op.Value, op.Key, v)
			} else if common.RequestType_DELETE == commandType {
				delete(kv.tab, op.Key)
				kv.logPrintf("delete key=%v", op.Key)
			} else if common.RequestType_GET != commandType {
				log.Printf("warning: receive unknown request type, opType=%s", op.RequestType)
			}
			if ch, _ := kv.replyChan[commandIndex]; ch != nil {
				kv.logPrintf("send apply result to commandIndex=%d, commandTerm=%d", commandIndex, commandTerm)
				ch <- ApplyResult{
					SessionId: sessionId,
					Term:      commandTerm,
				}
				close(ch)
				delete(kv.replyChan, commandIndex)
				kv.logPrintf("close commandIndex=%d channel", commandIndex)
			}
			if kv.maxRaftState > 0 && commandIndex == kv.nextSnapshotIndex {
				kv.nextSnapshotIndex = commandIndex + kv.maxRaftState
				snapshot, err := kv.makeSnapshot()
				if err != nil {
					err = &tool.RuntimeError{Stage: "make snapshot", Err: err}
					panic(err.Error())
				}
				kv.rf.Snapshot(commandIndex, snapshot)
				kv.logPrintf("make snapshot success, lastIncludedIndex=%d", commandIndex)
			}
			kv.mu.Unlock()
		} else if msg.SnapshotValid {
			kv.mu.Lock()
			err := kv.recoverFrom(msg.Snapshot)
			if err != nil {
				panic(err.Error())
			}
			if kv.commitIndex != msg.SnapshotIndex {
				log.Printf("warning: commitIndex in snapshot is %d but raft snapshot index is %d", kv.commitIndex, msg.SnapshotIndex)
			}
			kv.mu.Unlock()
		} else {
			log.Printf("warning: receive unknown type log, log content: %v", msg)
		}
	}
}

// cleanupSessions scans the sessionMap to clean up the records corresponding to the expired session
func (kv *KVServer) cleanupSessions() {
	timeout := kv.sessionTimeout
	for {
		time.Sleep(timeout)
		kv.mu.Lock()
		var expiredSessions []string

		for sessionId, lastVisitTime := range kv.sessionMap {
			if time.Since(lastVisitTime) >= timeout {
				expiredSessions = append(expiredSessions, sessionId)
			}
		}
		for _, s := range expiredSessions {
			delete(kv.sessionMap, s)
		}
		kv.mu.Unlock()
	}
}
