package healer

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"
)

type GroupConsumer struct {
	// TODO do not nedd one connection to each broker
	brokers              *Brokers
	topic                string
	correlationID        uint32
	clientID             string
	groupID              string
	sessionTimeout       int
	maxWaitTime          int32
	maxBytes             int32
	minBytes             int32
	fromBeginning        bool
	autoCommit           bool
	autoCommitIntervalMs int
	offsetsStorage       int // 0 zk, 1 kafka

	coordinator          *Broker
	generationID         int32
	memberID             string
	members              []*Member
	ifLeader             bool
	joined               bool
	partitionAssignments []*PartitionAssignment
	topicMetadatas       []*TopicMetadata
	simpleConsumers      []*SimpleConsumer

	messages chan *FullMessage

	mutex              sync.Locker
	assignmentStrategy AssignmentStrategy
}

//func NewGroupConsumer(brokerList, topic, clientID, groupID string, sessionTimeout int, maxWaitTime int32, minBytes int32, maxBytes int32, connectTimeout, timeout int) (*GroupConsumer, error) {
func NewGroupConsumer(config map[string]interface{}) (*GroupConsumer, error) {
	var (
		topic                string
		groupID              string
		clientID             string
		sessionTimeout       int
		maxWaitTime          int32
		minBytes             int32
		maxBytes             int32
		connectTimeout       int
		timeout              int
		autoCommitIntervalMs int
		autoCommit           bool
		offsetsStorage       int
	)

	topic = config["topic"].(string)
	groupID = config["group.id"].(string)
	if v, ok := config["client.id"]; ok {
		clientID = v.(string)
	} else {
		clientID = groupID
		ts := strconv.Itoa(int(time.Now().UnixNano() / 1000000))
		hostname, err := os.Hostname()
		if err != nil {
			glog.Infof("could not get hostname for clientID:%s", err)
			clientID = fmt.Sprintf("%s-%s", clientID, ts)
		} else {
			clientID = fmt.Sprintf("%s-%s-%s", clientID, ts, hostname)
		}
	}
	if v, ok := config["session.timeout.ms"]; ok {
		sessionTimeout = v.(int)
	} else {
		sessionTimeout = 30000
	}
	if v, ok := config["fetch.max.wait.ms"]; ok {
		maxWaitTime = int32(v.(int))
	} else {
		maxWaitTime = 10000
	}
	if v, ok := config["fetch.min.bytes"]; ok {
		minBytes = int32(v.(int))
	} else {
		minBytes = 1
	}
	if v, ok := config["max.partition.fetch.bytes"]; ok {
		maxBytes = int32(v.(int))
	} else {
		maxBytes = 10 * 1024 * 1024
	}
	if v, ok := config["connectTimeout"]; ok {
		connectTimeout = v.(int)
	} else {
		connectTimeout = 30
	}
	if v, ok := config["timeout"]; ok {
		timeout = v.(int)
	} else {
		timeout = 30
	}
	if timeout >= sessionTimeout {
		glog.Fatal("timeout must < sessionTimeout")
	}

	if v, ok := config["auto.commit.interval.ms"]; ok {
		autoCommitIntervalMs = v.(int)
	} else {
		autoCommitIntervalMs = 60000
	}
	if v, ok := config["auto.commit.enable"]; ok {
		autoCommit = v.(bool)
	} else {
		autoCommit = true
	}

	if v, ok := config["offsets.storage"]; ok {
		s := v.(string)
		if s == "kafka" {
			offsetsStorage = 1
		} else if s == "zookeeper" {
			offsetsStorage = 0
		} else {
			glog.Fatalf("offsets.storage must be kafka|zookeeper. `%s` is unknown", s)
		}
	} else {
		offsetsStorage = 1
	}

	brokers, err := NewBrokers(config["bootstrap.servers"].(string), clientID, connectTimeout, timeout)
	if err != nil {
		return nil, err
	}

	c := &GroupConsumer{
		brokers:              brokers,
		topic:                topic,
		correlationID:        0,
		clientID:             clientID,
		groupID:              groupID,
		sessionTimeout:       sessionTimeout,
		maxWaitTime:          maxWaitTime,
		minBytes:             minBytes,
		maxBytes:             maxBytes,
		autoCommit:           autoCommit,
		autoCommitIntervalMs: autoCommitIntervalMs,
		offsetsStorage:       offsetsStorage,

		mutex:              &sync.Mutex{},
		assignmentStrategy: &RangeAssignmentStrategy{},

		joined: false,
	}

	return c, nil
}

// request metadata and set partition metadat to group-consumer. only leader should request this
func (c *GroupConsumer) getTopicPartitionInfo() error {
	metaDataResponse, err := c.brokers.RequestMetaData(c.clientID, &c.topic)
	if err != nil {
		return err
	}

	b, _ := json.Marshal(metaDataResponse)
	glog.V(5).Infof("topic[%s] metadata:%s", c.topic, b)
	c.topicMetadatas = metaDataResponse.TopicMetadatas
	glog.Infof("there is %d partitions in topic[%s]", len(c.topicMetadatas[0].PartitionMetadatas), c.topic)
	return nil
}

func (c *GroupConsumer) getCoordinator() error {
	coordinatorResponse, err := c.brokers.FindCoordinator(c.clientID, c.groupID)
	if err != nil {
		return err
	}

	coordinatorBroker, err := c.brokers.NewBroker(coordinatorResponse.Coordinator.NodeID)
	if err != nil {
		return err
	}
	glog.Infof("coordinator for group[%s]:%s", c.groupID, coordinatorBroker.address)
	c.coordinator = coordinatorBroker

	return nil
}

func (c *GroupConsumer) parseGroupAssignments(memberAssignmentPayload []byte) error {
	memberAssignment, err := NewMemberAssignment(memberAssignmentPayload)
	if err != nil {
		return err
	}
	c.partitionAssignments = memberAssignment.PartitionAssignments
	c.simpleConsumers = make([]*SimpleConsumer, 0)

	for _, partitionAssignment := range c.partitionAssignments {
		for _, partitionID := range partitionAssignment.Partitions {
			simpleConsumer := &SimpleConsumer{}
			simpleConsumer.ClientID = c.clientID
			simpleConsumer.Brokers = c.brokers
			simpleConsumer.TopicName = partitionAssignment.Topic
			simpleConsumer.Partition = partitionID
			simpleConsumer.MaxWaitTime = c.maxWaitTime
			simpleConsumer.MaxBytes = c.maxBytes
			simpleConsumer.MinBytes = c.minBytes
			simpleConsumer.AutoCommit = c.autoCommit
			simpleConsumer.AutoCommitIntervalMs = c.autoCommitIntervalMs
			simpleConsumer.OffsetsStorage = c.offsetsStorage

			simpleConsumer.BelongTO = c

			c.simpleConsumers = append(c.simpleConsumers, simpleConsumer)
		}
	}

	return nil
}

// join && set generationID&memberID
func (c *GroupConsumer) join() (*JoinGroupResponse, error) {
	glog.Info("try to join group")
	c.memberID = ""
	var (
		protocolType string = "consumer"
		memberID     string = ""
	)

	protocolMetadata := &ProtocolMetadata{
		Version:      0,
		Subscription: []string{c.topic},
		UserData:     nil,
	}

	gps := []*GroupProtocol{&GroupProtocol{"range", protocolMetadata.Encode()}}
	joinGroupResponse, err := c.coordinator.requestJoinGroup(
		c.clientID, c.groupID, int32(c.sessionTimeout), memberID, protocolType,
		gps)

	if err != nil {
		return nil, err
	}

	c.generationID = joinGroupResponse.GenerationID
	c.memberID = joinGroupResponse.MemberID
	glog.Infof("memberID now is %s", c.memberID)

	if joinGroupResponse.LeaderID == c.memberID {
		c.ifLeader = true
		c.members = joinGroupResponse.Members
	} else {
		c.ifLeader = false
	}
	return joinGroupResponse, nil
}

func (c *GroupConsumer) sync() (*SyncGroupResponse, error) {
	glog.Info("try to sync group")
	var groupAssignment GroupAssignment
	if c.ifLeader {
		c.getTopicPartitionInfo()
		groupAssignment = c.assignmentStrategy.Assign(c.members, c.topicMetadatas)
	} else {
		groupAssignment = nil
	}
	glog.V(5).Infof("group assignment:%v", groupAssignment)

	syncGroupResponse, err := c.coordinator.requestSyncGroup(
		c.clientID, c.groupID, c.generationID, c.memberID, groupAssignment)

	if err != nil {
		return nil, err
	}

	c.parseGroupAssignments(syncGroupResponse.MemberAssignment)

	return syncGroupResponse, nil
}

func (c *GroupConsumer) joinAndSync() error {
	for {
		joinRes, err := c.join()
		if err != nil {
			glog.Infof("join error:%s", err)
			if err == AllError[16] {
				return err
			}
			time.Sleep(time.Second * 1)
			continue
		} else {
			if glog.V(2) {
				b, _ := json.Marshal(joinRes)
				glog.Infof("join response:%s", b)
			}
		}

		for i := 0; i < 3; i++ {
			syncRes, err := c.sync()
			if glog.V(2) {
				b, _ := json.Marshal(syncRes)
				glog.Infof("sync response:%s", b)
			}
			if err != nil {
				glog.Infof("sync error:%s", err)
				if err == AllError[27] {
					break // rejoin group
				} else {
					continue
				}
			} else {
				return nil
			}
		}
	}
}

func (c *GroupConsumer) heartbeat() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.joined == false {
		return nil
	}

	glog.V(10).Infof("heartbeat generationID:%d memberID:%s", c.generationID, c.memberID)
	_, err := c.coordinator.requestHeartbeat(c.clientID, c.groupID, c.generationID, c.memberID)
	return err
}

func (c *GroupConsumer) CommitOffset(topic string, partitionID int32, offset int64) {
	var apiVersion uint16
	if c.offsetsStorage == 1 {
		apiVersion = 2
	} else {
		apiVersion = 0
	}
	offsetComimtReq := NewOffsetCommitRequest(apiVersion, c.clientID, c.groupID)
	offsetComimtReq.SetMemberID(c.memberID)
	offsetComimtReq.SetGenerationID(c.generationID)
	offsetComimtReq.SetRetentionTime(-1)
	offsetComimtReq.AddPartiton(topic, partitionID, offset, "")

	payload, err := c.coordinator.Request(offsetComimtReq)
	if err == nil {
		_, err := NewOffsetCommitResponse(payload)
		if err == nil {
			glog.V(5).Infof("commit offset %s(%d) [%s][%d]:%d", c.memberID, c.generationID, topic, partitionID, offset)
		} else {
			glog.Errorf("commit offset %s(%d) [%s][%d]:%d error:%s", c.memberID, c.generationID, topic, partitionID, offset, err)
		}
	} else {
		glog.Errorf("commit offset %s(%d) [%s][%d]:%d error:%s", c.memberID, c.generationID, topic, partitionID, offset, err)
	}
}

func (c *GroupConsumer) stop() {
	if c.simpleConsumers != nil {
		for _, simpleConsumer := range c.simpleConsumers {
			simpleConsumer.Stop()
		}
	}
}

func (c *GroupConsumer) leave() {
	glog.Infof("try to leave %s", c.memberID)
	leaveReq := NewLeaveGroupRequest(c.clientID, c.groupID, c.memberID)
	payload, err := c.coordinator.Request(leaveReq)
	if err != nil {
		glog.Errorf("member %s could not leave group:%s", c.memberID, err)
		return
	}

	_, err = NewLeaveGroupResponse(payload)
	if err != nil {
		glog.Errorf("member %s could not leave group:%s", c.memberID, err)
	}

	c.memberID = ""
}

func (c *GroupConsumer) Close() {
	c.stop()
	c.leave()
}

func (c *GroupConsumer) Consume(fromBeginning bool, messages chan *FullMessage) (chan *FullMessage, error) {
	c.fromBeginning = fromBeginning

	if messages == nil {
		messages = make(chan *FullMessage, 10)
	}
	c.messages = messages

	// go heartbeat
	ticker := time.NewTicker(time.Millisecond * time.Duration(c.sessionTimeout) / 10)
	go func() {
		for range ticker.C {
			err := c.heartbeat()
			if err != nil {
				glog.Errorf("failed to send heartbeat:%s", err)
				if err != nil {
					c.stop()
					c.consumeWithoutHeartBeat(c.fromBeginning, c.messages)
				}
			}
		}
	}()

	return c.consumeWithoutHeartBeat(c.fromBeginning, c.messages)
}

func (c *GroupConsumer) consumeWithoutHeartBeat(fromBeginning bool, messages chan *FullMessage) (chan *FullMessage, error) {
	c.joined = false

	var err error
	for {
		err = c.getCoordinator()
		if err != nil {
			glog.Errorf("could not find coordinator:%s", err)
			continue
		}

		err = c.joinAndSync()
		if err == nil {
			break
		}
	}

	c.joined = true

	// consume
	for _, simpleConsumer := range c.simpleConsumers {
		var offset int64
		if fromBeginning {
			offset = -2
		} else {
			offset = -1
		}
		simpleConsumer.Consume(offset, messages)
	}

	return messages, nil
}
