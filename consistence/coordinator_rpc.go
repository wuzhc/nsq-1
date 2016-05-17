package consistence

import (
	"github.com/absolute8511/nsq/nsqd"
	// TODO: use the gorpc for performance and timeout
	_ "github.com/valyala/gorpc"
	"io"
	"net"
	"net/rpc"
	"os"
	"runtime"
)

type ErrRPCRetCode int

const (
	RpcNoErr ErrRPCRetCode = iota
	RpcCommonErr
)

const (
	RpcErrLeavingISRWait ErrRPCRetCode = iota + 10
	RpcErrNotTopicLeader
	RpcErrNoLeader
	RpcErrEpochMismatch
	RpcErrEpochLessThanCurrent
	RpcErrWriteQuorumFailed
	RpcErrCommitLogIDDup
	RpcErrCommitLogEOF
	RpcErrCommitLogOutofBound
	RpcErrMissingTopicLeaderSession
	RpcErrLeaderSessionMismatch
	RpcErrWriteDisabled
	RpcErrTopicNotExist
	RpcErrMissingTopicCoord
	RpcErrTopicCoordExistingAndMismatch
	RpcErrTopicLeaderChanged
	RpcErrTopicLoading
	RpcErrSlaveStateInvalid
	RpcErrTopicCoordStateInvalid
	RpcErrWriteOnNonISR
)

type NsqdNodeLoadFactor struct {
	nodeLF        float32
	topicLeaderLF map[string]map[int]float32
	topicSlaveLF  map[string]map[int]float32
}

type RpcAdminTopicInfo struct {
	TopicPartitionMetaInfo
	LookupdEpoch EpochType
	DisableWrite bool
}

type RpcAcquireTopicLeaderReq struct {
	RpcTopicData
	LeaderNodeID string
	LookupdEpoch EpochType
}

type RpcTopicLeaderSession struct {
	RpcTopicData
	LeaderNode   NsqdNodeInfo
	LookupdEpoch EpochType
	JoinSession  string
}

type NsqdCoordRpcServer struct {
	nsqdCoord      *NsqdCoordinator
	rpcListener    net.Listener
	rpcServer      *rpc.Server
	dataRootPath   string
	disableRpcTest bool
}

func NewNsqdCoordRpcServer(coord *NsqdCoordinator, rootPath string) *NsqdCoordRpcServer {
	return &NsqdCoordRpcServer{
		nsqdCoord:    coord,
		rpcServer:    rpc.NewServer(),
		dataRootPath: rootPath,
	}
}

// used only for test
func (self *NsqdCoordRpcServer) toggleDisableRpcTest(disable bool) {
	self.disableRpcTest = disable
	coordLog.Infof("rpc is disabled on node: %v", self.nsqdCoord.myNode.GetID())
}

func (self *NsqdCoordRpcServer) start(ip, port string) error {
	e := self.rpcServer.Register(self)
	if e != nil {
		panic(e)
	}
	self.rpcListener, e = net.Listen("tcp4", ip+":"+port)
	if e != nil {
		coordLog.Errorf("listen rpc error : %v", e.Error())
		os.Exit(1)
	}

	coordLog.Infof("nsqd coordinator rpc listen at : %v", self.rpcListener.Addr())
	self.rpcServer.Accept(self.rpcListener)
	return nil
}

func (self *NsqdCoordRpcServer) stop() {
	if self.rpcListener != nil {
		self.rpcListener.Close()
	}
}

func (self *NsqdCoordRpcServer) checkLookupForWrite(lookupEpoch EpochType) *CoordErr {
	if lookupEpoch < self.nsqdCoord.lookupLeader.Epoch {
		coordLog.Warningf("the lookupd epoch is smaller than last: %v", lookupEpoch)
		return ErrEpochMismatch
	}
	if self.disableRpcTest {
		return &CoordErr{"rpc disabled", RpcCommonErr, CoordNetErr}
	}
	return nil
}

func (self *NsqdCoordRpcServer) NotifyTopicLeaderSession(rpcTopicReq RpcTopicLeaderSession, ret *CoordErr) error {
	if err := self.checkLookupForWrite(rpcTopicReq.LookupdEpoch); err != nil {
		*ret = *err
		return nil
	}
	coordLog.Infof("got leader session notify : %v, leader node info:%v on node: %v", rpcTopicReq, rpcTopicReq.LeaderNode, self.nsqdCoord.myNode.GetID())
	topicCoord, err := self.nsqdCoord.getTopicCoord(rpcTopicReq.TopicName, rpcTopicReq.TopicPartition)
	if err != nil {
		coordLog.Infof("topic partition missing.")
		*ret = *err
		return nil
	}
	if rpcTopicReq.JoinSession != "" && !topicCoord.GetData().disableWrite {
		if FindSlice(topicCoord.topicInfo.ISR, self.nsqdCoord.myNode.GetID()) != -1 {
			coordLog.Errorf("join session should disable write first")
			*ret = *ErrTopicCoordStateInvalid
			return nil
		}
	}
	newSession := &TopicLeaderSession{
		LeaderNode:  &rpcTopicReq.LeaderNode,
		Session:     rpcTopicReq.TopicLeaderSession,
		LeaderEpoch: rpcTopicReq.TopicLeaderSessionEpoch,
	}
	err = self.nsqdCoord.updateTopicLeaderSession(topicCoord, newSession, rpcTopicReq.JoinSession)
	if err != nil {
		*ret = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) NotifyAcquireTopicLeader(rpcTopicReq RpcAcquireTopicLeaderReq, ret *CoordErr) error {
	if err := self.checkLookupForWrite(rpcTopicReq.LookupdEpoch); err != nil {
		*ret = *err
		return nil
	}
	if rpcTopicReq.TopicPartition < 0 || rpcTopicReq.TopicName == "" {
		return ErrTopicArgError
	}
	topicCoord, err := self.nsqdCoord.getTopicCoord(rpcTopicReq.TopicName, rpcTopicReq.TopicPartition)
	if err != nil {
		coordLog.Infof("topic partition missing.")
		*ret = *err
		return nil
	}
	tcData := topicCoord.GetData()
	if tcData.GetTopicEpoch() != rpcTopicReq.TopicEpoch ||
		tcData.GetLeader() != rpcTopicReq.LeaderNodeID ||
		tcData.GetLeader() != self.nsqdCoord.myNode.GetID() {
		coordLog.Infof("not topic leader while acquire leader: %v, %v", tcData, rpcTopicReq)
		return nil
	}
	err = self.nsqdCoord.notifyAcquireTopicLeader(tcData)
	if err != nil {
		*ret = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) UpdateTopicInfo(rpcTopicReq RpcAdminTopicInfo, ret *CoordErr) error {
	if err := self.checkLookupForWrite(rpcTopicReq.LookupdEpoch); err != nil {
		*ret = *err
		return nil
	}
	coordLog.Infof("got update request for topic : %v on node: %v", rpcTopicReq, self.nsqdCoord.myNode.GetID())
	if rpcTopicReq.Partition < 0 || rpcTopicReq.Name == "" {
		return ErrTopicArgError
	}

	self.nsqdCoord.coordMutex.Lock()
	coords, ok := self.nsqdCoord.topicCoords[rpcTopicReq.Name]
	myID := self.nsqdCoord.myNode.GetID()
	if rpcTopicReq.Leader == myID {
		for pid, tc := range coords {
			if tc.GetData().GetLeader() != myID {
				continue
			}
			if pid != rpcTopicReq.Partition {
				coordLog.Infof("found another partition %v already exist master for this topic %v", pid, rpcTopicReq.Name)
				if _, err := self.nsqdCoord.localNsqd.GetExistingTopic(rpcTopicReq.Name, rpcTopicReq.Partition); err != nil {
					coordLog.Infof("local no such topic, we can just remove this coord")
					tc.logMgr.Close()
					delete(coords, pid)
					continue
				}
				self.nsqdCoord.coordMutex.Unlock()
				*ret = *ErrTopicCoordExistingAndMismatch
				return nil
			}
		}
	}
	if rpcTopicReq.Leader != myID &&
		FindSlice(rpcTopicReq.ISR, myID) == -1 &&
		FindSlice(rpcTopicReq.CatchupList, myID) == -1 {
		// a topic info not belong to me,
		// check if we need to delete local
		coordLog.Infof("Not a topic(%s) related to me. isr is : %v", rpcTopicReq.Name, rpcTopicReq.ISR)
		if ok {
			tc, ok := coords[rpcTopicReq.Partition]
			if ok {
				if tc.GetData().topicInfo.Leader == myID {
					self.nsqdCoord.releaseTopicLeader(&tc.GetData().topicInfo, &tc.topicLeaderSession)
				}
				self.nsqdCoord.localNsqd.CloseExistingTopic(rpcTopicReq.Name, rpcTopicReq.Partition)
				coordLog.Infof("topic(%s) is removing from local node since not related", rpcTopicReq.Name)
				tc.logMgr.Close()
				delete(coords, rpcTopicReq.Partition)
			}
		}
		self.nsqdCoord.coordMutex.Unlock()
		return nil
	}
	if !ok {
		coords = make(map[int]*TopicCoordinator)
		self.nsqdCoord.topicCoords[rpcTopicReq.Name] = coords
	}
	tpCoord, ok := coords[rpcTopicReq.Partition]
	if !ok {
		var localErr error
		tpCoord, localErr = NewTopicCoordinator(rpcTopicReq.Name, rpcTopicReq.Partition,
			GetTopicPartitionBasePath(self.dataRootPath, rpcTopicReq.Name, rpcTopicReq.Partition), rpcTopicReq.SyncEvery)
		if localErr != nil || tpCoord == nil {
			self.nsqdCoord.coordMutex.Unlock()
			*ret = *ErrLocalInitTopicCoordFailed
			return nil
		}
		tpCoord.disableWrite = true
		coords[rpcTopicReq.Partition] = tpCoord
		rpcTopicReq.DisableWrite = true
		coordLog.Infof("A new topic coord init on the node: %v", rpcTopicReq.GetTopicDesp())
	}

	self.nsqdCoord.coordMutex.Unlock()
	err := self.nsqdCoord.updateTopicInfo(tpCoord, rpcTopicReq.DisableWrite, &rpcTopicReq.TopicPartitionMetaInfo)
	if err != nil {
		*ret = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) EnableTopicWrite(rpcTopicReq RpcAdminTopicInfo, ret *CoordErr) error {
	// set the topic as not writable.
	coordLog.Infof("got enable write for topic : %v", rpcTopicReq)
	if err := self.checkLookupForWrite(rpcTopicReq.LookupdEpoch); err != nil {
		*ret = *err
		return nil
	}
	tp, err := self.nsqdCoord.getTopicCoord(rpcTopicReq.Name, rpcTopicReq.Partition)
	if err != nil {
		*ret = *err
		return nil
	}
	err = tp.DisableWriteWithTimeout(false)
	if err != nil {
		*ret = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) DisableTopicWrite(rpcTopicReq RpcAdminTopicInfo, ret *CoordErr) error {
	coordLog.Infof("got disable write for topic : %v", rpcTopicReq)
	if err := self.checkLookupForWrite(rpcTopicReq.LookupdEpoch); err != nil {
		*ret = *err
		return nil
	}

	tp, err := self.nsqdCoord.getTopicCoord(rpcTopicReq.Name, rpcTopicReq.Partition)
	if err != nil {
		*ret = *err
		return nil
	}
	err = tp.DisableWriteWithTimeout(true)
	if err != nil {
		*ret = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) IsTopicWriteDisabled(rpcTopicReq RpcAdminTopicInfo, ret *bool) error {
	tp, err := self.nsqdCoord.getTopicCoord(rpcTopicReq.Name, rpcTopicReq.Partition)
	if err != nil {
		*ret = false
		return nil
	}
	*ret = tp.GetData().disableWrite
	return nil
}

func (self *NsqdCoordRpcServer) GetTopicStats(topic string, stat *NodeTopicStats) error {
	// TODO: get local coordinator stats and errors, get local topic data stats
	if topic == "" {
		// all topic status
		topicStats := self.nsqdCoord.localNsqd.GetStats()
		stat.NodeID = self.nsqdCoord.myNode.GetID()
		stat.ChannelDepthData = make(map[string]int64, len(topicStats))
		stat.ChannelHourlyConsumedData = make(map[string]int64, len(topicStats))
		stat.TopicLeaderDataSize = make(map[string]int64, len(topicStats))
		stat.TopicTotalDataSize = make(map[string]int64, len(topicStats))
		stat.TopicPubQPS = nil
		stat.TopicChannelSubQPS = nil
		stat.NodeCPUs = runtime.NumCPU()
		for _, ts := range topicStats {
			// plus 1 to handle the empty topic/channel
			stat.TopicTotalDataSize[ts.TopicName] = ts.BackendDepth/1024/1024 + 1
			if ts.IsLeader {
				stat.TopicLeaderDataSize[ts.TopicName] = ts.BackendDepth/1024/1024 + 1
				for _, chStat := range ts.Channels {
					stat.ChannelDepthData[ts.TopicName] += chStat.BackendDepth/1024/1024 + 1
					stat.ChannelHourlyConsumedData[ts.TopicName] += chStat.HourlySubSize / 1024 / 1024
				}
			}
		}
	}
	// the status of specific topic
	return nil
}

type RpcTopicData struct {
	TopicName               string
	TopicPartition          int
	TopicEpoch              EpochType
	TopicLeaderSessionEpoch EpochType
	TopicLeaderSession      string
}

type RpcChannelOffsetArg struct {
	RpcTopicData
	Channel string
	// position file + file offset
	ChannelOffset ChannelConsumerOffset
}

type RpcPutMessages struct {
	RpcTopicData
	LogData       CommitLogData
	TopicMessages []*nsqd.Message
}

type RpcPutMessage struct {
	RpcTopicData
	LogData      CommitLogData
	TopicMessage *nsqd.Message
}

type RpcCommitLogReq struct {
	RpcTopicData
	LogOffset int64
}

type RpcCommitLogRsp struct {
	LogOffset int64
	LogData   CommitLogData
	ErrInfo   CoordErr
}

type RpcPullCommitLogsReq struct {
	RpcTopicData
	StartLogOffset int64
	LogMaxNum      int
}

type RpcPullCommitLogsRsp struct {
	Logs     []CommitLogData
	DataList [][]byte
}

type RpcTestReq struct {
	Data string
}

type RpcTestRsp struct {
	RspData string
	RetErr  *CoordErr
}

func (self *NsqdCoordinator) checkForRpcCall(rpcData RpcTopicData) (*TopicCoordinator, *CoordErr) {
	topicCoord, err := self.getTopicCoord(rpcData.TopicName, rpcData.TopicPartition)
	if err != nil || topicCoord == nil {
		coordLog.Infof("rpc call with missing topic :%v", rpcData)
		return nil, ErrMissingTopicCoord
	}
	coordLog.Debugf("checking rpc call...")
	tcData := topicCoord.GetData()
	if tcData.GetTopicEpoch() != rpcData.TopicEpoch {
		coordLog.Infof("rpc call with wrong epoch :%v", rpcData)
		return nil, ErrEpochMismatch
	}
	if tcData.GetLeaderSession() != rpcData.TopicLeaderSession {
		coordLog.Infof("rpc call with wrong session:%v", rpcData)
		return nil, ErrLeaderSessionMismatch
	}
	if !tcData.localDataLoaded {
		coordLog.Infof("local data is still loading. %v", tcData.topicInfo.GetTopicDesp())
		return nil, ErrTopicLoading
	}
	return topicCoord, nil
}

func (self *NsqdCoordRpcServer) UpdateChannelOffset(info RpcChannelOffsetArg, retErr *CoordErr) error {
	tc, err := self.nsqdCoord.checkForRpcCall(info.RpcTopicData)
	if err != nil {
		*retErr = *err
		return nil
	}
	// update local channel offset
	err = self.nsqdCoord.updateChannelOffsetOnSlave(tc.GetData(), info.Channel, info.ChannelOffset)
	if err != nil {
		*retErr = *err
	}
	return nil
}

// receive from leader
func (self *NsqdCoordRpcServer) PutMessage(info RpcPutMessage, retErr *CoordErr) error {
	tc, err := self.nsqdCoord.checkForRpcCall(info.RpcTopicData)
	if err != nil {
		*retErr = *err
		return nil
	}
	// do local pub message
	err = self.nsqdCoord.putMessageOnSlave(tc, info.LogData, info.TopicMessage)
	if err != nil {
		*retErr = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) PutMessages(info RpcPutMessages, retErr *CoordErr) error {
	tc, err := self.nsqdCoord.checkForRpcCall(info.RpcTopicData)
	if err != nil {
		*retErr = *err
		return nil
	}
	// do local pub message
	err = self.nsqdCoord.putMessagesOnSlave(tc, info.LogData, info.TopicMessages)
	if err != nil {
		*retErr = *err
	}
	return nil
}

func (self *NsqdCoordRpcServer) GetLastCommitLogID(req RpcCommitLogReq, ret *int64) error {
	*ret = 0
	tc, err := self.nsqdCoord.getTopicCoordData(req.TopicName, req.TopicPartition)
	if err != nil {
		return err
	}
	*ret = tc.logMgr.GetLastCommitLogID()
	return nil
}

// return the logdata from offset, if the offset is larger than local,
// then return the last logdata on local.
func (self *NsqdCoordRpcServer) GetCommitLogFromOffset(req RpcCommitLogReq, ret *RpcCommitLogRsp) error {
	tcData, coorderr := self.nsqdCoord.getTopicCoordData(req.TopicName, req.TopicPartition)
	if coorderr != nil {
		ret.ErrInfo = *coorderr
		return nil
	}
	logData, err := tcData.logMgr.GetCommitLogFromOffset(req.LogOffset)
	if err != nil {
		var err2 error
		ret.LogOffset, err2 = tcData.logMgr.GetLastLogOffset()
		if err2 != nil {
			ret.ErrInfo = *NewCoordErr(err2.Error(), CoordCommonErr)
			return nil
		}
		logData, err2 = tcData.logMgr.GetCommitLogFromOffset(ret.LogOffset)

		if err2 != nil {
			if err2 == ErrCommitLogEOF {
				ret.ErrInfo = *ErrTopicCommitLogEOF
			} else {
				ret.ErrInfo = *NewCoordErr(err.Error(), CoordCommonErr)
			}
			return nil
		}
		ret.LogData = *logData
		if err == ErrCommitLogOutofBound {
			ret.ErrInfo = *ErrTopicCommitLogOutofBound
		} else if err == ErrCommitLogEOF {
			ret.ErrInfo = *ErrTopicCommitLogEOF
		} else {
			ret.ErrInfo = *NewCoordErr(err.Error(), CoordCommonErr)
		}
		return nil
	} else {
		ret.LogOffset = req.LogOffset
		ret.LogData = *logData
	}
	return nil
}

func (self *NsqdCoordRpcServer) PullCommitLogsAndData(req RpcPullCommitLogsReq, ret *RpcPullCommitLogsRsp) error {
	tcData, err := self.nsqdCoord.getTopicCoordData(req.TopicName, req.TopicPartition)
	if err != nil {
		return err
	}

	ret.DataList = make([][]byte, 0, len(ret.Logs))
	var localErr error
	ret.Logs, localErr = tcData.logMgr.GetCommitLogs(req.StartLogOffset, req.LogMaxNum)
	if localErr != nil {
		if localErr == io.EOF {
			return nil
		}
		return localErr
	}
	for _, l := range ret.Logs {
		d, err := self.nsqdCoord.readTopicRawData(tcData.topicInfo.Name, tcData.topicInfo.Partition, l.MsgOffset, l.MsgSize)
		if err != nil {
			return err
		}
		ret.DataList = append(ret.DataList, d)
	}
	return nil
}

func (self *NsqdCoordRpcServer) TestRpcError(req RpcTestReq, ret *RpcTestRsp) error {
	ret.RspData = req.Data
	ret.RetErr = &CoordErr{
		ErrMsg:  req.Data,
		ErrCode: RpcNoErr,
		ErrType: CoordCommonErr,
	}
	return nil
}
